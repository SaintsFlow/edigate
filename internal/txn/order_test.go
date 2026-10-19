package txn

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/SaintsFlow/edigate/internal/edi/document"
	"github.com/SaintsFlow/edigate/internal/store"
)

func str(s string) *string { return &s }

func num(s string) *json.Number { n := json.Number(s); return &n }

var testPrices = map[string]store.Price{
	"SKU-1": {UnitPrice: "1.50", Currency: "USD", Unit: "EA"},
	"SKU-2": {UnitPrice: "0.33", Currency: "USD", Unit: "EA"},
	"EUR-1": {UnitPrice: "9.00", Currency: "EUR", Unit: "EA"},
}

func TestBuildOrderPricesFromTheListAndSumsExactly(t *testing.T) {
	lines := []document.Line{
		{SKU: str("SKU-1"), Qty: num("10"), Unit: str("EA"), UnitPrice: num("0.01")}, // the price of the 850 is ignored
		{SKU: str("SKU-2"), Qty: num("2.5")},
	}
	h := document.Header850{ShipTo: str("DC01")}
	got, err := BuildOrder(h, lines, "ZZ:P", "PO-1", "none", testPrices)
	if err != nil {
		t.Fatal(err)
	}
	want := OrderPayload{
		PartnerID: "ZZ:P", BusinessRef: "PO-1", ShipTo: "DC01", Currency: "USD", DeliveryInstruction: "none",
		Lines: []OrderLine{
			{SKU: "SKU-1", Qty: "10.000", Unit: "EA", UnitPrice: "1.50"},
			{SKU: "SKU-2", Qty: "2.500", Unit: "EA", UnitPrice: "0.33"},
		},
		Total: "15.83", // 15.00 + 0.825 rounded half up
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload = %+v\nwant      %+v", got, want)
	}
}

func TestBuildOrderRefusals(t *testing.T) {
	good := document.Line{SKU: str("SKU-1"), Qty: num("1")}
	cases := map[string]struct {
		lines []document.Line
		want  []string
	}{
		"no lines":           {nil, []string{"no_lines"}},
		"unknown sku":        {[]document.Line{{SKU: str("NOPE"), Qty: num("1")}}, []string{"unknown_sku"}},
		"note reference sku": {[]document.Line{{SKU: str("$note_3"), Qty: num("1")}}, []string{"unknown_sku"}},
		"no sku":             {[]document.Line{{Qty: num("1")}}, []string{"unknown_sku"}},
		"zero quantity":      {[]document.Line{{SKU: str("SKU-1"), Qty: num("0")}}, []string{"invalid_quantity"}},
		"no quantity":        {[]document.Line{{SKU: str("SKU-1")}}, []string{"invalid_quantity"}},
		"four decimals":      {[]document.Line{{SKU: str("SKU-1"), Qty: num("1.2345")}}, []string{"invalid_quantity"}},
		"negative quantity":  {[]document.Line{{SKU: str("SKU-1"), Qty: num("-2")}}, []string{"invalid_quantity"}},
		"two currencies":     {[]document.Line{good, {SKU: str("EUR-1"), Qty: num("1")}}, []string{"mixed_currency"}},
		"two reasons":        {[]document.Line{{SKU: str("NOPE"), Qty: num("1")}, {SKU: str("SKU-1"), Qty: num("0")}}, []string{"invalid_quantity", "unknown_sku"}},
	}
	for name, c := range cases {
		_, err := BuildOrder(document.Header850{}, c.lines, "ZZ:P", "PO-1", "", testPrices)
		var deny *DenyError
		if !errors.As(err, &deny) || !reflect.DeepEqual(deny.Explain, c.want) {
			t.Errorf("%s: err = %v, want explain %v", name, err, c.want)
		}
	}
}

func TestBuildOrderShipToNoteReferenceIsDropped(t *testing.T) {
	got, err := BuildOrder(document.Header850{ShipTo: str("$note_2")}, []document.Line{{SKU: str("SKU-1"), Qty: num("1")}}, "ZZ:P", "PO-1", "", testPrices)
	if err != nil || got.ShipTo != "" {
		t.Fatalf("ship_to = %q (%v)", got.ShipTo, err)
	}
}

func TestSKUsSkipsReferencesAndEmptyLines(t *testing.T) {
	got := SKUs([]document.Line{{SKU: str("A")}, {SKU: str("$note_1")}, {}, {SKU: str("B")}})
	if !reflect.DeepEqual(got, []string{"A", "B"}) {
		t.Fatalf("SKUs = %v", got)
	}
}

func TestMoneyHelpers(t *testing.T) {
	for in, want := range map[string]string{"0": "0.00", "5": "5.00", "5.25": "5.25", "0.05": "0.05", "123456789.10": "123456789.10"} {
		n, ok := parseScaled(in, 2)
		if !ok || formatScaled(n, 2) != want {
			t.Errorf("%s -> %v %v, want %s", in, n, ok, want)
		}
	}
	for _, bad := range []string{"", ".5", "-1", "1e3", "1.234", "a", "1..2"} {
		if _, ok := parseScaled(bad, 2); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}
