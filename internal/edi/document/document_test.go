package document

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SaintsFlow/edigate/internal/edi/x12"
)

func firstSet(t *testing.T, file string) *x12.Set {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "x12", "testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	ic, err := x12.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &ic.Groups[0].Sets[0]
}

func TestBuild850(t *testing.T) {
	built, err := Build(firstSet(t, "850_full.edi"), "ZZ:ZZTESTBUYER")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"header":{"partner_id":"ZZ:ZZTESTBUYER","business_ref":"PO20002","purpose":"00","order_type":"SA",` +
		`"order_date":"2026-10-18","currency":"USD","ship_to":"DC01","buyer":"B001"},` +
		`"lines":[` +
		`{"line_no":"1","qty":10,"unit":"EA","unit_price":5.25,"sku":"012345678905","sku_qualifier":"UP"},` +
		`{"line_no":"2","qty":5,"unit":"EA","unit_price":12.50,"sku":"012345678912","sku_qualifier":"UP"},` +
		`{"line_no":"3","qty":2,"unit":"CA","unit_price":100,"sku":"TESTSKU3","sku_qualifier":"VN"}]}`
	if string(built.Body) != want {
		t.Fatalf("body:\n%s\nwant:\n%s", built.Body, want)
	}
	if built.BusinessRef != "PO20002" {
		t.Fatalf("business ref %q", built.BusinessRef)
	}
	// No free text of the file may be in the body.
	for _, text := range []string{"WIDGET", "PACK IN BOXES", "TEST NOTE", "TEST BUYER", "DOCK 4", "buyer@example", "SHIPREF", "LINEREF"} {
		if strings.Contains(string(built.Body), text) {
			t.Errorf("free text %q reached the body", text)
		}
	}
}

func TestBuild810(t *testing.T) {
	built, err := Build(firstSet(t, "810_full.edi"), "ZZ:ZZTESTBUYER")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"header":{"partner_id":"ZZ:ZZTESTBUYER","business_ref":"INV0001","invoice_date":"2026-10-18",` +
		`"order_ref":"PO12345","currency":"USD","total_amount":115.00},` +
		`"lines":[` +
		`{"line_no":"1","qty":10,"unit":"EA","unit_price":5.25,"sku":"012345678905","sku_qualifier":"UP"},` +
		`{"line_no":"2","qty":5,"unit":"EA","unit_price":12.50,"sku":"012345678912","sku_qualifier":"UP"}]}`
	if string(built.Body) != want {
		t.Fatalf("body:\n%s\nwant:\n%s", built.Body, want)
	}
	for _, text := range []string{"WIDGET", "NOTE ON THE INVOICE", "TEST ACCOUNTS", "ar@example", "INVREF"} {
		if strings.Contains(string(built.Body), text) {
			t.Errorf("free text %q reached the body", text)
		}
	}
}

func TestBuild810TotalWithTwoImpliedDecimals(t *testing.T) {
	built, err := Build(firstSet(t, "810_min.edi"), "ZZ:ZZTESTBUYER")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(built.Body), `"total_amount":52.50`) { // TDS*5250
		t.Fatalf("body: %s", built.Body)
	}
}

func TestBuild997(t *testing.T) {
	built, err := Build(firstSet(t, "997_partial.edi"), "ZZ:ZZTESTBUYER")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"group_code":"PO","group_control":"1","sets":[` +
		`{"set_id":"850","control":"0001","ack":"A","errors":[]},` +
		`{"set_id":"850","control":"0002","ack":"R","errors":[]}],` +
		`"group_ack":"P","included":"2","received":"2","accepted":"1"}`
	if string(built.Body) != want || built.BusinessRef != "" {
		t.Fatalf("body %s ref %q", built.Body, built.BusinessRef)
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	built, _ := Build(firstSet(t, "850_full.edi"), "ZZ:ZZTESTBUYER")
	header, lines, err := Decode("850", built.Body)
	if err != nil {
		t.Fatal(err)
	}
	h, ok := header.(Header850)
	if !ok || h.BusinessRef != "PO20002" || len(lines) != 3 || lines[1].UnitPrice == nil || string(*lines[1].UnitPrice) != "12.50" {
		t.Fatalf("decoded: %+v %+v", header, lines)
	}
	if _, _, err := Decode("997", built.Body); err == nil {
		t.Error("a 997 body must not be readable by the agent path")
	}
}

func TestNumberText(t *testing.T) {
	cases := map[string]string{"10": "10", "007": "7", "0.50": "0.50", ".5": "0.5", "7.": "7", "-3": "-3", "-0": "0", "-0.00": "0.00", "-.5": "-0.5"}
	for in, want := range cases {
		if got := decimal(in); got != want {
			t.Errorf("decimal(%q) = %q, want %q", in, got, want)
		}
	}
	implied := map[string]string{"5250": "52.50", "5": "0.05", "0": "0.00", "-5250": "-52.50", "000100": "1.00", "100": "1.00"}
	for in, want := range implied {
		if got := impliedDecimal(in, 2); got != want {
			t.Errorf("impliedDecimal(%q) = %q, want %q", in, got, want)
		}
	}
}
