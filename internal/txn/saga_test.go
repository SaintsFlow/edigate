package txn_test

// Tests of wave 2-7, named after the registry in docs/threat-model.md: T-SAGA-01..03 (sagas
// and compensations) and T-ACK-01..05 (MDN, 997, repeats with the same numbers; T-ACK-06 is
// in internal/store). They go through the whole gateway: partner -> /edi/inbound -> agent over
// MCP -> policy -> operator over /ops/ -> relay -> mock-1c, mock-wms and partner-emulator,
// and the 997 of the partner back to /edi/inbound.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// confirm calls erp.confirm_order.
func (s *sagaRig) confirm(orderTxn, ackType string) answer {
	s.t.Helper()
	return s.tool("erp.confirm_order", map[string]any{"sales_order_txn_id": orderTxn, "ack_type": ackType})
}

// reserve runs wms.reserve and waits until the reservation is done.
func (s *sagaRig) reserve(orderTxn string) answer {
	s.t.Helper()
	a := s.tool("wms.reserve", map[string]any{"sales_order_txn_id": orderTxn})
	if a.Status != "accepted" {
		s.t.Fatalf("reserve: %+v", a)
	}
	s.until("the reservation is done", func() bool { return s.txnStatus(s.agent, a.TxnID) == "completed" })
	return a
}

// sagaStateOf is saga_state of txn.status.
func (s *sagaRig) sagaStateOf(txnID string) string {
	s.t.Helper()
	raw, wire := s.call(s.agent, "txn.status", map[string]any{"txn_id": txnID})
	if wire != nil {
		s.t.Fatalf("txn.status: %v", wire)
	}
	var out struct {
		SagaState *string `json:"saga_state"`
	}
	s.must(json.Unmarshal(raw, &out))
	if out.SagaState == nil {
		return ""
	}
	return *out.SagaState
}

// reviewRules lists the rules of the open review requests of the tenant.
func (s *sagaRig) reviewRules() string {
	s.t.Helper()
	return s.text(`SELECT COALESCE(string_agg(rules::text, ',' ORDER BY requested_at), '') FROM approvals
	               WHERE tenant_id = 'ta' AND kind = 'review' AND status = 'pending'`)
}

// confirmed runs the order up to a sent 855 and returns the txn_id of the order and the reference.
func (s *sagaRig) confirmed() (orderTxn, ref string) {
	s.t.Helper()
	doc, ref := s.newOrder()
	o := s.createOrder(doc)
	if a := s.confirm(o.TxnID, "AC"); a.Status != "accepted" {
		s.t.Fatalf("confirm: %+v", a)
	}
	return o.TxnID, ref
}

// The whole of scenario S2: an order of 133.00 goes through the order saga (the order in the
// ERP, the 855) and the shipment saga (reservation, shipment with the SSCC of the warehouse,
// the 856). Neither document needs a request: it is close to the order. Both are delivered
// (MDN and 997), the partner got one of each with different ISA13, and both sagas are done.
func TestSagaOrderAndShipmentGoThrough(t *testing.T) {
	s := newSagaRig(t)
	doc, ref := s.newOrder()
	o := s.createOrder(doc)
	if a := s.confirm(o.TxnID, "AC"); a.Status != "accepted" {
		t.Fatalf("confirm: %+v", a)
	}
	s.settleDoc("855", ref, "delivered")
	s.until("the order saga is done", func() bool { return s.sagaState("order", ref) == "done" })
	if got := s.sagaStateOf(o.TxnID); got != "done" {
		t.Errorf("saga_state of the order = %q", got)
	}

	s.reserve(o.TxnID)
	if a := s.tool("edi.send_asn", map[string]any{"sales_order_txn_id": o.TxnID}); a.Status != "accepted" {
		t.Fatalf("send_asn: %+v", a)
	}
	s.settleDoc("856", ref, "delivered")
	s.until("the shipment saga is done", func() bool { return s.sagaState("shipment", ref) == "done" })

	if n := s.count(`SELECT count(*) FROM approvals`); n != 0 {
		t.Errorf("approvals = %d, want 0: both documents are close to the order", n)
	}
	for _, st01 := range []string{"855", "856"} {
		if tr, ic := s.received(st01, ref); tr != 1 || ic != 1 {
			t.Errorf("%s: %d transmissions, %d interchanges, want 1 and 1", st01, tr, ic)
		}
		if got := s.text(`SELECT d.mdn_ok::text || '|' || d.ack_status FROM outbound_docs d JOIN sagas sg ON sg.id = d.saga_id
		                  WHERE d.st01 = $1 AND sg.business_key->>1 = $2`, st01, ref); got != "true|A" {
			t.Errorf("%s: mdn_ok|ack_status = %s", st01, got)
		}
	}
	if n := s.count(`SELECT count(DISTINCT isa13) FROM mock_partner.received WHERE business_ref = $1`, ref); n != 2 {
		t.Errorf("different ISA13 at the partner = %d, want 2", n)
	}
	// The 856 carries the SSCC the warehouse gave, one pack per line.
	raw := string(s.rawOf("856", ref))
	var sscc []string
	s.must(json.Unmarshal([]byte(s.text(`SELECT to_jsonb(sscc)::text FROM mock_wms.shipments`)), &sscc))
	if len(sscc) != 2 {
		t.Fatalf("sscc = %v", sscc)
	}
	for _, label := range sscc {
		if !strings.Contains(raw, "MAN*GM*"+label+"~") {
			t.Errorf("the 856 lacks the SSCC %s: %s", label, raw)
		}
	}
	if !strings.Contains(raw, "SN1**40*EA~") || !strings.Contains(raw, "SN1**36.5*EA~") {
		t.Errorf("the 856 does not carry the shipped quantities: %s", raw)
	}
}

// edi.send_asn without a done reservation is policy_denied (saga.order); a txn_id of
// another tenant or of a tool that is not an order is not_found.
func TestSendAsnNeedsTheReservation(t *testing.T) {
	s := newSagaRig(t)
	doc, _ := s.newOrder()
	o := s.createOrder(doc)

	_, wire := s.call(s.agent, "edi.send_asn", map[string]any{"sales_order_txn_id": o.TxnID})
	if reasonOf(wire) != "policy_denied" || strings.Join(explainOf(wire), ",") != "saga.order" {
		t.Fatalf("send_asn without a reservation: %v", wire)
	}
	if n := s.count(`SELECT count(*) FROM sagas WHERE kind = 'shipment'`); n != 0 {
		t.Errorf("a refused call started a shipment saga")
	}
	_, wire = s.call(s.agent, "erp.confirm_order", map[string]any{"sales_order_txn_id": "11111111-1111-4111-8111-111111111111", "ack_type": "AC"})
	if reasonOf(wire) != "not_found" {
		t.Errorf("unknown txn_id: %v", wire)
	}
	_, btoken := s.token("tb", "agent", "")
	if _, wire = s.call(s.session(btoken), "wms.reserve", map[string]any{"sales_order_txn_id": o.TxnID}); reasonOf(wire) != "not_found" {
		t.Errorf("txn_id of another tenant: %v", wire)
	}
}

// The 855 does not leave before the order exists in the ERP: while step 1 is not done, the
// row of step 2 is not given to the relay, however due it is.
func TestConfirmWaitsBehindTheOrder(t *testing.T) {
	s := newSagaRig(t)
	doc, ref := s.newOrder()
	o := s.tool("erp.create_sales_order", map[string]any{"source_document_id": doc})
	if a := s.confirm(o.TxnID, "AC"); a.Status != "accepted" {
		t.Fatalf("confirm: %+v", a)
	}
	s.exec(`UPDATE outbox SET next_attempt_at = now() + interval '1 hour' WHERE effect_type = 'onec.create_order'`)
	claim, err := s.st.ClaimOutbox(context.Background())
	s.must(err)
	if claim != nil {
		claim.Release(context.Background())
		t.Fatalf("the relay was given %s while the order is not created", claim.Row.EffectType)
	}
	s.exec(`UPDATE outbox SET next_attempt_at = now() WHERE effect_type = 'onec.create_order'`)
	s.settleDoc("855", ref, "delivered")
	if s.orders("ta") != 1 {
		t.Errorf("orders in mock-1c = %d", s.orders("ta"))
	}
}

// A shipment that differs from the order by more than 25 percent makes the 856 wait for one
// operator (forecast.deviation); with the operator's approval it is sent and delivered.
func TestShipmentFarFromTheOrderWaitsForAnOperator(t *testing.T) {
	s := newSagaRig(t)
	orderTxn, ref := s.confirmed()
	s.settleDoc("855", ref, "delivered")
	s.reserve(orderTxn)
	s.wms.SetShipQtyFactor("1.5") // MOCK_SHIP_QTY_FACTOR: 199.50 against 133.00
	if a := s.tool("edi.send_asn", map[string]any{"sales_order_txn_id": orderTxn}); a.Status != "accepted" {
		t.Fatalf("send_asn: %+v", a)
	}
	s.until("the 856 waits for an operator", func() bool {
		return s.count(`SELECT count(*) FROM approvals WHERE tool = 'edi.send_856' AND status = 'pending'`) == 1
	})
	if got := s.text(`SELECT rules::text FROM approvals WHERE tool = 'edi.send_856'`); got != `["forecast.deviation"]` {
		t.Errorf("rules = %s", got)
	}
	if n := s.count(`SELECT count(*) FROM outbound_docs WHERE st01 = '856'`); n != 0 {
		t.Errorf("a 856 with numbers exists before the approval")
	}
	if tr, _ := s.received("856", ref); tr != 0 {
		t.Fatalf("the partner got the 856 before the approval")
	}
	id := s.text(`SELECT id::text FROM approvals WHERE tool = 'edi.send_856'`)
	if code, body := s.ops(s.op1, http.MethodPost, "/ops/approvals/"+id+"/approve"); code != http.StatusOK {
		t.Fatalf("approve: HTTP %d %s", code, body)
	}
	s.settleDoc("856", ref, "delivered")
	s.until("the shipment saga is done", func() bool { return s.sagaState("shipment", ref) == "done" })
	if !strings.Contains(string(s.rawOf("856", ref)), "SN1**60*EA~") {
		t.Errorf("the 856 does not carry the shipped quantity")
	}
}

// T-SAGA-01: a failure before the send runs the compensations. (a) The request of the 855 is
// rejected: the order that was created is cancelled in mock-1c, and the partner got nothing.
// (b) The warehouse refuses the shipment: the reservation is released and no 856 exists.
func TestT_SAGA_01_FailureBeforeTheSendIsCompensated(t *testing.T) {
	t.Run("rejected request of the 855", func(t *testing.T) {
		s := newSagaRig(t)
		doc, ref := s.newOrder()
		o := s.createOrder(doc)
		a := s.confirm(o.TxnID, "RJ") // a rejection moves the amount by 100 percent: a request
		if a.Status != "pending_approval" {
			t.Fatalf("RJ: %+v", a)
		}
		if code, body := s.ops(s.op1, http.MethodPost, "/ops/approvals/"+a.ApprovalID+"/reject"); code != http.StatusOK {
			t.Fatalf("reject: HTTP %d %s", code, body)
		}
		s.until("the saga is compensated", func() bool { return s.sagaState("order", ref) == "compensated" })
		if got := s.text(`SELECT status FROM mock_onec.sales_orders WHERE business_ref = $1`, ref); got != "cancelled" {
			t.Errorf("order status = %s, want cancelled", got)
		}
		if tr, _ := s.received("855", ref); tr != 0 {
			t.Errorf("the partner got %d transmissions of the 855", tr)
		}
		if n := s.count(`SELECT count(*) FROM outbound_docs`); n != 0 {
			t.Errorf("outbound_docs = %d, want 0", n)
		}
		if got := s.text(`SELECT string_agg(status, ',' ORDER BY step_no) FROM saga_steps st JOIN sagas sg ON sg.id = st.saga_id WHERE sg.kind = 'order'`); got != "compensated,failed" {
			t.Errorf("step statuses = %s", got)
		}
		if got := s.sagaStateOf(o.TxnID); got != "compensated" {
			t.Errorf("saga_state = %q", got)
		}
	})

	t.Run("refused shipment", func(t *testing.T) {
		s := newSagaRig(t)
		doc, ref := s.newOrder()
		o := s.createOrder(doc)
		s.reserve(o.TxnID)
		s.wms.SetRejectRefPrefix("PO-") // MOCK_REJECT_REF_PREFIX for wms.ship
		if a := s.tool("edi.send_asn", map[string]any{"sales_order_txn_id": o.TxnID}); a.Status != "accepted" {
			t.Fatalf("send_asn: %+v", a)
		}
		s.until("the shipment saga is compensated", func() bool { return s.sagaState("shipment", ref) == "compensated" })
		if got := s.text(`SELECT status FROM mock_wms.reservations`); got != "released" {
			t.Errorf("reservation status = %s, want released", got)
		}
		if n := s.count(`SELECT count(*) FROM mock_wms.shipments`); n != 0 {
			t.Errorf("shipments = %d, want 0", n)
		}
		if tr, _ := s.received("856", ref); tr != 0 {
			t.Errorf("the partner got a 856")
		}
		if got := s.text(`SELECT string_agg(status, ',' ORDER BY step_no) FROM saga_steps st JOIN sagas sg ON sg.id = st.saga_id WHERE sg.kind = 'shipment'`); got != "compensated,failed,skipped" {
			t.Errorf("step statuses = %s", got)
		}
	})
}

// T-SAGA-02: a compensation that fails for good (MOCK_FAIL_CANCEL) leaves the saga in incident
// and opens a request for the operators with the rule saga.compensation_failed.
func TestT_SAGA_02_FailedCompensationIsAnIncident(t *testing.T) {
	s := newSagaRig(t)
	doc, ref := s.newOrder()
	o := s.createOrder(doc)
	s.mock.SetFailCancel(true)
	a := s.confirm(o.TxnID, "RJ")
	if code, body := s.ops(s.op1, http.MethodPost, "/ops/approvals/"+a.ApprovalID+"/reject"); code != http.StatusOK {
		t.Fatalf("reject: HTTP %d %s", code, body)
	}
	s.until("the saga is an incident", func() bool { return s.sagaState("order", ref) == "incident" })
	if got := s.reviewRules(); got != `["saga.compensation_failed"]` {
		t.Errorf("review rules = %s", got)
	}
	if got := s.text(`SELECT status FROM mock_onec.sales_orders WHERE business_ref = $1`, ref); got != "active" {
		t.Errorf("order status = %s: the cancel must not have gone through", got)
	}
	var seen bool
	for _, p := range s.pending(s.op1) {
		seen = seen || (p["kind"] == "review" && strings.Contains(string(mustJSON(t, p["rules"])), "saga.compensation_failed"))
	}
	if !seen {
		t.Errorf("the operator does not see the request of the failed compensation")
	}
	if got := s.sagaStateOf(o.TxnID); got != "incident" {
		t.Errorf("saga_state = %q", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// T-SAGA-03: an error after the send is an incident and the document is not sent again
// with new numbers: a negative 997 (AK5 = R), a 997 that never comes, and a missing MDN. After
// the time limits no saga is left running.
func TestT_SAGA_03_FailureAfterTheSendIsAnIncident(t *testing.T) {
	s := newSagaRig(t)
	s.runner.AckTimeout = 200 * time.Millisecond // the 24 hours of the 997, shrunk for the test

	s.knobs(`{"ack_status":"R"}`)
	_, rejected := s.confirmed()
	s.until("the negative 997 makes an incident", func() bool { return s.sagaState("order", rejected) == "incident" })
	if got := s.text(`SELECT incident_reason FROM outbound_docs d JOIN sagas sg ON sg.id = d.saga_id WHERE sg.business_key->>1 = $1`, rejected); got != "ack_rejected" {
		t.Errorf("reason = %s", got)
	}

	s.knobs(`{"ack_status":"none"}`)
	_, silent := s.confirmed()
	s.until("a 997 that does not come makes an incident", func() bool { return s.sagaState("order", silent) == "incident" })
	if got := s.text(`SELECT incident_reason FROM outbound_docs d JOIN sagas sg ON sg.id = d.saga_id WHERE sg.business_key->>1 = $1`, silent); got != "ack_missing" {
		t.Errorf("reason = %s", got)
	}

	s.knobs(`{"ack_status":"A","mdn_drop":1}`)
	_, lost := s.confirmed()
	s.until("a missing MDN makes an incident", func() bool { return s.sagaState("order", lost) == "incident" })

	for _, ref := range []string{rejected, silent, lost} {
		if _, ic := s.received("855", ref); ic != 1 {
			t.Errorf("%s: the partner got %d different ISA13, want 1: a document is never sent again with new numbers", ref, ic)
		}
	}
	if tr, _ := s.received("855", rejected); tr != 1 {
		t.Errorf("after a negative 997 the document was sent %d times, want 1", tr)
	}
	for _, rule := range []string{"ack.rejected", "ack.missing", "mdn.missing"} {
		if !strings.Contains(s.reviewRules(), rule) {
			t.Errorf("no review request with %s in %s", rule, s.reviewRules())
		}
	}
	if n := s.count(`SELECT count(*) FROM sagas WHERE state = 'running'`); n != 0 {
		t.Errorf("%d sagas are still running after the time limits", n)
	}
}

// T-ACK-01: with no MDN at all the transmission is repeated three times with the same
// Message-Id, the same ISA13 and the same bytes, and then the document is an incident.
func TestT_ACK_01_NoMDNRepeatsThreeTimesThenIncident(t *testing.T) {
	s := newSagaRig(t)
	s.knobs(`{"mdn_drop":1}`)
	_, ref := s.confirmed()
	s.settleDoc("855", ref, "incident")

	var n, bodies, ids, isas int
	s.must(s.pool.QueryRow(context.Background(),
		`SELECT count(*), count(DISTINCT body), count(DISTINCT message_id), count(DISTINCT isa13)
		 FROM mock_partner.received WHERE business_ref = $1`, ref).Scan(&n, &bodies, &ids, &isas))
	if n != 4 || bodies != 1 || ids != 1 || isas != 1 {
		t.Fatalf("transmissions = %d, different bodies %d, Message-Ids %d, ISA13 %d; want 4, 1, 1, 1", n, bodies, ids, isas)
	}
	var sent []byte
	s.must(s.pool.QueryRow(context.Background(), `SELECT body FROM mock_partner.received WHERE business_ref = $1 LIMIT 1`, ref).Scan(&sent))
	if !bytes.Equal(sent, s.rawOf("855", ref)) {
		t.Error("the bytes sent are not the bytes stored")
	}
	if got := s.text(`SELECT transmissions::text || '|' || incident_reason FROM outbound_docs`); got != "4|no_mdn" {
		t.Errorf("transmissions|reason = %s", got)
	}
	s.until("the saga is an incident", func() bool { return s.sagaState("order", ref) == "incident" })
	if got := s.reviewRules(); got != `["mdn.missing"]` {
		t.Errorf("review rules = %s", got)
	}
}

// outboundDocs makes n confirmed orders whose partner answers no 997, so that the documents wait
// in the state sent. It returns the references.
func (s *sagaRig) outboundDocs(n int) []string {
	s.t.Helper()
	s.knobs(`{"ack_status":"none"}`)
	var refs []string
	for range n {
		_, ref := s.confirmed()
		refs = append(refs, ref)
	}
	for _, ref := range refs {
		s.settleDoc("855", ref, "sent")
	}
	return refs
}

// T-ACK-02: a 997 with AK5 = A, E and R, and one with AK9 = P and a different AK5 for each
// set: A is delivered, E is delivered and written to the audit log as accepted with remarks,
// R is an incident, and in the mixed 997 every set gets its own result.
func TestT_ACK_02_VerdictsOfTheSets(t *testing.T) {
	s := newSagaRig(t)
	refs := s.outboundDocs(5)
	gs := make([]string, 5)
	st := make([]string, 5)
	for i, ref := range refs {
		gs[i], st[i] = s.docNumbers("855", ref)
	}
	// Documents 3 and 4 are made two sets of one group, as a partner's 997 may name them.
	s.exec(`UPDATE outbound_docs SET gs06 = $1 WHERE st02 = $2`, gs[3], st[4])
	gs[4] = gs[3]

	post := func(group, ak9 string, sets ...[2]string) {
		t.Helper()
		if code, body := s.send997(s.partnerID, s.ptoken, nextISA13(), group, ak9, sets...); code != http.StatusOK {
			t.Fatalf("997: HTTP %d %s", code, body)
		}
	}
	post(gs[0], "A", [2]string{st[0], "A"})
	post(gs[1], "E", [2]string{st[1], "E"})
	post(gs[2], "R", [2]string{st[2], "R"})
	post(gs[3], "P", [2]string{st[3], "A"}, [2]string{st[4], "R"})

	want := []string{"delivered", "delivered", "incident", "delivered", "incident"}
	for i, ref := range refs {
		if got := s.docState("855", ref); got != want[i] {
			t.Errorf("document %d: %s, want %s", i, got, want[i])
		}
	}
	if n := s.auditCount("ack_accepted_with_errors"); n != 1 {
		t.Errorf("audit rows ack_accepted_with_errors = %d, want 1", n)
	}
	if n := s.auditCount("ack_rejected"); n != 2 {
		t.Errorf("audit rows ack_rejected = %d, want 2", n)
	}
	s.until("the sagas follow their documents", func() bool {
		return s.sagaState("order", refs[0]) == "done" && s.sagaState("order", refs[1]) == "done" &&
			s.sagaState("order", refs[2]) == "incident" && s.sagaState("order", refs[4]) == "incident"
	})
}

// T-ACK-03: a 997 is matched by the partner that sent it, AK102 and AK202. A 997 of another
// partner with the same numbers touches nothing of ours, and no 997 shows in edi.list_inbox or
// edi.read_document.
func TestT_ACK_03_MatchingIsByPartnerAndNumbers(t *testing.T) {
	s := newSagaRig(t)
	refs := s.outboundDocs(1)
	gs, st := s.docNumbers("855", refs[0])

	other := "ZZ:ZZTESTOTHER"
	s.must(s.st.AddPartner(context.Background(), partnerOf("ta", other)))
	_, otoken := s.token("ta", "partner", other)
	if code, body := s.send997(other, otoken, nextISA13(), gs, "A", [2]string{st, "A"}); code != http.StatusOK {
		t.Fatalf("997 of the other partner: HTTP %d %s", code, body)
	}
	if got := s.docState("855", refs[0]); got != "sent" {
		t.Fatalf("a 997 of another partner changed our document to %s", got)
	}
	if s.auditCount("ack_unmatched") != 1 {
		t.Errorf("the unmatched 997 was not written to the audit log")
	}
	// A 997 with our partner but other numbers finds nothing either.
	if code, body := s.send997(s.partnerID, s.ptoken, nextISA13(), "999999", "A", [2]string{st, "A"}); code != http.StatusOK {
		t.Fatalf("997 of ours with other numbers: HTTP %d %s", code, body)
	}
	if got := s.docState("855", refs[0]); got != "sent" {
		t.Fatalf("other AK102 changed the document to %s", got)
	}

	if code, body := s.send997(s.partnerID, s.ptoken, nextISA13(), gs, "A", [2]string{st, "A"}); code != http.StatusOK {
		t.Fatalf("the right 997: HTTP %d %s", code, body)
	}
	if got := s.docState("855", refs[0]); got != "delivered" {
		t.Fatalf("the right 997 left the document %s", got)
	}

	// The agent never sees a 997.
	raw, wire := s.call(s.agent, "edi.list_inbox", map[string]any{})
	if wire != nil {
		t.Fatal(wire)
	}
	if n := s.count(`SELECT count(*) FROM edi_documents WHERE st01 = '997'`); n != 3 {
		t.Fatalf("stored 997 = %d, want 3", n)
	}
	if strings.Contains(string(raw), "997") {
		t.Errorf("a 997 is in the inbox: %s", raw)
	}
	for _, id := range s.ids(`SELECT id::text FROM edi_documents WHERE st01 = '997'`) {
		if _, wire := s.call(s.agent, "edi.read_document", map[string]any{"document_id": id}); reasonOf(wire) != "not_found" {
			t.Errorf("edi.read_document of a 997: %v", wire)
		}
	}
}

// T-ACK-04: the first final 997 decides. The same verdict again is ignored (audit ack_duplicate);
// R after A makes an incident, also for a saga that is done; an MDN that comes after the
// incident is only written to the audit log.
func TestT_ACK_04_FirstVerdictDecides(t *testing.T) {
	t.Run("repeat and contradiction", func(t *testing.T) {
		s := newSagaRig(t)
		_, ref := s.confirmed()
		s.settleDoc("855", ref, "delivered")
		s.until("the saga is done", func() bool { return s.sagaState("order", ref) == "done" })
		gs, st := s.docNumbers("855", ref)

		if code, body := s.send997(s.partnerID, s.ptoken, nextISA13(), gs, "A", [2]string{st, "A"}); code != http.StatusOK {
			t.Fatalf("repeat: HTTP %d %s", code, body)
		}
		if s.auditCount("ack_duplicate") != 1 || s.docState("855", ref) != "delivered" {
			t.Errorf("a repeat of the verdict must be ignored: duplicates %d, state %s", s.auditCount("ack_duplicate"), s.docState("855", ref))
		}
		if code, body := s.send997(s.partnerID, s.ptoken, nextISA13(), gs, "R", [2]string{st, "R"}); code != http.StatusOK {
			t.Fatalf("contradiction: HTTP %d %s", code, body)
		}
		if got := s.docState("855", ref); got != "incident" {
			t.Fatalf("R after A left the document %s, want incident", got)
		}
		if s.auditCount("ack_conflict") != 1 {
			t.Errorf("the contradiction is not in the audit log")
		}
		s.until("the finished saga becomes an incident", func() bool { return s.sagaState("order", ref) == "incident" })
		if !strings.Contains(s.reviewRules(), "ack.conflict") {
			t.Errorf("review rules = %s", s.reviewRules())
		}
		if tr, _ := s.received("855", ref); tr != 1 {
			t.Errorf("transmissions = %d, want 1: nothing is sent again", tr)
		}
	})

	t.Run("MDN after the incident", func(t *testing.T) {
		s := newSagaRig(t)
		s.knobs(`{"ack_status":"R","mdn_delay_ms":400}`) // the 997 overtakes the MDN
		_, ref := s.confirmed()
		s.until("the MDN is written to the audit log", func() bool { return s.auditCount("mdn_after_incident") == 1 })
		if got := s.docState("855", ref); got != "incident" {
			t.Errorf("state = %s: an MDN after the incident must not change it", got)
		}
		if got := s.text(`SELECT incident_reason FROM outbound_docs`); got != "ack_rejected" {
			t.Errorf("reason = %s", got)
		}
	})
}

// T-ACK-05: the relay stops after the numbers were allocated and before the send (the hook);
// after the restart the transmission carries the same ISA13, GS06 and ST02 and the same bytes.
func TestT_ACK_05_RestartKeepsTheNumbersAndBytes(t *testing.T) {
	s := newSagaRig(t)
	doc, ref := s.newOrder()
	o := s.createOrder(doc)
	if a := s.confirm(o.TxnID, "AC"); a.Status != "accepted" {
		t.Fatalf("confirm: %+v", a)
	}
	s.relay.BeforeSend = func() error { return errors.New("stopped before the send") }
	if _, err := s.relay.Step(context.Background()); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("the relay should have stopped: %v", err)
	}
	if tr, _ := s.received("855", ref); tr != 0 {
		t.Fatal("something was sent")
	}
	isa13, gs06, st02 := s.text(`SELECT isa13 FROM outbound_docs`), s.text(`SELECT gs06 FROM outbound_docs`), s.text(`SELECT st02 FROM outbound_docs`)
	stored := s.rawOf("855", ref)
	if got := s.text(`SELECT state || '|' || transmissions FROM outbound_docs`); got != "queued|0" {
		t.Fatalf("state|transmissions = %s", got)
	}

	s.relay.BeforeSend = nil // the restart
	s.settleDoc("855", ref, "delivered")
	var sent []byte
	var gotISA, gotGS, gotST string
	s.must(s.pool.QueryRow(context.Background(), `SELECT body, isa13, gs06, st02 FROM mock_partner.received WHERE business_ref = $1`, ref).
		Scan(&sent, &gotISA, &gotGS, &gotST))
	if gotISA != isa13 || gotGS != gs06 || gotST != st02 {
		t.Errorf("numbers after the restart: %s %s %s, before: %s %s %s", gotISA, gotGS, gotST, isa13, gs06, st02)
	}
	if !bytes.Equal(sent, stored) {
		t.Error("the bytes after the restart differ from the stored ones")
	}
}
