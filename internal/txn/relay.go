package txn

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	mrand "math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/store"
)

// The retry schedule (flow P5, invariant I4): a temporary fault is retried after
// 1 s, then twice as long each time, up to 128 s after the eighth failure. The
// ninth failure ends the row as "dead".
const (
	maxRetries = 8
	baseDelay  = time.Second
	minDelay   = 10 * time.Millisecond
	idlePoll   = 200 * time.Millisecond
)

// The MDN wait of a document sent to a partner (invariant I6): the same transmission is
// repeated after 30 minutes, at most three times, then the document is an incident.
const (
	mdnWait    = 30 * time.Minute
	minMDNWait = 10 * time.Second
	mdnRepeats = 3
)

// Transmitter sends a document to a partner and returns the MDN it answered with.
type Transmitter interface {
	Transmit(ctx context.Context, t adapters.Transmission) (adapters.Receipt, error)
}

// Relay delivers outbox rows to the ERP, the warehouse and the partner. Every worker takes
// one row per transaction (FOR UPDATE SKIP LOCKED), asks the adapter whether the effect is
// already there, applies it if not, and records the result in the same
// transaction. Whatever dies in between, the row is taken again, and the
// idempotent receiver (external_id = hex of the key) keeps the effect single.
// A document for a partner goes to AS2 instead: it is sent as it was stored, again and
// again with the same bytes until an MDN counts.
type Relay struct {
	Store   *store.Store
	Adapter adapters.Adapter
	AS2     Transmitter // nil: rows of documents for partners stay queued
	Only    []string    // if set, the relay delivers rows of these effect types only (the open modes: the 997 of the intake)
	Workers int         // parallel workers; at least 1
	Scale   float64     // pauses are divided by it (EDIGATE_TIME_SCALE); 0 means 1
	Log     *slog.Logger
	Chaos   *Chaos // nil: no fault injection

	// MDNRepeatAfter is the pause before a transmission without an MDN is repeated.
	// Zero means 30 minutes divided by Scale, but not less than 10 seconds.
	MDNRepeatAfter time.Duration

	// AfterApply is for tests: it runs right after a successful Apply, before the
	// row is updated. An error from it leaves the row untouched, like a crash there.
	AfterApply func() error

	// BeforeSend is for tests: it runs before a document is sent to the partner, after its
	// numbers and bytes were fixed. An error from it leaves the row untouched.
	BeforeSend func() error
}

// Run starts the workers and blocks until ctx ends.
func (r *Relay) Run(ctx context.Context) {
	n := max(r.Workers, 1)
	done := make(chan struct{})
	for range n {
		go func() {
			defer func() { done <- struct{}{} }()
			r.work(ctx)
		}()
	}
	for range n {
		<-done
	}
}

func (r *Relay) work(ctx context.Context) {
	for ctx.Err() == nil {
		did, err := r.Step(ctx)
		if err != nil && ctx.Err() == nil {
			r.Log.Error("relay step failed", "error", err.Error())
		}
		if err != nil || !did {
			select {
			case <-ctx.Done():
			case <-time.After(idlePoll):
			}
		}
	}
}

// Step handles one due outbox row. It reports false when there was none. An
// error means the row stays as it was (the transaction is rolled back).
func (r *Relay) Step(ctx context.Context) (bool, error) {
	var claim *store.Claim
	var err error
	switch {
	case len(r.Only) > 0:
		claim, err = r.Store.ClaimOutboxOnly(ctx, r.Only...)
	case r.AS2 == nil:
		claim, err = r.Store.ClaimOutbox(ctx, adapters.EffectSend855, adapters.EffectSend856, adapters.EffectSend997)
	default:
		claim, err = r.Store.ClaimOutbox(ctx)
	}
	if err != nil || claim == nil {
		return false, err
	}
	defer claim.Release(ctx)
	// The fault injection is about the effects of an agent and the documents it causes. The 997 of
	// the intake exists before any agent acts, and a process that dies there would die before the
	// episode of the benchmark has started.
	if claim.Row.EffectType != adapters.EffectSend997 {
		r.Chaos.maybeExit(r.Chaos.exitBeforeSend())
	}
	if adapters.IsSend(claim.Row.EffectType) {
		return true, r.sendDocument(ctx, claim)
	}

	e := adapters.Effect{
		Type: claim.Row.EffectType, TenantID: claim.Row.TenantID,
		ExternalID: hex.EncodeToString(claim.Row.Key), Payload: claim.Row.Payload,
	}
	found, ref, err := r.Adapter.Exists(ctx, e.Type, e.ExternalID)
	if err == nil && !found {
		ref, err = r.Adapter.Apply(ctx, e)
		if err == nil {
			r.Chaos.maybeExit(r.Chaos.exitAfterApply())
			if r.AfterApply != nil {
				if hookErr := r.AfterApply(); hookErr != nil {
					return true, hookErr
				}
			}
		}
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if err == nil {
		return true, claim.Ack(ctx, ref)
	}
	return true, r.failed(ctx, claim, err)
}

// sendDocument makes one transmission of a document to the partner and records it. The
// bytes, the Message-Id and the numbers come from the row, so a repeat is the same
// transmission (I7). An MDN that names our Message-Id and the SHA-256 of our bytes counts and
// completes the row; the document is delivered only when the 997 agrees too. Without an
// MDN the row waits and the transmission is repeated, and after the third repeat the
// document is an incident. A document that is already an incident (a negative 997 came) is
// never sent again.
func (r *Relay) sendDocument(ctx context.Context, claim *store.Claim) error {
	var t adapters.Transmission
	if err := json.Unmarshal(claim.Row.Payload, &t); err != nil {
		return claim.Fail(ctx, "failed", "adapter_rejected", "the transmission is unreadable")
	}
	reason, err := claim.DocIncident(ctx, t.DocID)
	if err != nil {
		return err
	}
	if reason != "" {
		r.Log.Warn("document not sent: it is an incident", "outbox_id", claim.Row.ID, "reason", reason)
		return claim.Fail(ctx, "failed", reason, "the document is an incident")
	}
	if r.BeforeSend != nil {
		if err := r.BeforeSend(); err != nil {
			return err
		}
	}
	receipt, sendErr := r.AS2.Transmit(ctx, t)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	counted := sendErr == nil && receipt.Counts(t)
	mdn, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := claim.RecordTransmission(ctx, t.DocID, counted, mdn); err != nil {
		return err
	}
	if counted {
		return claim.Ack(ctx, t.MessageID)
	}
	cause := "the MDN does not match the transmission"
	if sendErr != nil {
		cause = sendErr.Error()
	}
	if claim.Row.Attempts >= mdnRepeats {
		r.Log.Warn("document gave up: no MDN", "outbox_id", claim.Row.ID)
		return claim.FailSend(ctx, t.DocID, "no_mdn", cause)
	}
	return claim.Retry(ctx, cause, r.mdnPause())
}

// mdnPause is the wait before a transmission without an MDN is repeated.
func (r *Relay) mdnPause() time.Duration {
	if r.MDNRepeatAfter > 0 {
		return r.MDNRepeatAfter
	}
	d := mdnWait
	if r.Scale > 0 {
		d = time.Duration(float64(d) / r.Scale)
	}
	return max(d, minMDNWait)
}

// failed decides what a failed delivery becomes: a permanent refusal of the ERP
// ends the row as "failed"; any other fault is retried until the attempts run out.
func (r *Relay) failed(ctx context.Context, c *store.Claim, cause error) error {
	var se *adapters.StatusError
	switch {
	case errors.As(cause, &se) && se.Permanent():
		r.Log.Warn("effect refused by the ERP", "outbox_id", c.Row.ID, "status", se.Code)
		return c.Fail(ctx, "failed", "adapter_rejected", cause.Error())
	case c.Row.Attempts >= maxRetries:
		r.Log.Warn("effect delivery gave up", "outbox_id", c.Row.ID, "error", cause.Error())
		return c.Fail(ctx, "dead", "dead", cause.Error())
	}
	return c.Retry(ctx, cause.Error(), r.delay(c.Row.Attempts))
}

// delay is the pause after the failure number attempts+1, shortened by the time scale.
func (r *Relay) delay(attempts int) time.Duration {
	d := baseDelay << attempts
	if r.Scale > 0 {
		d = time.Duration(float64(d) / r.Scale)
	}
	return max(d, minDelay)
}

// Chaos injects faults into the relay, for tests of the guarantee only. The
// probabilities are off (zero) by default.
type Chaos struct {
	ExitBeforeSend float64       // chance to exit after a row is chosen, before the ERP is asked
	ExitAfterApply float64       // chance to exit right after the ERP took the effect, before the row is updated
	CrashAfter     time.Duration // if set, the process exits at a random moment 1 to 1.5 times this long after Start
	Rand           func() float64
	Exit           func() // defaults to os.Exit(137)
}

// NewChaos builds the fault injector. With a seed the draws repeat from run to
// run; without one they come from crypto/rand.
func NewChaos(exitBeforeSend, exitAfterApply float64, crashAfter time.Duration, seed *uint64) *Chaos {
	c := &Chaos{ExitBeforeSend: exitBeforeSend, ExitAfterApply: exitAfterApply, CrashAfter: crashAfter, Exit: func() { os.Exit(137) }}
	if seed != nil {
		g := mrand.New(mrand.NewPCG(*seed, *seed^0x9e3779b97f4a7c15))
		var mu sync.Mutex // the workers draw at the same time
		c.Rand = func() float64 {
			mu.Lock()
			defer mu.Unlock()
			return g.Float64()
		}
	} else {
		c.Rand = cryptoFloat
	}
	return c
}

func cryptoFloat() float64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 1 // no draw, no fault
	}
	return float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53)
}

func (c *Chaos) exitBeforeSend() float64 {
	if c == nil {
		return 0
	}
	return c.ExitBeforeSend
}

func (c *Chaos) exitAfterApply() float64 {
	if c == nil {
		return 0
	}
	return c.ExitAfterApply
}

func (c *Chaos) maybeExit(p float64) {
	if c != nil && p > 0 && c.Rand() < p {
		c.Exit()
	}
}

// Start arms the timed crash, if CrashAfter is set.
func (c *Chaos) Start() {
	if c == nil || c.CrashAfter <= 0 {
		return
	}
	jitter, _ := rand.Int(rand.Reader, big.NewInt(int64(c.CrashAfter/2)+1))
	wait := c.CrashAfter + time.Duration(jitter.Int64())
	time.AfterFunc(wait, c.Exit)
}
