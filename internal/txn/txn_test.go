package txn_test

// Tests of the guarantee "at most one effect per business key" (wave 2-5), named
// after the registry in docs/threat-model.md: T-IDEM-01..07 and T-OUTBOX-01..03.
// They go through the whole gateway: a partner posts an 850, an agent calls the
// tool over MCP, the relay delivers to mock-1c.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/txn"
)

var oneLine = []orderLine{{"SKU-1", "10", "0.01"}}

// T-IDEM-01: the same call twice in a row and ten times at once: one key, one
// outbox row, one effect in mock-1c, one txn_id for everybody.
func TestT_IDEM_01_SameCallGivesSameTxnAndOneEffect(t *testing.T) {
	r := newRig(t)
	partner, ptoken := r.partner("ta")
	_, atoken := r.token("ta", "agent", "")
	cs := r.session(atoken)
	doc := r.send850("ta", partner, ptoken, nextISA13(), "1", "PO-100", oneLine...)

	first, wire := r.order(cs, doc)
	if wire != nil || first.Status != "accepted" || first.TxnID == "" {
		t.Fatalf("first call: %+v %v", first, wire)
	}
	second, wire := r.order(cs, doc)
	if wire != nil || second != first {
		t.Fatalf("second call: %+v %v, want %+v", second, wire, first)
	}

	var wg sync.WaitGroup
	got := make([]answer, 10)
	errs := make([]string, 10)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, w := r.order(cs, doc)
			got[i] = a
			if w != nil {
				errs[i] = w.Message
			}
		}()
	}
	wg.Wait()
	for i := range got {
		if errs[i] != "" || got[i] != first {
			t.Errorf("parallel call %d: %+v %q, want %+v", i, got[i], errs[i], first)
		}
	}

	if n := r.count(`SELECT count(*) FROM idempotency_keys`); n != 1 {
		t.Errorf("keys = %d, want 1", n)
	}
	if n := r.count(`SELECT count(*) FROM outbox`); n != 1 {
		t.Errorf("outbox rows = %d, want 1", n)
	}
	r.drain()
	if n := r.orders("ta"); n != 1 {
		t.Errorf("orders in mock-1c = %d, want 1", n)
	}
	if s := r.text(`SELECT status FROM edi_documents WHERE id = $1::uuid`, doc); s != "processed" {
		t.Errorf("document status = %s, want processed", s)
	}
}

// Prices, unit and total come from the price list; the answer holds txn_id and
// status only (no key, no business reference, no price of the 850).
func TestOrderPayloadAndAnswerFields(t *testing.T) {
	r := newRig(t)
	partner, ptoken := r.partner("ta")
	_, atoken := r.token("ta", "agent", "")
	cs := r.session(atoken)
	doc := r.send850("ta", partner, ptoken, nextISA13(), "1", "PO-Case-007", orderLine{"SKU-1", "10", "0.01"}, orderLine{"SKU-2", "3", "0.01"})

	raw, wire := r.call(cs, "erp.create_sales_order", map[string]any{"source_document_id": doc})
	if wire != nil {
		t.Fatal(wire)
	}
	var fields map[string]any
	r.must(json.Unmarshal(raw, &fields))
	if len(fields) != 2 || fields["status"] != "accepted" || fields["txn_id"] == nil {
		t.Errorf("answer = %s, want only status and txn_id", raw)
	}
	for _, leak := range []string{"PO-Case-007", "bk_ref", r.keyHex(fields["txn_id"].(string))} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the answer leaks %q: %s", leak, raw)
		}
	}

	r.drain()
	var total, price, lines string
	r.must(r.pool.QueryRow(context.Background(),
		`SELECT total::text, lines->0->>'unit_price', lines::text FROM mock_onec.sales_orders WHERE business_ref = 'PO-Case-007'`).Scan(&total, &price, &lines))
	if total != "21.00" || price != "1.50" { // 10 x 1.50 + 3 x 2.00; the 850 said 0.01
		t.Errorf("total = %s, first price = %s (lines %s)", total, price, lines)
	}
	if ship := r.text(`SELECT ship_to FROM mock_onec.sales_orders WHERE business_ref = 'PO-Case-007'`); ship != "DC01" {
		t.Errorf("ship_to = %q", ship)
	}
}

func TestOrderRefusals(t *testing.T) {
	r := newRig(t)
	partner, ptoken := r.partner("ta")
	_, atoken := r.token("ta", "agent", "")
	cs := r.session(atoken)

	// A SKU that is not in the price list, and one that is a note reference
	// (a space fails the SKU format), both give policy_denied with unknown_sku.
	for name, sku := range map[string]string{"unknown": "NO-SUCH", "note reference": "BAD SKU"} {
		doc := r.send850("ta", partner, ptoken, nextISA13(), "1", "PO-"+strings.ReplaceAll(name, " ", ""), orderLine{sku, "1", "1"})
		time.Sleep(600 * time.Millisecond) // the bucket of the partner
		_, wire := r.order(cs, doc)
		if reasonOf(wire) != "policy_denied" {
			t.Fatalf("%s: want policy_denied, got %v", name, wire)
		}
		var d struct{ Explain []string }
		r.must(json.Unmarshal(wire.Data, &d))
		if len(d.Explain) != 1 || d.Explain[0] != "unknown_sku" {
			t.Errorf("%s: explain = %v", name, d.Explain)
		}
	}
	if n := r.count(`SELECT count(*) FROM idempotency_keys`) + r.count(`SELECT count(*) FROM outbox`); n != 0 {
		t.Errorf("a refused order left %d rows", n)
	}

	// Unknown and foreign documents give the same not_found.
	_, wire := r.order(cs, "11111111-1111-4111-8111-111111111111")
	if reasonOf(wire) != "not_found" {
		t.Errorf("unknown document: %v", wire)
	}
	_, otherToken := r.partner("tb")
	otherDoc := r.send850("tb", "ZZ:ZZTESTBUYER", otherToken, nextISA13(), "1", "PO-B", oneLine...)
	if _, wire := r.order(cs, otherDoc); reasonOf(wire) != "not_found" {
		t.Errorf("document of another tenant: %v", wire)
	}
}

// T-IDEM-02: the same order in two exchanges (other ISA13 and GS06) is the same
// key and the same request: the same txn_id, one order, no conflict.
func TestT_IDEM_02_SameOrderInANewExchangeIsTheSameKey(t *testing.T) {
	r := newRig(t)
	partner, ptoken := r.partner("ta")
	_, atoken := r.token("ta", "agent", "")
	cs := r.session(atoken)
	doc1 := r.send850("ta", partner, ptoken, nextISA13(), "1", "PO-200", oneLine...)
	doc2 := r.send850("ta", partner, ptoken, nextISA13(), "7", "PO-200", oneLine...)
	if doc1 == doc2 {
		t.Fatal("the two exchanges must be two documents")
	}

	a, wire := r.order(cs, doc1)
	if wire != nil {
		t.Fatal(wire)
	}
	b, wire := r.order(cs, doc2)
	if wire != nil || b != a {
		t.Fatalf("second exchange: %+v %v, want %+v", b, wire, a)
	}
	if n := r.count(`SELECT count(*) FROM audit_log WHERE event_type = 'conflicting_duplicate'`); n != 0 {
		t.Errorf("conflicting_duplicate rows = %d, want 0", n)
	}
	r.drain()
	if n := r.orders("ta"); n != 1 {
		t.Errorf("orders = %d, want 1", n)
	}
	if s := r.text(`SELECT status FROM edi_documents WHERE id = $1::uuid`, doc2); s != "processed" {
		t.Errorf("second document status = %s", s)
	}
}

// T-IDEM-03: the same BEG03 with other lines, and the same document with another
// delivery instruction, are conflicting duplicates: no effect, an audit row with both hashes.
func TestT_IDEM_03_SameKeyOtherRequestIsAConflict(t *testing.T) {
	r := newRig(t)
	partner, ptoken := r.partner("ta")
	_, atoken := r.token("ta", "agent", "")
	cs := r.session(atoken)
	doc1 := r.send850("ta", partner, ptoken, nextISA13(), "1", "PO-300", oneLine...)
	doc2 := r.send850("ta", partner, ptoken, nextISA13(), "2", "PO-300", orderLine{"SKU-1", "99", "0.01"})
	first, wire := r.order(cs, doc1, "none")
	if wire != nil {
		t.Fatal(wire)
	}

	if _, wire := r.order(cs, doc2, "none"); reasonOf(wire) != "conflicting_duplicate" || wire.Code != -32013 {
		t.Fatalf("other lines: %v", wire)
	}
	if _, wire := r.order(cs, doc1, "after_hours"); reasonOf(wire) != "conflicting_duplicate" {
		t.Fatalf("other delivery instruction: %v", wire)
	}
	if again, wire := r.order(cs, doc1, "none"); wire != nil || again != first {
		t.Fatalf("the original request must still answer the same: %+v %v", again, wire)
	}

	if n := r.count(`SELECT count(*) FROM outbox`); n != 1 {
		t.Errorf("outbox rows = %d, want 1", n)
	}
	rows, err := r.pool.Query(context.Background(),
		`SELECT detail->>'stored_req_hash', detail->>'new_req_hash' FROM audit_log WHERE event_type = 'conflicting_duplicate' ORDER BY seq`)
	r.must(err)
	defer rows.Close()
	n := 0
	for rows.Next() {
		var stored, fresh string
		r.must(rows.Scan(&stored, &fresh))
		if len(stored) != 64 || len(fresh) != 64 || stored == fresh {
			t.Errorf("hashes in the audit row: %q %q", stored, fresh)
		}
		n++
	}
	if n != 2 {
		t.Errorf("conflict audit rows = %d, want 2", n)
	}
	r.drain()
	if n := r.orders("ta"); n != 1 {
		t.Errorf("orders = %d, want 1", n)
	}
}

// T-IDEM-04: the same business key in two tenants is two keys and two orders.
func TestT_IDEM_04_TheKeyIsPerTenant(t *testing.T) {
	r := newRig(t)
	var txns []string
	for _, tenant := range []string{"ta", "tb"} {
		partner, ptoken := r.partner(tenant)
		_, atoken := r.token(tenant, "agent", "")
		doc := r.send850(tenant, partner, ptoken, nextISA13(), "1", "PO-400", oneLine...)
		a, wire := r.order(r.session(atoken), doc)
		if wire != nil {
			t.Fatal(wire)
		}
		txns = append(txns, a.TxnID)
	}
	if txns[0] == txns[1] {
		t.Error("two tenants share a txn_id")
	}
	if n := r.count(`SELECT count(DISTINCT key) FROM idempotency_keys`); n != 2 {
		t.Errorf("keys = %d, want 2", n)
	}
	r.drain()
	if r.orders("ta") != 1 || r.orders("tb") != 1 {
		t.Errorf("orders: ta %d, tb %d, want 1 each", r.orders("ta"), r.orders("tb"))
	}
}

func (r *rig) reopen(token, keyHex string) (int, string) {
	r.t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.url+"/ops/txn/"+keyHex+"/reopen", nil)
	r.must(err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	r.must(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	r.must(err)
	return resp.StatusCode, string(body)
}

// T-IDEM-05: a permanent refusal of the ERP fails the key; a repeat gets the same
// outcome; only an operator of the tenant reopens it, with an audit row; the next
// call is a new attempt with a new txn_id.
func TestT_IDEM_05_FailedKeyAndReopen(t *testing.T) {
	r := newRig(t)
	r.mock.SetRejectRefPrefix("REJ")
	partner, ptoken := r.partner("ta")
	_, atoken := r.token("ta", "agent", "")
	cs := r.session(atoken)
	doc := r.send850("ta", partner, ptoken, nextISA13(), "1", "REJ-500", oneLine...)

	first, wire := r.order(cs, doc)
	if wire != nil || first.Status != "accepted" {
		t.Fatalf("first call: %+v %v", first, wire)
	}
	r.drain()
	if s := r.text(`SELECT status FROM outbox`); s != "failed" {
		t.Fatalf("outbox status = %s, want failed", s)
	}
	if s := r.text(`SELECT state FROM idempotency_keys`); s != "failed" {
		t.Fatalf("key state = %s, want failed", s)
	}
	if n := r.orders("ta"); n != 0 {
		t.Fatalf("orders = %d, want 0", n)
	}

	again, wire := r.order(cs, doc)
	if wire != nil || again.Status != "rejected" || again.Reason != "adapter_rejected" || again.TxnID != first.TxnID {
		t.Fatalf("repeat of a failed key: %+v %v", again, wire)
	}
	raw, wire := r.call(cs, "txn.status", map[string]any{"txn_id": first.TxnID})
	if wire != nil || !strings.Contains(string(raw), `"status":"rejected"`) {
		t.Fatalf("txn.status = %s %v", raw, wire)
	}

	key := r.keyHex(first.TxnID)
	opID, opToken := r.token("ta", "operator", "")
	_, otherOpToken := r.token("tb", "operator", "")
	if code, _ := r.reopen("", key); code != http.StatusUnauthorized {
		t.Errorf("no token: HTTP %d", code)
	}
	if code, body := r.reopen(atoken, key); code != http.StatusForbidden || !strings.Contains(body, `"reason":"forbidden"`) {
		t.Errorf("agent token: HTTP %d %s", code, body)
	}
	if code, body := r.reopen(otherOpToken, key); code != http.StatusNotFound || !strings.Contains(body, `"reason":"not_found"`) {
		t.Errorf("operator of another tenant: HTTP %d %s", code, body)
	}
	if code, body := r.reopen(opToken, "zz"); code != http.StatusBadRequest || !strings.Contains(body, `"reason":"schema_invalid"`) {
		t.Errorf("bad key: HTTP %d %s", code, body)
	}
	if s := r.text(`SELECT state FROM idempotency_keys`); s != "failed" {
		t.Fatalf("refused reopen changed the key: %s", s)
	}
	if code, body := r.reopen(opToken, key); code != http.StatusOK || !strings.Contains(body, `"state":"reopened"`) {
		t.Fatalf("operator: HTTP %d %s", code, body)
	}
	if p := r.text(`SELECT principal_id FROM audit_log WHERE event_type = 'txn_reopen'`); p != opID {
		t.Errorf("audit principal = %s, want %s", p, opID)
	}
	if code, body := r.reopen(opToken, key); code != http.StatusConflict || !strings.Contains(body, "key is not failed") {
		t.Errorf("reopen of a key that is not failed: HTTP %d %s", code, body)
	}

	r.mock.SetRejectRefPrefix("")
	next, wire := r.order(cs, doc)
	if wire != nil || next.Status != "accepted" || next.TxnID == first.TxnID {
		t.Fatalf("after reopen: %+v %v, want a new txn_id", next, wire)
	}
	if n := r.count(`SELECT count(*) FROM idempotency_keys`); n != 1 {
		t.Errorf("keys = %d, want the one reused row", n)
	}
	r.drain()
	if n := r.orders("ta"); n != 1 {
		t.Errorf("orders = %d, want 1", n)
	}
	if s := r.text(`SELECT state FROM idempotency_keys`); s != "completed" {
		t.Errorf("key state = %s, want completed", s)
	}
}

// T-IDEM-06: the same order under BEG03 PO-001 and PO001 is two keys and two
// orders: a documented limit (the business reference must be stable).
func TestT_IDEM_06_OtherBEG03IsOtherOrder(t *testing.T) {
	r := newRig(t)
	partner, ptoken := r.partner("ta")
	_, atoken := r.token("ta", "agent", "")
	cs := r.session(atoken)
	d1 := r.send850("ta", partner, ptoken, nextISA13(), "1", "PO-001", oneLine...)
	d2 := r.send850("ta", partner, ptoken, nextISA13(), "2", "PO001", oneLine...)
	a, w1 := r.order(cs, d1)
	b, w2 := r.order(cs, d2)
	if w1 != nil || w2 != nil || a.TxnID == b.TxnID {
		t.Fatalf("answers: %+v %v / %+v %v", a, w1, b, w2)
	}
	r.drain()
	if n := r.orders("ta"); n != 2 {
		t.Errorf("orders = %d, want 2", n)
	}
}

// T-IDEM-07: txn.status of a txn_id of another tenant gives the same not_found as
// a random uuid; for its own txn_id it follows the order to completed.
func TestT_IDEM_07_TxnStatusAndOtherTenants(t *testing.T) {
	r := newRig(t)
	partner, ptoken := r.partner("ta")
	_, atoken := r.token("ta", "agent", "")
	_, btoken := r.token("tb", "agent", "")
	ca, cb := r.session(atoken), r.session(btoken)
	doc := r.send850("ta", partner, ptoken, nextISA13(), "1", "PO-700", oneLine...)
	a, wire := r.order(ca, doc)
	if wire != nil {
		t.Fatal(wire)
	}

	_, foreign := r.call(cb, "txn.status", map[string]any{"txn_id": a.TxnID})
	_, random := r.call(cb, "txn.status", map[string]any{"txn_id": "22222222-2222-4222-8222-222222222222"})
	if reasonOf(foreign) != "not_found" || foreign.Code != random.Code || foreign.Message != random.Message {
		t.Errorf("foreign %v, random %v: must be the same not_found", foreign, random)
	}

	var st struct {
		TxnID       string  `json:"txn_id"`
		Status      string  `json:"status"`
		ExternalRef *string `json:"external_ref"`
	}
	raw, wire := r.call(ca, "txn.status", map[string]any{"txn_id": a.TxnID})
	r.must(json.Unmarshal(raw, &st))
	if wire != nil || st.Status != "accepted" || st.ExternalRef != nil {
		t.Fatalf("before delivery: %s %v", raw, wire)
	}
	r.drain()
	raw, _ = r.call(ca, "txn.status", map[string]any{"txn_id": a.TxnID})
	st.ExternalRef = nil
	r.must(json.Unmarshal(raw, &st))
	id := r.text(`SELECT id::text FROM mock_onec.sales_orders`)
	if st.Status != "completed" || st.ExternalRef == nil || *st.ExternalRef != id {
		t.Errorf("after delivery: %s, mock order id %s", raw, id)
	}
}

// T-OUTBOX-01: a fault between the insert of the key and the insert of the outbox
// row rolls back both; there is no key and the repeat goes through.
func TestT_OUTBOX_01_FaultBetweenKeyAndOutboxRollsBack(t *testing.T) {
	r := newRig(t)
	partner, ptoken := r.partner("ta")
	doc := r.send850("ta", partner, ptoken, nextISA13(), "1", "PO-800", oneLine...)

	key := txn.Key("ta", "erp.create_sales_order", partner, "PO-800")
	in := store.ReserveInput{
		TenantID: "ta", Tool: "erp.create_sales_order", PrincipalID: "agent-x", TraceID: "trace-x", DocumentID: doc,
		Key: key[:], BusinessKey: []byte(`["` + partner + `","PO-800"]`), ReqHash: make([]byte, 32),
		EffectType: "onec.create_order", Payload: []byte(`{}`),
	}
	boom := errors.New("injected fault")
	in.BeforeOutbox = func() error { return boom }
	if _, err := r.st.Reserve(context.Background(), in); !errors.Is(err, boom) {
		t.Fatalf("Reserve = %v, want the injected fault", err)
	}
	for table, n := range map[string]int{
		"idempotency_keys": r.count(`SELECT count(*) FROM idempotency_keys`),
		"outbox":           r.count(`SELECT count(*) FROM outbox`),
		"audit effect":     r.count(`SELECT count(*) FROM audit_log WHERE event_type = 'effect_reserved'`),
	} {
		if n != 0 {
			t.Errorf("%s has %d rows after the rollback", table, n)
		}
	}
	if s := r.text(`SELECT status FROM edi_documents WHERE id = $1::uuid`, doc); s != "received" {
		t.Errorf("document status = %s, want received", s)
	}

	in.BeforeOutbox = nil
	res, err := r.st.Reserve(context.Background(), in)
	if err != nil || res.Outcome != store.ReservedNew || res.Status != "accepted" {
		t.Fatalf("repeat: %+v %v", res, err)
	}
	if r.count(`SELECT count(*) FROM idempotency_keys`) != 1 || r.count(`SELECT count(*) FROM outbox`) != 1 {
		t.Error("the repeat must leave one key and one outbox row")
	}
}

// T-OUTBOX-02: the call is committed with no relay running; the first relay run
// dies after the ERP took the effect and before the row is updated; the next run
// finds the effect with Exists and does not send it again.
func TestT_OUTBOX_02_RelayRestartDoesNotDoubleTheEffect(t *testing.T) {
	r := newRig(t)
	partner, ptoken := r.partner("ta")
	_, atoken := r.token("ta", "agent", "")
	doc := r.send850("ta", partner, ptoken, nextISA13(), "1", "PO-900", oneLine...)
	if _, wire := r.order(r.session(atoken), doc); wire != nil {
		t.Fatal(wire)
	}
	if s := r.text(`SELECT status FROM outbox`); s != "pending" || r.orders("ta") != 0 {
		t.Fatalf("before the relay: outbox %s, orders %d", s, r.orders("ta"))
	}

	crash := errors.New("crash after apply")
	r.relay.AfterApply = func() error { return crash }
	if _, err := r.relay.Step(context.Background()); !errors.Is(err, crash) {
		t.Fatalf("first run = %v, want the crash", err)
	}
	if s := r.text(`SELECT status FROM outbox`); s != "pending" || r.orders("ta") != 1 {
		t.Fatalf("after the crash: outbox %s, orders %d, want pending and 1", s, r.orders("ta"))
	}

	r.relay.AfterApply = nil
	r.drain()
	if s := r.text(`SELECT status FROM outbox`); s != "acked" {
		t.Errorf("outbox status = %s, want acked", s)
	}
	if r.orders("ta") != 1 {
		t.Errorf("orders = %d, want 1", r.orders("ta"))
	}
	if ref := r.text(`SELECT external_ref FROM outbox`); ref != r.text(`SELECT id::text FROM mock_onec.sales_orders`) {
		t.Errorf("external_ref = %s", ref)
	}
	if n := r.count(`SELECT count(*) FROM audit_log WHERE event_type = 'effect_applied' AND idem_key IS NOT NULL`); n != 1 {
		t.Errorf("effect_applied audit rows = %d, want 1", n)
	}
}

// T-OUTBOX-03: mock-1c commits the order and loses the answer; the relay retries,
// finds the order and acks: one order, the outbox row acked.
func TestT_OUTBOX_03_ERPLosesTheAnswer(t *testing.T) {
	r := newRig(t)
	r.mock.SetFailAfterCommitRate(1)
	partner, ptoken := r.partner("ta")
	_, atoken := r.token("ta", "agent", "")
	doc := r.send850("ta", partner, ptoken, nextISA13(), "1", "PO-950", oneLine...)
	if _, wire := r.order(r.session(atoken), doc); wire != nil {
		t.Fatal(wire)
	}

	if did, err := r.relay.Step(context.Background()); err != nil || !did {
		t.Fatalf("first step: %v %v", did, err)
	}
	if r.orders("ta") != 1 {
		t.Fatalf("orders = %d, want 1 (committed, answer lost)", r.orders("ta"))
	}
	if r.text(`SELECT status FROM outbox`) != "pending" || r.count(`SELECT attempts FROM outbox`) != 1 || r.text(`SELECT last_error FROM outbox`) == "" {
		t.Fatal("the row must stay pending with one attempt and an error")
	}

	r.mock.SetFailAfterCommitRate(0)
	time.Sleep(30 * time.Millisecond) // the shortened pause
	r.drain()
	if s := r.text(`SELECT status FROM outbox`); s != "acked" {
		t.Errorf("outbox status = %s, want acked", s)
	}
	if r.orders("ta") != 1 {
		t.Errorf("orders = %d, want 1", r.orders("ta"))
	}
}

// failing is an adapter that always answers with a temporary fault.
type failing struct{ calls int }

func (f *failing) Apply(context.Context, adapters.Effect) (string, error) {
	f.calls++
	return "", &adapters.StatusError{Code: 503}
}

func (f *failing) Exists(context.Context, string) (bool, string, error) { return false, "", nil }

// A temporary fault is retried eight times, the ninth failure makes the row dead
// and the key failed with reason "dead".
func TestRelayGivesUpAfterEightRetries(t *testing.T) {
	r := newRig(t)
	f := &failing{}
	r.relay.Adapter = f
	partner, ptoken := r.partner("ta")
	_, atoken := r.token("ta", "agent", "")
	cs := r.session(atoken)
	doc := r.send850("ta", partner, ptoken, nextISA13(), "1", "PO-990", oneLine...)
	first, wire := r.order(cs, doc)
	if wire != nil {
		t.Fatal(wire)
	}
	for r.text(`SELECT status FROM outbox`) == "pending" && f.calls < 20 {
		if _, err := r.relay.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	if f.calls != 9 || r.text(`SELECT status FROM outbox`) != "dead" || r.text(`SELECT state FROM idempotency_keys`) != "failed" {
		t.Fatalf("calls %d, outbox %s, key %s", f.calls, r.text(`SELECT status FROM outbox`), r.text(`SELECT state FROM idempotency_keys`))
	}
	again, wire := r.order(cs, doc)
	if wire != nil || again.Status != "rejected" || again.Reason != "dead" || again.TxnID != first.TxnID {
		t.Errorf("repeat: %+v %v", again, wire)
	}
}
