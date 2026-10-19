package tools

import (
	"context"
	"testing"

	"github.com/SaintsFlow/edigate/internal/policy"
	"github.com/SaintsFlow/edigate/internal/store"
)

// The text of a note whose reason is "format" goes back in the place of its reference, wherever it
// stands in the body; a free text is not in the body and nothing is substituted for it.
func TestResolveNotes(t *testing.T) {
	body := []byte(`{"header":{"ship_to":"$note_2","buyer":"B001","references":[{"qualifier":"ZZ","id":"$note_3"}]},` +
		`"lines":[{"sku":"$note_1","qty":5.5,"unit":"EA"}]}`)
	notes := []store.RawNote{
		{Ref: "$note_1", Reason: "format", Text: "SKU 1 BAD"},
		{Ref: "$note_2", Reason: "format", Text: "DC 01"},
		{Ref: "$note_3", Reason: "free_text", Text: "never in the body"},
	}
	got, err := resolveNotes(body, notes)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"header":{"buyer":"B001","references":[{"id":"$note_3","qualifier":"ZZ"}],"ship_to":"DC 01"},"lines":[{"qty":5.5,"sku":"SKU 1 BAD","unit":"EA"}]}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if same, _ := resolveNotes(body, nil); string(same) != string(body) {
		t.Error("a body without format notes must stay as it is")
	}
}

// AllowAll allows, asks for no approval and keeps out of the daily counter.
func TestAllowAll(t *testing.T) {
	var dec Decider = policy.AllowAll{}
	d, err := dec.Decide(context.Background(), policy.Principal{}, policy.ToolCall{Tool: "erp.create_sales_order"})
	if err != nil || !d.Allow || d.Approval != policy.ApprovalNone || len(d.Deny) != 0 || len(d.Fired) != 0 {
		t.Errorf("decision = %+v, %v", d, err)
	}
	if c, ok := dec.(countsDaily); !ok || c.Counts() {
		t.Error("AllowAll must say that it does not count")
	}
}

// requestKeys: the business key by default; the arguments in the mode no-bizkey, where the
// key is the same for the same arguments in another form and the stored key gets a third part.
func TestRequestKeys(t *testing.T) {
	c := Caller{TenantID: "ta"}
	k1, bk1, err := requestKeys(c, "erp.create_sales_order", "P", "R", "P", "R")
	if err != nil || string(bk1) != `["P","R"]` {
		t.Fatalf("business key: %s, %v", bk1, err)
	}
	if k2, _, _ := requestKeys(c, "erp.create_sales_order", "P", "R", "P", "R"); k1 != k2 {
		t.Error("the same business key must give the same key")
	}
	a := Caller{TenantID: "ta", ArgsKey: true, Args: []byte(`{"b": 1, "a": 2}`)}
	b := Caller{TenantID: "ta", ArgsKey: true, Args: []byte(`{"a":2,"b":1}`)}
	ka, bka, err := requestKeys(a, "erp.create_sales_order", "P", "R", "P", "R")
	if err != nil {
		t.Fatal(err)
	}
	kb, _, _ := requestKeys(b, "erp.create_sales_order", "P", "R", "P", "R")
	if ka != kb || ka == k1 {
		t.Error("the key of the mode no-bizkey must follow the canonical arguments, not the business key")
	}
	if len(bka) <= len(bk1) {
		t.Errorf("the stored business key %s must carry the key as a third part", bka)
	}
}
