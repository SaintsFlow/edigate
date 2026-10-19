package taint

import (
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

func TestCheckSink(t *testing.T) {
	cases := []struct {
		name, tool, args string
		refused          bool
	}{
		{"a bare reference", "erp.create_sales_order", `{"customer":"$note_1"}`, true},
		{"a reference inside a sentence", "notify.send_email", `{"body":"see $note_12 please"}`, true},
		{"nested object and array", "wms.reserve", `{"a":{"b":[1,{"c":"x $note_7"}]}}`, true},
		{"an object key", "edi.send_asn", `{"$note_3":"x"}`, true},
		{"an escaped dollar sign", "erp.confirm_order", `{"a":"$note_4"}`, true},
		{"every up. tool", "up.anything", `{"a":"$note_1"}`, true},
		{"post_vendor_invoice", "erp.post_vendor_invoice", `{"a":"$note_1"}`, true},
		{"a value that is not a reference", "erp.create_sales_order", `{"a":"price $5","b":"note_1","c":"$note_x","d":5}`, false},
		{"an extracted value", "erp.create_sales_order", `{"instruction":"call_before_delivery","h":180}`, false},
		{"a tool that is not a sink", "edi.read_document", `{"document_id":"$note_1"}`, false},
		{"no arguments", "erp.create_sales_order", ``, false},
	}
	for _, c := range cases {
		err := CheckSink(c.tool, []byte(c.args))
		if c.refused != (err != nil) {
			t.Errorf("%s: err = %v", c.name, err)
		}
		if err != nil {
			var wire *jsonrpc.Error
			if !errors.As(err, &wire) || wire.Code != -32010 {
				t.Errorf("%s: not a tainted_sink error: %v", c.name, err)
			}
		}
	}
}

func TestIsSink(t *testing.T) {
	for _, tool := range []string{"erp.create_sales_order", "erp.confirm_order", "wms.reserve", "edi.send_asn",
		"erp.post_vendor_invoice", "notify.send_email", "up.erp.create_sales_order"} {
		if !IsSink(tool) {
			t.Errorf("%s must be a sink", tool)
		}
	}
	for _, tool := range []string{"edi.list_inbox", "edi.read_document", "edi.extract_note", "upload", "erp.read_order"} {
		if IsSink(tool) {
			t.Errorf("%s must not be a sink", tool)
		}
	}
}
