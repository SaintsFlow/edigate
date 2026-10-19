package txn_test

// Tests of wave 2-9c: the currency of a document without CUR, the default value of an optional
// enum in the request hash, and the label of the price list.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/SaintsFlow/edigate/internal/mode"
)

// bookInvoiceWithoutCUR posts an 810 without CUR, books it and lets two operators approve it. It
// returns the currency mock-1c stored.
func (r *rig) bookInvoiceWithoutCUR(ref string) string {
	r.t.Helper()
	supplier, stoken := r.supplier("ta")
	_, atoken := r.token("ta", "agent", "")
	_, op1 := r.token("ta", "operator", "")
	_, op2 := r.token("ta", "operator", "")
	cs := r.session(atoken)
	r.noCUR = true
	doc := r.send810("ta", supplier, stoken, nextISA13(), "1", "INV-"+ref, "SPO-"+ref, "3000", invoiceLine{"SKU-1", "10", "3.00"})
	a, wire := r.invoice(cs, doc)
	if wire != nil || a.Status != "pending_approval" {
		r.t.Fatalf("invoice without CUR: %+v %v", a, wire)
	}
	path := "/ops/approvals/" + a.ApprovalID + "/approve"
	for _, op := range []string{op1, op2} {
		if code, body := r.ops(op, http.MethodPost, path); code != http.StatusOK {
			r.t.Fatalf("approve: HTTP %d: %s", code, body)
		}
	}
	r.drain()
	r.drain()
	return r.text(`SELECT currency FROM mock_onec.vendor_invoices`)
}

// A clean 810 without CUR goes to a request for two operators and then books in the currency of
// the tenant; with a currency of the partner it books in that one.
func TestInvoiceWithoutCURTakesTheDefaultCurrency(t *testing.T) {
	r := newRig(t)
	r.supplier("ta")
	r.supplierOrder("ta", "SPO-1", "30.00") // in USD, the currency of the tenant
	if got := r.bookInvoiceWithoutCUR("1"); got != "USD" {
		t.Errorf("currency = %s, want USD (the tenant)", got)
	}

	r = newRig(t)
	r.exec(`UPDATE tenants SET currency = 'GBP' WHERE id = 'ta'`)
	supplier, _ := r.supplier("ta")
	r.exec(`UPDATE partners SET currency = 'EUR' WHERE tenant_id = 'ta' AND partner_id = $1`, supplier)
	r.exec(`INSERT INTO supplier_orders (tenant_id, partner_id, order_number, lines, total, currency)
	        VALUES ('ta', $1, 'SPO-2', '[{"sku":"SKU-1","qty":"10","unit_price":"3.00"}]', 30.00, 'EUR')`, supplier)
	if got := r.bookInvoiceWithoutCUR("2"); got != "EUR" {
		t.Errorf("currency = %s, want EUR (the partner before the tenant)", got)
	}
	if n := r.count(`SELECT count(*) FROM audit_log WHERE policy_explain::text LIKE '%currency%'`); n != 0 {
		t.Errorf("%d audit rows name a currency problem", n)
	}
}

// An 850 has no CUR in the order: the currency is that of the price list.
func TestOrderWithoutCURIsInTheCurrencyOfThePriceList(t *testing.T) {
	r := newRig(t)
	partner, ptoken := r.partner("ta")
	r.exec(`UPDATE price_list SET currency = 'EUR' WHERE tenant_id = 'ta'`)
	_, atoken := r.token("ta", "agent", "")
	cs := r.session(atoken)
	doc := r.send850("ta", partner, ptoken, nextISA13(), "1", "PO-CUR", oneLine...)
	if a, wire := r.order(cs, doc); wire != nil || a.Status != "accepted" {
		t.Fatalf("order: %+v %v", a, wire)
	}
	r.drain()
	if got := r.text(`SELECT currency FROM mock_onec.sales_orders`); got != "EUR" {
		t.Errorf("currency = %s, want EUR", got)
	}
}

// A call without delivery_instruction and one with "none" are the same request: one key, no
// conflict, and the order carries "none". A real change after them is a conflicting duplicate.
func TestAbsentOptionalEnumEqualsNone(t *testing.T) {
	r := newRig(t)
	partner, ptoken := r.partner("ta")
	_, atoken := r.token("ta", "agent", "")
	cs := r.session(atoken)
	doc := r.send850("ta", partner, ptoken, nextISA13(), "1", "PO-ENUM", oneLine...)

	first, wire := r.order(cs, doc) // the field is absent
	if wire != nil {
		t.Fatal(wire)
	}
	if again, wire := r.order(cs, doc, "none"); wire != nil || again != first {
		t.Fatalf("with none: %+v %v, want the answer of the first call %+v", again, wire, first)
	}
	if n := r.count(`SELECT count(*) FROM idempotency_keys`); n != 1 {
		t.Errorf("keys = %d, want 1", n)
	}
	if n := r.count(`SELECT count(*) FROM audit_log WHERE event_type = 'conflicting_duplicate'`); n != 0 {
		t.Errorf("conflicting_duplicate rows = %d, want 0", n)
	}
	if got := r.text(`SELECT payload->>'delivery_instruction' FROM outbox WHERE effect_type = 'onec.create_order'`); got != "none" {
		t.Errorf("delivery_instruction of the order = %q, want none", got)
	}
	if _, wire := r.order(cs, doc, "call_before_delivery"); reasonOf(wire) != "conflicting_duplicate" {
		t.Errorf("a real change: %v, want conflicting_duplicate", wire)
	}
}

// erp.get_price_list returns the label of the price list in every answer, in every mode that has
// the tool; null while no label is set.
func TestPriceListAnswerCarriesTheLabel(t *testing.T) {
	s := newSagaRig(t)
	ask := func() string {
		raw, errText := s.raw("erp.get_price_list", `{"sku":["SKU-1"]}`)
		if errText != "" {
			t.Fatalf("get_price_list: %s", errText)
		}
		var got struct {
			PriceListID *string `json:"price_list_id"`
			Prices      []any   `json:"prices"`
		}
		s.must(json.Unmarshal(raw, &got))
		if len(got.Prices) != 1 {
			t.Fatalf("prices: %s", raw)
		}
		if got.PriceListID == nil {
			return "null"
		}
		return *got.PriceListID
	}
	if got := ask(); got != "null" {
		t.Errorf("label before it is set = %s, want null", got)
	}
	s.must(s.st.SetPriceListID(t.Context(), "ta", "CANARY-TEST-1"))
	for _, m := range []mode.Mode{mode.Full, mode.Plain, mode.ReqHash} {
		s.play(m)
		if got := ask(); got != "CANARY-TEST-1" {
			t.Errorf("mode %s: label = %s, want CANARY-TEST-1", m, got)
		}
	}
	if strings.Contains(s.text(`SELECT coalesce(string_agg(detail::text, ''), '') FROM audit_log WHERE event_type = 'price_list_read'`), "CANARY") {
		t.Error("the label is in the audit log")
	}
}
