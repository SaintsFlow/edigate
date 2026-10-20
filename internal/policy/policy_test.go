package policy

import (
	"context"
	"reflect"
	"testing"
)

func TestCents(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "12": 1200, "12.5": 1250, "12.50": 1250, "0.01": 1, "5000.00": 500000} {
		if got, ok := Cents(in); !ok || got != want {
			t.Errorf("Cents(%q) = %d, %v, want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", ".5", "1.234", "-1", "1e3", "a", "1,5"} {
		if _, ok := Cents(bad); ok {
			t.Errorf("Cents(%q) must fail", bad)
		}
	}
	if got, ok := Cents("99999999999999999.99"); !ok || got != maxCents {
		t.Errorf("a huge amount is cut to %d, got %d %v", maxCents, got, ok)
	}
}

func TestInvoiceLinesMergesAndSorts(t *testing.T) {
	got := InvoiceLines([]NormLine{{"B", 1000, 200}, {"A", 2000, 100}, {"A", 3000, 100}, {"A", 1000, 150}})
	want := [][]string{{"A", "1000", "150"}, {"A", "5000", "100"}, {"B", "1000", "200"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("InvoiceLines = %v, want %v", got, want)
	}
}

// The embedded policy answers; a tool that is not in the table is refused.
func TestDecideUnknownTool(t *testing.T) {
	e, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.Decide(context.Background(), Principal{ID: "a", Kind: "agent", TenantID: "t"},
		ToolCall{Tool: "erp.delete_everything", Effect: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allow || d.Tier != -1 || !reflect.DeepEqual(d.Deny, []string{"partner.unknown", "tool.unknown"}) || d.Approval != ApprovalNone {
		t.Errorf("decision = %+v", d)
	}
}
