package txn

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
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

// Relay delivers outbox rows to the ERP. Every worker takes one row per
// transaction (FOR UPDATE SKIP LOCKED), asks the adapter whether the effect is
// already there, applies it if not, and records the result in the same
// transaction. Whatever dies in between, the row is taken again, and the
// idempotent receiver (external_id = hex of the key) keeps the effect single.
type Relay struct {
	Store   *store.Store
	Adapter adapters.Adapter
	Workers int     // parallel workers; at least 1
	Scale   float64 // pauses are divided by it (EDIGATE_TIME_SCALE); 0 means 1
	Log     *slog.Logger
	Chaos   *Chaos // nil: no fault injection

	// AfterApply is for tests: it runs right after a successful Apply, before the
	// row is updated. An error from it leaves the row untouched, like a crash there.
	AfterApply func() error
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
	claim, err := r.Store.ClaimOutbox(ctx)
	if err != nil || claim == nil {
		return false, err
	}
	defer claim.Release(ctx)
	r.Chaos.maybeExit(r.Chaos.exitBeforeSend())

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
