// Package saga drives the sagas (invariant I5): it looks at the open sagas again and again
// and does what each one needs next.
//
// Input: the sagas in the database (created by erp.create_sales_order and wms.reserve) and the
// facts about their steps. Output: changes to the sagas, written by store.AdvanceSaga, and
// the 856, which this package asks for when the shipment is done and the warehouse has
// given its facts. The decisions themselves are in internal/store (decide); here are the
// loop, the two outside questions (what the warehouse recorded, whether an effect exists),
// and the time limits.
//
// The runner keeps no state of its own. Any number of processes may run it: a saga that
// another one is working on is skipped.
package saga

import (
	"context"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/tools"
)

const (
	defaultPoll = 500 * time.Millisecond
	ackWait     = 24 * time.Hour
	minAckWait  = 60 * time.Second
)

// Facts gives the facts the warehouse recorded about a shipment.
type Facts interface {
	Shipment(ctx context.Context, externalID string) (adapters.Shipment, error)
}

// Runner advances the sagas.
type Runner struct {
	Store   *store.Store
	Adapter adapters.Adapter // asked whether an effect exists, for a step that failed without an answer
	Facts   Facts
	Policy  tools.Decider // decides the 856 (it is a tool call of the saga itself)
	Scale   float64       // the time limits are divided by it (EDIGATE_TIME_SCALE); 0 means 1
	Log     *slog.Logger

	// AckTimeout is how long a sent document waits for its 997. Zero means 24 hours divided
	// by Scale, but not less than 60 seconds.
	AckTimeout time.Duration
	// Poll is the pause between two passes; zero means half a second.
	Poll time.Duration
}

// Run passes over the open sagas until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	poll := r.Poll
	if poll <= 0 {
		poll = defaultPoll
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if _, err := r.Pass(ctx); err != nil && ctx.Err() == nil {
			r.Log.Error("saga pass failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass advances every open saga once and returns how many changed.
func (r *Runner) Pass(ctx context.Context) (int, error) {
	ids, err := r.Store.OpenSagas(ctx)
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, id := range ids {
		ok, err := r.advance(ctx, id)
		if err != nil {
			r.Log.Error("saga step failed", "saga_id", id, "error", err.Error())
			continue
		}
		if ok {
			changed++
		}
	}
	return changed, nil
}

func (r *Runner) ackTimeout() time.Duration {
	if r.AckTimeout > 0 {
		return r.AckTimeout
	}
	d := ackWait
	if r.Scale > 0 {
		d = time.Duration(float64(d) / r.Scale)
	}
	return max(d, minAckWait)
}

// advance gathers the facts the store cannot find out by itself, then lets it decide.
func (r *Runner) advance(ctx context.Context, id string) (bool, error) {
	sg, err := r.Store.LoadSaga(ctx, id, r.ackTimeout())
	if err != nil {
		return false, err
	}
	hints := store.AdvanceHints{AckTimeout: r.ackTimeout(), Denied: map[int]bool{}, Existence: map[int]string{}}
	if sg.State == "running" && sg.Kind == "shipment" && len(sg.Steps) == 3 && r.Facts != nil {
		ship, notice := sg.Steps[1], sg.Steps[2]
		if ship.KeyState == "completed" && notice.Key == nil {
			if r.sendNotice(ctx, sg, ship) {
				hints.Denied[notice.No] = true
			}
		}
	}
	for _, st := range sg.Steps {
		if st.KeyState == "failed" && st.KeyReason == "dead" && st.CompKey == nil {
			hints.Existence[st.No] = r.exists(ctx, st)
		}
	}
	return r.Store.AdvanceSaga(ctx, id, hints)
}

// sendNotice makes the 856 from the facts of the warehouse. It reports true when the policy
// refused it for good; any other failure is only logged and tried again on the next pass.
func (r *Runner) sendNotice(ctx context.Context, sg store.Saga, ship store.SagaStep) bool {
	facts, err := r.Facts.Shipment(ctx, hex.EncodeToString(ship.Key))
	if err != nil {
		r.Log.Warn("shipment facts not available", "saga_id", sg.ID, "error", err.Error())
		return false
	}
	_, err = tools.SendShipNotice(ctx, r.Store, r.Policy, sg, facts)
	if err == nil {
		return false
	}
	if _, reason := rpcerr.From(err); reason == rpcerr.PolicyDenied {
		r.Log.Warn("the 856 is refused by the policy", "saga_id", sg.ID)
		return true
	}
	r.Log.Error("the 856 could not be queued", "saga_id", sg.ID, "error", err.Error())
	return false
}

// exists asks the system whether the effect of a step that ran out of attempts is there:
// found, not_found or unknown. The compensation is queued either way (it does nothing
// for an effect that is not there); the answer goes to the audit log.
func (r *Runner) exists(ctx context.Context, st store.SagaStep) string {
	found, _, err := r.Adapter.Exists(ctx, st.Name, hex.EncodeToString(st.Key))
	switch {
	case err != nil:
		return "unknown"
	case found:
		return "found"
	}
	return "not_found"
}
