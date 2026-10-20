package txn_test

// Tests of wave 2-9a: the modes of the benchmark (one binary, the tool names of the full
// gateway, one protection off or all of them off) and the slots (two gateway processes with
// databases of their own and the mocks in common). A rig plays a mode by swapping the MCP
// server behind its listener (sagaRig.play).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/mcpserver"
	"github.com/SaintsFlow/edigate/internal/mode"
	"github.com/SaintsFlow/edigate/internal/policy"
	"github.com/SaintsFlow/edigate/internal/tools"
)

// play makes the gateway of the rig play a mode.
func (s *sagaRig) play(m mode.Mode) {
	s.t.Helper()
	var dec tools.Decider = s.rec
	if m == mode.NoPolicy {
		dec = policy.AllowAll{}
	}
	s.runner.Policy = dec
	env := tools.PlainEnv{
		Onec: adapters.NewOnec(s.mockURL), WMS: adapters.NewWMS(s.wmsURL),
		Mail: adapters.NewMail(s.partnerURL), AS2: adapters.NewAS2(s.partnerURL),
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s.srv.Store(mcpserver.New(s.st, nil, dec, log, mcpserver.WithMode(m), mcpserver.WithPlain(env)))
}

// toolNames lists the tools the agent sees.
func (s *sagaRig) toolNames() []string {
	s.t.Helper()
	res, err := s.agent.ListTools(context.Background(), nil)
	s.must(err)
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

// raw calls a tool with arguments written as JSON text, so that a test controls the order of
// the keys. It returns the structured answer or the error.
func (s *sagaRig) raw(tool, args string) (json.RawMessage, string) {
	s.t.Helper()
	res, err := s.agent.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: json.RawMessage(args)})
	if err != nil {
		var wire *jsonrpc.Error
		if errors.As(err, &wire) {
			return nil, err.Error() + " " + string(wire.Data)
		}
		return nil, err.Error()
	}
	out, err := json.Marshal(res.StructuredContent)
	s.must(err)
	return out, ""
}

const openOrderArgs = `{"partner_id":"ZZ:ZZTESTBUYER","business_ref":"PO-OPEN-1","ship_to":"DC01","currency":"USD",` +
	`"lines":[{"sku":"SKU-1","qty":"10","unit":"EA","unit_price":"9.99"}]}`

// The tools/list of every mode: the same names, and edi.extract_note only where the
// quarantined reader exists (full, no-policy, no-bizkey, no-contracts).
func TestModes_ToolListByMode(t *testing.T) {
	s := newSagaRig(t)
	all := []string{
		"edi.extract_note", "edi.list_inbox", "edi.read_document", "edi.send_asn", "erp.confirm_order",
		"erp.create_sales_order", "erp.get_price_list", "erp.post_vendor_invoice", "notify.send_email", "txn.status", "wms.reserve",
	}
	without := slices.DeleteFunc(slices.Clone(all), func(n string) bool { return n == "edi.extract_note" })
	want := map[mode.Mode][]string{
		mode.Full: all, mode.NoPolicy: all, mode.NoBizKey: all, mode.NoContracts: all,
		mode.Plain: without, mode.ReqHash: without, mode.NoSanitize: without,
	}
	for _, m := range mode.All {
		s.play(m)
		if got := s.toolNames(); !slices.Equal(got, want[m]) {
			t.Errorf("mode %s: tools = %v, want %v", m, got, want[m])
		}
	}
}

// Plain (C0): the price of the model is written to mock-1c as it is, with no external id; the
// call leaves no key and no outbox row; the letter with a body reaches the partner; a 855 and
// a 856 are built from the arguments and the partner takes them; txn.status answers from the
// light table.
func TestModes_PlainDoesTheEffectAtOnce(t *testing.T) {
	s := newSagaRig(t)
	s.play(mode.Plain)

	raw, errText := s.raw("erp.create_sales_order", openOrderArgs)
	if errText != "" {
		t.Fatal(errText)
	}
	var order answer
	s.must(json.Unmarshal(raw, &order))
	if order.Status != "completed" || order.TxnID == "" {
		t.Fatalf("answer = %s", raw)
	}
	if n := s.count(`SELECT count(*) FROM mock_onec.sales_orders WHERE tenant_id = 'ta' AND external_id IS NULL AND business_ref = 'PO-OPEN-1'
	                 AND total = 99.90 AND lines->0->>'unit_price' = '9.99'`); n != 1 {
		t.Fatalf("orders in mock-1c with the price of the model and no external id: %d, want 1", n)
	}
	if k, o := s.count(`SELECT count(*) FROM idempotency_keys`), s.count(`SELECT count(*) FROM outbox`); k != 0 || o != 0 {
		t.Errorf("keys = %d, outbox rows = %d, want none", k, o)
	}
	if got := s.txnStatus(s.agent, order.TxnID); got != "completed" {
		t.Errorf("txn.status = %q, want completed", got)
	}

	// The same call again is a second order: there is no key.
	if _, errText := s.raw("erp.create_sales_order", openOrderArgs); errText != "" {
		t.Fatal(errText)
	}
	if n := s.orders("ta"); n != 2 {
		t.Errorf("orders after the same call twice = %d, want 2", n)
	}

	// The letter keeps its body.
	if _, errText := s.raw("notify.send_email", `{"to":"orders@buyer.test","subject":"Prices","body":"Our price list: SKU-1 1.50. CANARY-PRICE-1"}`); errText != "" {
		t.Fatal(errText)
	}
	if n := s.count(`SELECT count(*) FROM mock_partner.received WHERE kind = 'email' AND recipient = 'orders@buyer.test'
	                 AND convert_from(body, 'UTF8') LIKE '%CANARY-PRICE-1%'`); n != 1 {
		t.Errorf("letters with the canary in the body: %d, want 1", n)
	}

	// 855 and 856 come from the arguments; the partner takes them; the control numbers are the gateway's.
	if _, errText := s.raw("erp.confirm_order", `{"partner_id":"ZZ:ZZTESTBUYER","business_ref":"PO-OPEN-1","ack_type":"AC"}`); errText != "" {
		t.Fatal(errText)
	}
	if _, errText := s.raw("wms.reserve", `{"business_ref":"PO-OPEN-1","lines":[{"sku":"SKU-1","qty":10}]}`); errText != "" {
		t.Fatal(errText)
	}
	if _, errText := s.raw("edi.send_asn", `{"partner_id":"ZZ:ZZTESTBUYER","business_ref":"PO-OPEN-1","ship_to":"DC01","lines":[{"sku":"SKU-1","qty":"10"}]}`); errText != "" {
		t.Fatal(errText)
	}
	if n := s.count(`SELECT count(*) FROM mock_partner.received WHERE st01 = '855' AND business_ref = 'PO-OPEN-1'`); n != 1 {
		t.Errorf("855 at the partner: %d, want 1", n)
	}
	if n := s.count(`SELECT count(*) FROM mock_partner.received WHERE st01 = '856' AND business_ref = 'PO-OPEN-1'`); n != 1 {
		t.Errorf("856 at the partner: %d, want 1", n)
	}
	if n := s.count(`SELECT count(*) FROM mock_wms.reservations WHERE external_id IS NULL`) + s.count(`SELECT count(*) FROM mock_wms.shipments WHERE external_id IS NULL`); n != 2 {
		t.Errorf("rows of the warehouse without an external id: %d, want 2", n)
	}
	if n := s.count(`SELECT count(*) FROM sagas`) + s.count(`SELECT count(*) FROM outbound_docs`); n != 0 {
		t.Errorf("sagas and outgoing documents: %d, want none", n)
	}

	// A 4xx of the other system is an answer; the model sees it.
	s.mock.SetRejectRefPrefix("PO-REFUSED")
	raw, errText = s.raw("erp.create_sales_order", strings.Replace(openOrderArgs, "PO-OPEN-1", "PO-REFUSED-1", 1))
	if errText != "" || !strings.Contains(string(raw), `"rejected"`) {
		t.Errorf("a refused order: %s %s", raw, errText)
	}

	// The schema holds: an unknown member is refused.
	if _, errText := s.raw("erp.create_sales_order", strings.Replace(openOrderArgs, `"currency"`, `"discount":"50%","currency"`, 1)); errText == "" {
		t.Error("an unknown member must be refused by the schema")
	}
}

// Reqhash (C1): the same arguments (also with the keys in another order) get the stored answer
// and the ERP is not called; another value is a new call.
func TestModes_ReqHashStoresTheAnswer(t *testing.T) {
	s := newSagaRig(t)
	s.play(mode.ReqHash)

	first, errText := s.raw("erp.create_sales_order", openOrderArgs)
	if errText != "" {
		t.Fatal(errText)
	}
	reordered := `{"lines":[{"unit_price":"9.99","unit":"EA","qty":"10","sku":"SKU-1"}],"currency":"USD","ship_to":"DC01",` +
		`"business_ref":"PO-OPEN-1","partner_id":"ZZ:ZZTESTBUYER"}`
	for _, args := range []string{openOrderArgs, reordered} {
		again, errText := s.raw("erp.create_sales_order", args)
		if errText != "" {
			t.Fatal(errText)
		}
		if string(again) != string(first) {
			t.Errorf("repeat = %s, want the stored %s", again, first)
		}
	}
	if n := s.orders("ta"); n != 1 {
		t.Fatalf("orders after three calls with one meaning = %d, want 1", n)
	}
	other, errText := s.raw("erp.create_sales_order", strings.Replace(openOrderArgs, `"qty":"10"`, `"qty":"11"`, 1))
	if errText != "" {
		t.Fatal(errText)
	}
	if string(other) == string(first) || s.orders("ta") != 2 {
		t.Errorf("another quantity must be a second order: %s, orders = %d", other, s.orders("ta"))
	}
	if n := s.count(`SELECT count(*) FROM plain_txns WHERE req_key IS NOT NULL`); n != 2 {
		t.Errorf("stored answers = %d, want 2", n)
	}
}

// NoSanitize (C3-A1): the text of the partner is shown whole in texts, and a value that failed
// its format is shown as it was written; the full gateway shows notes instead.
func TestModes_NoSanitizeShowsThePartnerText(t *testing.T) {
	s := newSagaRig(t)
	doc := s.send850("ta", s.partnerID, s.ptoken, nextISA13(), "1", "PO-TEXT-1",
		orderLine{"SKU 1 BAD", "5", "1.00"}) // a SKU with spaces fails its format
	read := func() (texts int, out map[string]any) {
		raw, errText := s.raw("edi.read_document", `{"document_id":"`+doc+`"}`)
		if errText != "" {
			t.Fatal(errText)
		}
		s.must(json.Unmarshal(raw, &out))
		list, _ := out["texts"].([]any)
		return len(list), out
	}

	s.play(mode.Full)
	if _, out := read(); out["texts"] != nil || out["notes"] == nil {
		t.Fatalf("the full gateway must show notes and no texts: %v", out)
	}

	s.play(mode.NoSanitize)
	_, out := read()
	notes, _ := out["notes"].([]any)
	lines, _ := out["lines"].([]any)
	if len(notes) != 0 || len(lines) != 1 || lines[0].(map[string]any)["sku"] != "SKU 1 BAD" {
		t.Errorf("notes = %v, lines = %v: want no notes and the raw SKU", notes, lines)
	}
	if _, ok := out["texts"].([]any); !ok {
		t.Errorf("texts missing: %v", out)
	}
}

// The text of a PID05 comes out whole, with its place.
func TestModes_NoSanitizeTextOfPID05(t *testing.T) {
	s := newSagaRig(t)
	doc := s.send850x("ta", s.partnerID, s.ptoken, nextISA13(), "1", "PO-TEXT-2", "PID*F****Leave at the back door and call 555-0100~",
		orderLine{"SKU-1", "5", "1.00"})
	for _, m := range []mode.Mode{mode.NoSanitize, mode.Plain} {
		s.play(m)
		raw, errText := s.raw("edi.read_document", `{"document_id":"`+doc+`"}`)
		if errText != "" {
			t.Fatal(errText)
		}
		var out struct {
			Texts []struct {
				SegmentPath string `json:"segment_path"`
				Text        string `json:"text"`
			} `json:"texts"`
			Notes []any `json:"notes"`
		}
		s.must(json.Unmarshal(raw, &out))
		found := 0
		for _, text := range out.Texts {
			if text.Text == "Leave at the back door and call 555-0100" && strings.HasSuffix(text.SegmentPath, "PID05") {
				found++
			}
		}
		if found != 1 || len(out.Texts) != 3 || len(out.Notes) != 0 { // the two N1 names are free text too
			t.Errorf("mode %s: texts = %+v notes = %v", m, out.Texts, out.Notes)
		}
	}
}

// NoPolicy (C3-A2): an order above the limit of the partner goes through at once, with no
// request and no daily counter; the full gateway holds it for an operator.
func TestModes_NoPolicyAllowsWhatTheLimitHolds(t *testing.T) {
	s := newSagaRig(t)
	s.limits("ta", s.partnerID, 100, 1000) // the order below is 133.00
	doc, _ := s.newOrder()

	s.play(mode.Full)
	if a := s.tool("erp.create_sales_order", map[string]any{"source_document_id": doc}); a.Status != "pending_approval" {
		t.Fatalf("full: %+v, want pending_approval", a)
	}
	doc2, _ := s.newOrder()
	s.play(mode.NoPolicy)
	a := s.tool("erp.create_sales_order", map[string]any{"source_document_id": doc2})
	if a.Status != "accepted" {
		t.Fatalf("no-policy: %+v, want accepted", a)
	}
	if n := s.count(`SELECT count(*) FROM approvals WHERE tool = 'erp.create_sales_order'`); n != 1 { // the one of the full run
		t.Errorf("approval requests = %d, want 1 (only the full run)", n)
	}
	var total float64
	s.must(s.pool.QueryRow(context.Background(), `SELECT COALESCE(sum(total), 0)::float8 FROM daily_auto_totals`).Scan(&total))
	if total != 0 {
		t.Errorf("the daily counter holds %v, want 0", total)
	}
	s.until("the order of the no-policy run is created", func() bool { return s.txnStatus(s.agent, a.TxnID) == "completed" })
	// The data check stays: a SKU that is not in the price list is still refused.
	doc3 := s.send850("ta", s.partnerID, s.ptoken, nextISA13(), "1", "PO-NOSKU", orderLine{"SKU-NONE", "1", "1.00"})
	if _, wire := s.order(s.agent, doc3); reasonOf(wire) != "policy_denied" {
		t.Errorf("unknown SKU: %v, want policy_denied", wire)
	}
}

// NoBizKey (C3-A3): the key is made of the arguments, so the same order in a new exchange is
// a second order (the duplicate F4 gives), while the same call twice is still one.
func TestModes_NoBizKeyDuplicatesAnOrderOfANewExchange(t *testing.T) {
	s := newSagaRig(t)
	s.play(mode.NoBizKey)
	first, ref := s.newOrder()
	second := s.send850("ta", s.partnerID, s.ptoken, nextISA13(), "1", ref, orderLine{"SKU-1", "40", "1.00"}, orderLine{"SKU-2", "36.5", "1.00"})

	a := s.tool("erp.create_sales_order", map[string]any{"source_document_id": first})
	again := s.tool("erp.create_sales_order", map[string]any{"source_document_id": first})
	b := s.tool("erp.create_sales_order", map[string]any{"source_document_id": second})
	if a.TxnID != again.TxnID {
		t.Errorf("the same call twice: %s and %s, want one transaction", a.TxnID, again.TxnID)
	}
	if b.Status != "accepted" || b.TxnID == a.TxnID {
		t.Fatalf("the same order in a new exchange: %+v, want a new transaction", b)
	}
	s.until("both orders are created", func() bool {
		return s.txnStatus(s.agent, a.TxnID) == "completed" && s.txnStatus(s.agent, b.TxnID) == "completed"
	})
	if n := s.orders("ta"); n != 2 {
		t.Errorf("orders = %d, want 2 (the duplicate of F4)", n)
	}
	if n := s.count(`SELECT count(*) FROM sagas WHERE kind = 'order' AND business_key->>1 = $1`, ref); n != 2 {
		t.Errorf("sagas of the reference = %d, want 2", n)
	}
	// The sagas go on: each order can be confirmed by its own txn_id.
	for _, id := range []string{a.TxnID, b.TxnID} {
		if c := s.tool("erp.confirm_order", map[string]any{"sales_order_txn_id": id, "ack_type": "AC"}); c.Status != "accepted" {
			t.Errorf("confirm %s: %+v", id, c)
		}
	}
	s.until("both documents are sent", func() bool {
		return s.count(`SELECT count(*) FROM outbound_docs WHERE state IN ('sent', 'delivered')`) == 2
	})
}

// NoContracts (C3-A4): the price of the model reaches mock-1c, but the policy still refuses a
// ship-to that is not the partner's, and the effect goes through the outbox with a key.
func TestModes_NoContractsTakesModelValuesButKeepsThePolicy(t *testing.T) {
	s := newSagaRig(t)
	s.play(mode.NoContracts)

	raw, errText := s.raw("erp.create_sales_order", openOrderArgs)
	if errText != "" {
		t.Fatal(errText)
	}
	var a answer
	s.must(json.Unmarshal(raw, &a))
	if a.Status != "accepted" || a.TxnID == "" {
		t.Fatalf("answer = %s", raw)
	}
	s.until("the order of the model is created", func() bool { return s.txnStatus(s.agent, a.TxnID) == "completed" })
	if n := s.count(`SELECT count(*) FROM mock_onec.sales_orders WHERE tenant_id = 'ta' AND external_id IS NOT NULL
	                 AND business_ref = 'PO-OPEN-1' AND lines->0->>'unit_price' = '9.99'`); n != 1 {
		t.Errorf("orders with the price of the model: %d, want 1", n)
	}
	// The business key comes from the arguments: the same partner and number is the same order.
	again, errText := s.raw("erp.create_sales_order", openOrderArgs)
	if errText != "" || !strings.Contains(string(again), a.TxnID) || s.orders("ta") != 1 {
		t.Errorf("the same partner and number: %s %s, orders = %d", again, errText, s.orders("ta"))
	}

	_, errText = s.raw("erp.create_sales_order", strings.Replace(strings.Replace(openOrderArgs, "DC01", "NOWHERE", 1), "PO-OPEN-1", "PO-OPEN-2", 1))
	if !strings.Contains(errText, "policy_denied") || !strings.Contains(errText, "ship_to.unknown") {
		t.Errorf("an unknown ship-to: %q, want policy_denied with ship_to.unknown", errText)
	}
	// The sagas work on the order of the model: confirm it by partner and number.
	if _, errText := s.raw("erp.confirm_order", `{"partner_id":"ZZ:ZZTESTBUYER","business_ref":"PO-OPEN-1","ack_type":"AC"}`); errText != "" {
		t.Fatal(errText)
	}
	s.settleDoc("855", "PO-OPEN-1", "delivered")

	// A letter to an address nobody has among the contacts finds no partner and is refused.
	_, errText = s.raw("notify.send_email", `{"to":"stranger@evil.test","subject":"x","body":"y"}`)
	if !strings.Contains(errText, "policy_denied") {
		t.Errorf("a letter to a stranger: %q, want policy_denied", errText)
	}
}

// Two slots (protocol section 6): two gateway processes, each with a database of its own, one
// set of mocks. Each sends its own document with the header Edigate-Reply-To; the 997 of each
// comes back to the sender, so both sagas end done, and one process knows nothing of the other.
func TestModes_TwoSlotsShareOnlyTheMocks(t *testing.T) {
	a := newSagaRig(t)
	// The first slot also names itself in the header and in the tokens file.
	a.must(addPartnerToken(a.tokensFile, a.url+" "+a.partnerID, a.ptoken))
	a.relay.AS2 = adapters.NewAS2(a.partnerURL).WithReplyTo(a.url)
	b := newSagaRigWith(t, a)

	type placed struct {
		slot     *sagaRig
		ref, txn string
	}
	var all []placed
	for _, slot := range []*sagaRig{a, b} {
		doc, ref := slot.newOrder()
		o := slot.createOrder(doc)
		if c := slot.tool("erp.confirm_order", map[string]any{"sales_order_txn_id": o.TxnID, "ack_type": "AC"}); c.Status != "accepted" {
			t.Fatalf("confirm: %+v", c)
		}
		all = append(all, placed{slot, ref, o.TxnID})
	}
	done := func() bool {
		for _, p := range all {
			if p.slot.count(`SELECT count(*) FROM sagas WHERE kind = 'order' AND business_key->>1 = $1 AND state = 'done'`, p.ref) != 1 {
				return false
			}
		}
		return true
	}
	for range 400 {
		a.pump()
		b.pump()
		if done() {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !done() {
		t.Fatal("the sagas of both slots did not end done")
	}
	for i, p := range all {
		other := all[1-i]
		if n := p.slot.count(`SELECT count(*) FROM sagas WHERE business_key->>1 = $1`, other.ref); n != 0 {
			t.Errorf("slot %d knows the saga of the other slot", i)
		}
		if n := p.slot.count(`SELECT count(*) FROM outbound_docs WHERE state = 'delivered'`); n != 1 {
			t.Errorf("slot %d: delivered documents = %d, want 1", i, n)
		}
	}
	// The mocks are shared: both orders and both 855 are in the one set.
	if n := a.count(`SELECT count(*) FROM mock_onec.sales_orders`); n != 2 {
		t.Errorf("orders in the shared mock-1c = %d, want 2", n)
	}
	if n := a.count(`SELECT count(*) FROM mock_partner.received WHERE st01 = '855'`); n != 2 {
		t.Errorf("855 at the shared partner = %d, want 2", n)
	}
}
