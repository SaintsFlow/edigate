package contracts

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateListInbox(t *testing.T) {
	cases := []struct {
		name string
		args string
		ok   bool
	}{
		{"empty object", `{}`, true},
		{"empty arguments", ``, true},
		{"status and limit", `{"status":"received","limit":10}`, true},
		{"extra field", `{"limit":1,"extra":true}`, false},
		{"limit zero", `{"limit":0}`, false},
		{"limit 101", `{"limit":101}`, false},
		{"limit 100", `{"limit":100}`, true},
		{"bad status", `{"status":"deleted"}`, false},
		{"not an object", `[]`, false},
		{"not json", `{`, false},
	}
	for _, c := range cases {
		err := Validate("edi.list_inbox", []byte(c.args))
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", c.name, err)
		}
	}
}

func TestValidateOutputListInbox(t *testing.T) {
	if err := ValidateOutput("edi.list_inbox", []byte(`{"documents":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOutput("edi.list_inbox", []byte(`{"documents":[],"x":1}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("extra output field must fail, got %v", err)
	}
	if err := ValidateOutput("edi.list_inbox", []byte(`{}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing documents must fail, got %v", err)
	}
}

func TestValidateReadDocumentInput(t *testing.T) {
	const id = `"5b0cf1c2-0f3e-4c3e-9a55-3f0c6f7a1b11"`
	cases := []struct {
		name string
		args string
		ok   bool
	}{
		{"uuid", `{"document_id":` + id + `}`, true},
		{"no id", `{}`, false},
		{"not a uuid", `{"document_id":"abc"}`, false},
		{"extra field", `{"document_id":` + id + `,"x":1}`, false},
		{"id is a number", `{"document_id":5}`, false},
	}
	for _, c := range cases {
		err := Validate("edi.read_document", []byte(c.args))
		if c.ok != (err == nil) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

func TestValidateReadDocumentOutput(t *testing.T) {
	const line = `{"line_no":"1","qty":10,"unit":"EA","unit_price":5.25,"sku":"A1","sku_qualifier":"UP"}`
	const h850 = `{"partner_id":"ZZ:P","business_ref":"PO1","purpose":"00","order_type":"SA","order_date":"2026-10-18","currency":null,"ship_to":null,"buyer":null}`
	const h810 = `{"partner_id":"ZZ:P","business_ref":"INV1","invoice_date":"2026-10-18","order_ref":null,"currency":"USD","total_amount":52.50}`
	doc := func(st01, header, lines, extra string) string {
		return `{"id":"x","st01":"` + st01 + `","status":"received","header":` + header + `,"lines":[` + lines + `],"notes":[]` + extra + `}`
	}
	good := []string{doc("850", h850, line, ""), doc("810", h810, line, ""), doc("850", h850, "", "")}
	for _, g := range good {
		if err := ValidateOutput("edi.read_document", []byte(g)); err != nil {
			t.Errorf("valid output rejected: %v\n%s", err, g)
		}
	}
	bad := map[string]string{
		"extra top-level field":       doc("850", h850, line, `,"raw":"x"`),
		"extra header field":          doc("850", strings.Replace(h850, `"buyer":null`, `"buyer":null,"note":"x"`, 1), line, ""),
		"extra line field":            doc("850", h850, strings.Replace(line, `"sku":"A1"`, `"sku":"A1","text":"x"`, 1), ""),
		"a 997":                       doc("997", h850, line, ""),
		"a rejected status":           strings.Replace(doc("850", h850, line, ""), "received", "rejected", 1),
		"a note with text":            strings.Replace(doc("850", h850, line, ""), `"notes":[]`, `"notes":[{"ref":"$note_1","segment_path":"PO1[1]/PID[1]","text":"x"}]`, 1),
		"810 header with a 850 field": doc("810", strings.Replace(h810, `"currency":"USD"`, `"currency":"USD","buyer":null`, 1), line, ""),
	}
	for name, b := range bad {
		if err := ValidateOutput("edi.read_document", []byte(b)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}
