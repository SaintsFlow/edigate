package document

import (
	"os"
	"path/filepath"
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

func TestBuildAck(t *testing.T) {
	body, err := BuildAck(firstSet(t, "997_partial.edi"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"group_code":"PO","group_control":"1","sets":[` +
		`{"set_id":"850","control":"0001","ack":"A","errors":[]},` +
		`{"set_id":"850","control":"0002","ack":"R","errors":[]}],` +
		`"group_ack":"P","included":"2","received":"2","accepted":"1"}`
	if string(body) != want {
		t.Fatalf("body %s", body)
	}
}

func TestDecode(t *testing.T) {
	raw := []byte(`{"header":{"partner_id":"ZZ:P","business_ref":"PO1","purpose":"00","order_type":"SA",` +
		`"order_date":"2026-10-18","currency":null,"ship_to":null,"buyer":null,"secret":"x"},` +
		`"lines":[{"line_no":"1","qty":1,"unit":"EA","unit_price":12.50,"sku":null,"sku_qualifier":null}]}`)
	header, lines, err := Decode("850", raw)
	if err != nil {
		t.Fatal(err)
	}
	h, ok := header.(Header850)
	if !ok || h.BusinessRef != "PO1" || len(lines) != 1 || lines[0].UnitPrice == nil || string(*lines[0].UnitPrice) != "12.50" {
		t.Fatalf("decoded: %+v %+v", header, lines)
	}
	if h.References == nil {
		t.Error("a body without references must decode to an empty list")
	}
	if _, _, err := Decode("997", raw); err == nil {
		t.Error("a 997 body must not be readable by the agent path")
	}
}

func TestNumberText(t *testing.T) {
	cases := map[string]string{"10": "10", "007": "7", "0.50": "0.50", ".5": "0.5", "7.": "7", "-3": "-3", "-0": "0", "-0.00": "0.00", "-.5": "-0.5"}
	for in, want := range cases {
		if got := Decimal(in); got != want {
			t.Errorf("Decimal(%q) = %q, want %q", in, got, want)
		}
	}
	implied := map[string]string{"5250": "52.50", "5": "0.05", "0": "0.00", "-5250": "-52.50", "000100": "1.00", "100": "1.00"}
	for in, want := range implied {
		if got := ImpliedDecimal(in, 2); got != want {
			t.Errorf("ImpliedDecimal(%q) = %q, want %q", in, got, want)
		}
	}
}
