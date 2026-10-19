// Package taint stops a reference to a note from reaching a tool that has an
// effect (control M3).
//
// Input: the name of a tool and its raw arguments, after the schema check.
// Output: nil, or the gateway's tainted_sink error (internal/rpcerr).
//
// A "sink" is a tool whose call changes something outside the gateway. The
// agent holds note references ($note_N) instead of partner text; a reference
// in the arguments of a sink means the agent is passing partner-controlled text
// toward an effect, so the call is refused. Values from edi.extract_note are
// closed values and are not references, so they pass.
package taint

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/SaintsFlow/edigate/internal/rpcerr"
)

// sinks are the tools with an effect. The tools arrive in waves 2-5 to 2-8;
// the list is here from the start so that none of them can be added without it.
var sinks = map[string]bool{
	"erp.create_sales_order":  true,
	"erp.confirm_order":       true,
	"wms.reserve":             true,
	"edi.send_asn":            true,
	"erp.post_vendor_invoice": true,
	"notify.send_email":       true,
}

// sinkPrefix marks every tool of the upstream gateway as a sink.
const sinkPrefix = "up."

// noteRef finds a note reference inside any string.
var noteRef = regexp.MustCompile(`\$note_[0-9]+`)

// IsSink reports whether the tool has an effect.
func IsSink(tool string) bool {
	return sinks[tool] || strings.HasPrefix(tool, sinkPrefix)
}

// CheckSink returns tainted_sink when tool is a sink and any string in args, at
// any depth and including object keys, holds something that looks like a note
// reference. For any other tool it does nothing.
func CheckSink(tool string, args json.RawMessage) error {
	if !IsSink(tool) || len(args) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return nil // the schema check has already refused anything that is not JSON
	}
	if hasRef(v) {
		return rpcerr.New(rpcerr.TaintedSink, "a note reference is not allowed in this tool")
	}
	return nil
}

func hasRef(v any) bool {
	switch x := v.(type) {
	case string:
		return noteRef.MatchString(x)
	case []any:
		for _, e := range x {
			if hasRef(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if noteRef.MatchString(k) || hasRef(e) {
				return true
			}
		}
	}
	return false
}
