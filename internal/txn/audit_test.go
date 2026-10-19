package txn_test

// T-AUD-02 (control AUD-02): after a whole run of the gateway every audit row names who did it
// and under which trace, the rows of an effect carry the idempotency key, and the hash chain of
// the tenant verifies (the JSON fields of the rows survive the way the database keeps them).

import (
	"context"
	"testing"

	"github.com/SaintsFlow/edigate/internal/audit"
)

func TestT_AUD_02_EveryRowHasPrincipalTraceAndEffectsHaveTheKey(t *testing.T) {
	s := newSagaRig(t)
	doc, ref := s.newOrder()
	o := s.createOrder(doc)
	if a := s.confirm(o.TxnID, "AC"); a.Status != "accepted" {
		t.Fatalf("confirm: %+v", a)
	}
	s.settleDoc("855", ref, "delivered")
	s.until("the order saga is done", func() bool { return s.sagaState("order", ref) == "done" })
	s.reserve(o.TxnID)
	if a := s.tool("edi.send_asn", map[string]any{"sales_order_txn_id": o.TxnID}); a.Status != "accepted" {
		t.Fatalf("send_asn: %+v", a)
	}
	s.settleDoc("856", ref, "delivered")
	s.until("the shipment saga is done", func() bool { return s.sagaState("shipment", ref) == "done" })

	total := s.count(`SELECT count(*) FROM audit_log`)
	if total < 10 {
		t.Fatalf("only %d audit rows after a whole run", total)
	}
	if n := s.count(`SELECT count(*) FROM audit_log WHERE principal_id = '' OR trace_id = ''`); n != 0 {
		t.Errorf("%d rows without principal_id or trace_id", n)
	}
	// The events that belong to an effect name its key. The 997 that answers a group is the one
	// effect without a key (it has no business key and no saga).
	if rows := s.text(`SELECT COALESCE(string_agg(event_type || ':' || COALESCE(outcome, ''), ','), '') FROM audit_log WHERE idem_key IS NULL
	                  AND event_type IN ('effect_reserved', 'effect_applied', 'effect_failed', 'outbound_queued', 'outbound_sent')
	                  AND NOT (event_type = 'outbound_queued' AND outcome = '997')`); rows != "" {
		t.Errorf("effect rows without idem_key: %s", rows)
	}
	if s.count(`SELECT count(*) FROM audit_log WHERE event_type = 'effect_applied' AND idem_key IS NOT NULL`) == 0 {
		t.Error("no effect_applied rows to check")
	}
	res, err := s.st.VerifyAudit(context.Background(), audit.Options{Tenant: "ta"})
	if err != nil || res.Failure != "" || res.Rows == 0 {
		t.Errorf("verify: %+v %v", res, err)
	}
}
