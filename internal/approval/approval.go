// Package approval holds the parts of the approval requests (flow P7) that are not
// SQL: what the operator is shown, how long a request lives, and the loop that
// closes requests whose time is over. The queries and the decisions themselves
// are in internal/store (Reserve opens a request, DecideApproval decides it) and the
// HTTP routes in internal/ops.
//
// What the operator sees (control M7) is typed values and the names of the rules,
// never the text of a note. The caller builds the arguments from the stored typed
// body of the document; a value that failed its format is there as a note
// reference ($note_N), which says nothing.
package approval

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/SaintsFlow/edigate/internal/store"
)

// minTTL is the shortest life of a request, however large the time scale is.
const minTTL = 30 * time.Second

// sweepEvery is how often the loop looks for requests that are over.
const sweepEvery = time.Second

// TTL is the life of a request: 30 minutes divided by the time scale
// (EDIGATE_TIME_SCALE), but not less than 30 seconds.
func TTL(scale float64) time.Duration {
	d := store.DefaultApprovalTTL
	if scale > 0 {
		d = time.Duration(float64(d) / scale)
	}
	return max(d, minTTL)
}

// View is the JSON the operator reads: the tool, its tier, the names of the rules
// that made it a request, and the typed arguments of the effect.
func View(tool string, tier int, rules []string, args any) ([]byte, error) {
	if rules == nil {
		rules = []string{}
	}
	return json.Marshal(struct {
		Tool  string   `json:"tool"`
		Tier  int      `json:"tier"`
		Rules []string `json:"rules"`
		Args  any      `json:"args"`
	}{tool, tier, rules, args})
}

// Run closes expired requests until ctx ends.
func Run(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := st.ExpireApprovals(ctx)
			if err != nil && ctx.Err() == nil {
				log.Error("expire approvals failed", "error", err.Error())
			}
			if n > 0 {
				log.Info("approvals expired", "count", n)
			}
		}
	}
}
