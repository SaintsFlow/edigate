package contracts

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestValidateCreateSalesOrder(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	cases := []struct {
		name, raw string
		ok        bool
	}{
		{"document only", `{"source_document_id":"` + id + `"}`, true},
		{"with an instruction", `{"source_document_id":"` + id + `","delivery_instruction":"liftgate_required"}`, true},
		{"no document", `{}`, false},
		{"free text instruction", `{"source_document_id":"` + id + `","delivery_instruction":"leave at the gate"}`, false},
		{"note reference instruction", `{"source_document_id":"` + id + `","delivery_instruction":"$note_1"}`, false},
		{"note reference document", `{"source_document_id":"$note_1"}`, false},
		{"a price", `{"source_document_id":"` + id + `","unit_price":"1.00"}`, false},
		{"a partner", `{"source_document_id":"` + id + `","partner_id":"ZZ:X"}`, false},
	}
	for _, c := range cases {
		if err := Validate("erp.create_sales_order", []byte(c.raw)); c.ok != (err == nil) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

// The enum of delivery_instruction lives in two schema files (the tool input and
// the note the quarantined reader fills); they must stay the same.
func TestDeliveryInstructionEnumsAgree(t *testing.T) {
	enumOf := func(path string, pick func(map[string]any) map[string]any) []any {
		raw, err := files.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var s map[string]any
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		return pick(s)["enum"].([]any)
	}
	prop := func(name string) func(map[string]any) map[string]any {
		return func(s map[string]any) map[string]any { return s["properties"].(map[string]any)[name].(map[string]any) }
	}
	tool := enumOf("schemas/erp.create_sales_order.in.json", prop("delivery_instruction"))
	note := enumOf("notes/delivery_instruction.json", prop("instruction"))
	if !reflect.DeepEqual(tool, note) {
		t.Fatalf("tool enum %v differs from the note enum %v", tool, note)
	}
}

func TestValidateTxnStatus(t *testing.T) {
	if err := Validate("txn.status", []byte(`{"txn_id":"11111111-1111-4111-8111-111111111111"}`)); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{`{}`, `{"txn_id":"nope"}`, `{"txn_id":"11111111-1111-4111-8111-111111111111","x":1}`} {
		if err := Validate("txn.status", []byte(bad)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", bad, err)
		}
	}
	if err := ValidateOutput("txn.status", []byte(`{"txn_id":"x","status":"completed","external_ref":"7"}`)); err != nil {
		t.Error(err)
	}
	if err := ValidateOutput("txn.status", []byte(`{"txn_id":"x","status":"accepted","external_ref":null}`)); err != nil {
		t.Error(err)
	}
	if err := ValidateOutput("txn.status", []byte(`{"txn_id":"x","status":"accepted","external_ref":null,"key":"ab"}`)); err == nil {
		t.Error("an extra field must be refused")
	}
}

func TestValidateOrderOutput(t *testing.T) {
	for _, ok := range []string{`{"status":"accepted","txn_id":"x"}`, `{"status":"rejected","txn_id":"x","reason":"dead"}`} {
		if err := ValidateOutput("erp.create_sales_order", []byte(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{`{"status":"accepted"}`, `{"status":"pending_approval","txn_id":"x"}`, `{"status":"accepted","txn_id":"x","key":"ab"}`} {
		if err := ValidateOutput("erp.create_sales_order", []byte(bad)); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}
