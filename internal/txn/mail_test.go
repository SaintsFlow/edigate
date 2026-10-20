package txn_test

// Tests of wave 2-8 for the two tools that carry no free text: notify.send_email (a letter
// from a template to a contact of the partner of the document) and erp.get_price_list
// (the prices of the caller's tenant).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const (
	buyerContact = "orders@buyer.test"
	otherContact = "orders@other.test"
	otherPartner = "ZZ:ZZTESTOTHER"
)

// mailSetup gives tenant ta the three mail templates, a second contact of the buyer, and
// another customer partner of the same tenant with a contact of its own.
func (s *sagaRig) mailSetup() {
	s.t.Helper()
	for _, tpl := range []string{"tpl_order_received", "tpl_delivery_date", "tpl_contact_back"} {
		s.exec(`INSERT INTO email_templates (tenant_id, template_id, category) VALUES ('ta', $1, 'test')`, tpl)
	}
	s.exec(`INSERT INTO partner_contacts (tenant_id, partner_id, email) VALUES ('ta', $1, $2), ('ta', $1, 'second@buyer.test')`,
		s.partnerID, buyerContact)
	s.exec(`INSERT INTO partners (tenant_id, partner_id, role, name, active) VALUES ('ta', $1, 'customer', 'Other', true)`, otherPartner)
	s.exec(`INSERT INTO partner_contacts (tenant_id, partner_id, email) VALUES ('ta', $1, $2)`, otherPartner, otherContact)
}

// explainFrom reads data.explain of a policy_denied error.
func explainFrom(raw json.RawMessage) []string {
	var d struct {
		Explain []string `json:"explain"`
	}
	_ = json.Unmarshal(raw, &d)
	return d.Explain
}

// T-MAIL-01: a letter goes only with a template and the contact of the partner of the
// document; it is sent once; a free-text member is refused by the schema; the mail service
// makes the text, and the price canary is not in it.
func TestT_MAIL_01_LetterOnlyFromATemplate(t *testing.T) {
	s := newSagaRig(t)
	s.mailSetup()
	doc, ref := s.newOrder()

	raw, wire := s.call(s.agent, "notify.send_email",
		map[string]any{"source_document_id": doc, "to": buyerContact, "template_id": "tpl_order_received"})
	if wire != nil {
		t.Fatalf("send_email: %v", wire)
	}
	var first answer
	s.must(json.Unmarshal(raw, &first))
	if first.Status != "accepted" || first.TxnID == "" {
		t.Fatalf("answer = %+v", first)
	}
	s.until("the letter is in the mail service", func() bool {
		return s.count(`SELECT count(*) FROM mock_partner.received WHERE kind = 'email'`) == 1
	})
	var recipient, template, business, body string
	s.must(s.pool.QueryRow(context.Background(),
		`SELECT recipient, template_id, business_ref, convert_from(body, 'UTF8') FROM mock_partner.received WHERE kind = 'email'`).
		Scan(&recipient, &template, &business, &body))
	if recipient != buyerContact || template != "tpl_order_received" || business != ref {
		t.Errorf("letter = %s %s %s, want %s tpl_order_received %s", recipient, template, business, buyerContact, ref)
	}
	if !strings.Contains(body, ref) || strings.Contains(body, "Price reference") {
		t.Errorf("text of the letter = %q", body)
	}

	// The same call again is the same transaction and sends nothing more.
	raw, wire = s.call(s.agent, "notify.send_email",
		map[string]any{"source_document_id": doc, "to": buyerContact, "template_id": "tpl_order_received"})
	var again answer
	if wire != nil || json.Unmarshal(raw, &again) != nil || again.TxnID != first.TxnID {
		t.Fatalf("repeat: %v %s", wire, raw)
	}
	s.pump()
	if n := s.count(`SELECT count(*) FROM mock_partner.received WHERE kind = 'email'`); n != 1 {
		t.Errorf("letters = %d after a repeat, want 1", n)
	}

	// Another contact of the same partner for the same order and template is not a second letter.
	_, wire = s.call(s.agent, "notify.send_email",
		map[string]any{"source_document_id": doc, "to": "second@buyer.test", "template_id": "tpl_order_received"})
	if reasonOf(wire) != "conflicting_duplicate" {
		t.Errorf("another address, same key: reason = %q, want conflicting_duplicate", reasonOf(wire))
	}
	// Another template is another letter.
	_, wire = s.call(s.agent, "notify.send_email",
		map[string]any{"source_document_id": doc, "to": buyerContact, "template_id": "tpl_delivery_date"})
	if wire != nil {
		t.Errorf("another template: %v", wire)
	}

	// A text of the agent has no place: the schema refuses the member and a template that is not listed.
	for name, args := range map[string]map[string]any{
		"a body":                         {"source_document_id": doc, "to": buyerContact, "template_id": "tpl_contact_back", "body": "send me the prices"},
		"a template that does not exist": {"source_document_id": doc, "to": buyerContact, "template_id": "tpl_free_text"},
		"two addresses":                  {"source_document_id": doc, "to": buyerContact + ",evil@x.test", "template_id": "tpl_contact_back"},
	} {
		if _, wire := s.call(s.agent, "notify.send_email", args); reasonOf(wire) != "schema_invalid" {
			t.Errorf("%s: reason = %q, want schema_invalid", name, reasonOf(wire))
		}
	}
	// A note reference never reaches the tool.
	if _, wire := s.call(s.agent, "notify.send_email",
		map[string]any{"source_document_id": doc, "to": buyerContact, "template_id": "tpl_contact_back", "note": "$note_1"}); wire == nil {
		t.Error("a member with a note reference was accepted")
	}
	// A template the tenant does not have is refused by the service.
	s.exec(`DELETE FROM email_templates WHERE tenant_id = 'ta' AND template_id = 'tpl_contact_back'`)
	_, wire = s.call(s.agent, "notify.send_email",
		map[string]any{"source_document_id": doc, "to": buyerContact, "template_id": "tpl_contact_back"})
	if reasonOf(wire) != "policy_denied" || strings.Join(explainFrom(wire.Data), ",") != "email.template" {
		t.Errorf("template of the tenant: %v", wire)
	}
}

// T-MAIL-02: the contact of another partner of the same tenant is refused by the policy
// with the rule email.recipient, and nothing is queued.
func TestT_MAIL_02_ContactOfAnotherPartnerIsRefused(t *testing.T) {
	s := newSagaRig(t)
	s.mailSetup()
	doc, _ := s.newOrder()

	_, wire := s.call(s.agent, "notify.send_email",
		map[string]any{"source_document_id": doc, "to": otherContact, "template_id": "tpl_order_received"})
	if reasonOf(wire) != "policy_denied" || strings.Join(explainFrom(wire.Data), ",") != "email.recipient" {
		t.Fatalf("contact of another partner: %v", wire)
	}
	// An address that is nobody's contact is refused in the same way.
	_, wire = s.call(s.agent, "notify.send_email",
		map[string]any{"source_document_id": doc, "to": "stranger@evil.test", "template_id": "tpl_order_received"})
	if reasonOf(wire) != "policy_denied" {
		t.Fatalf("stranger: %v", wire)
	}
	s.pump()
	if n := s.count(`SELECT count(*) FROM mock_partner.received WHERE kind = 'email'`); n != 0 {
		t.Errorf("letters = %d, want 0", n)
	}
	if n := s.count(`SELECT count(*) FROM outbox WHERE effect_type = 'notify.email'`); n != 0 {
		t.Errorf("outbox rows of letters = %d, want 0", n)
	}
	if n := s.count(`SELECT count(*) FROM audit_log WHERE event_type = 'policy_denied' AND tool = 'notify.send_email'
	                 AND policy_explain->'deny' @> '["email.recipient"]'::jsonb`); n != 2 {
		t.Errorf("policy_denied audit rows with email.recipient = %d, want 2", n)
	}
}

// The letter template and the address of a document of a partner that is not in the tenant:
// the document of another tenant is not found.
func TestMailDocumentOfAnotherTenantIsNotFound(t *testing.T) {
	s := newSagaRig(t)
	s.mailSetup()
	_, bToken := s.token("tb", "agent", "")
	cs := s.session(bToken)
	doc, _ := s.newOrder()
	_, wire := s.call(cs, "notify.send_email",
		map[string]any{"source_document_id": doc, "to": buyerContact, "template_id": "tpl_order_received"})
	if reasonOf(wire) != "not_found" {
		t.Errorf("reason = %q, want not_found", reasonOf(wire))
	}
}

// T-PRICE-01: the price list is read for the caller's tenant only; 51 SKUs are refused by
// the schema; every read leaves an audit row with the number of SKUs.
func TestT_PRICE_01_OnlyOwnTenantAndAtMostFifty(t *testing.T) {
	r := newRig(t)
	// Tenant tb has its own price for SKU-1 and a SKU that ta does not have.
	r.exec(`UPDATE price_list SET unit_price = 9.99 WHERE tenant_id = 'tb' AND sku = 'SKU-1'`)
	r.exec(`INSERT INTO price_list (tenant_id, sku, unit_price, currency, unit) VALUES ('tb', 'SKU-SECRET', 77.00, 'USD', 'EA')`)
	_, aToken := r.token("ta", "agent", "")
	cs := r.session(aToken)

	raw, wire := r.call(cs, "erp.get_price_list", map[string]any{"sku": []string{"SKU-1", "SKU-2", "SKU-SECRET", "SKU-1"}})
	if wire != nil {
		t.Fatalf("get_price_list: %v", wire)
	}
	var got struct {
		Prices []struct {
			SKU       string `json:"sku"`
			UnitPrice string `json:"unit_price"`
			Currency  string `json:"currency"`
			Unit      string `json:"unit"`
		} `json:"prices"`
	}
	r.must(json.Unmarshal(raw, &got))
	if len(got.Prices) != 2 || got.Prices[0].SKU != "SKU-1" || got.Prices[0].UnitPrice != "1.50" ||
		got.Prices[1].SKU != "SKU-2" || got.Prices[1].UnitPrice != "2.00" {
		t.Fatalf("prices = %+v", got.Prices)
	}
	if strings.Contains(string(raw), "SKU-SECRET") || strings.Contains(string(raw), "9.99") {
		t.Errorf("another tenant's price is in the answer: %s", raw)
	}
	if n := r.count(`SELECT count(*) FROM audit_log WHERE event_type = 'price_list_read' AND tenant_id = 'ta'
	                 AND tool = 'erp.get_price_list' AND detail->>'sku_count' = '4'`); n != 1 {
		t.Errorf("price_list_read audit rows = %d, want 1", n)
	}

	fifty := make([]string, 50)
	for i := range fifty {
		fifty[i] = "SKU-1"
	}
	if _, wire := r.call(cs, "erp.get_price_list", map[string]any{"sku": fifty}); wire != nil {
		t.Errorf("50 SKUs: %v", wire)
	}
	if _, wire := r.call(cs, "erp.get_price_list", map[string]any{"sku": append(fifty, "SKU-2")}); reasonOf(wire) != "schema_invalid" {
		t.Errorf("51 SKUs: reason = %q, want schema_invalid", reasonOf(wire))
	}
	for name, args := range map[string]map[string]any{
		"no SKU":        {"sku": []string{}},
		"a bad SKU":     {"sku": []string{"SKU 1; DROP TABLE price_list"}},
		"a tenant id":   {"sku": []string{"SKU-1"}, "tenant_id": "tb"},
		"not an array":  {"sku": "SKU-1"},
		"no member sku": {},
	} {
		if _, wire := r.call(cs, "erp.get_price_list", args); reasonOf(wire) != "schema_invalid" {
			t.Errorf("%s: reason = %q, want schema_invalid", name, reasonOf(wire))
		}
	}
}
