package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/edge"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store/storetest"
)

// T-TAINT-01: a note reference in the arguments of a sink tool is refused after
// the schema check and before the service; the refusal is audited; the same
// arguments pass for a tool that is not a sink.
func TestT_TAINT_01_NoteReferenceNeverReachesASink(t *testing.T) {
	st, pool := storetest.New(t)
	ctx := context.Background()
	if err := st.AddTenant(ctx, "tenant-a", "Tenant A", ""); err != nil {
		t.Fatal(err)
	}
	s := newServer(st, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// The sinks of the later waves have no schema files yet: accept any object.
	s.validateIn = func(string, []byte) error { return nil }
	s.validateOut = func(string, []byte) error { return nil }

	var calls atomic.Int32
	svc := func(context.Context, edge.Identity, json.RawMessage) (any, error) {
		calls.Add(1)
		return map[string]string{"done": "yes"}, nil
	}
	call := func(tool, args string) error {
		req := &mcp.CallToolRequest{
			Params: &mcp.CallToolParamsRaw{Name: tool, Arguments: json.RawMessage(args)},
			Extra: &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{
				UserID: "agent-a", Extra: map[string]any{"tenant_id": "tenant-a", "kind": "agent", "trace_id": "trace-" + tool},
			}},
		}
		_, err := s.call(ctx, tool, svc, req)
		return err
	}

	const sink = "erp.create_sales_order"
	for name, args := range map[string]string{
		"a reference":                `{"customer":"$note_1"}`,
		"a nested object and string": `{"lines":[{"memo":"see $note_12"}]}`,
		"an object key":              `{"meta":{"$note_3":"x"}}`,
	} {
		err := call(sink, args)
		var wire *jsonrpc.Error
		if !errors.As(err, &wire) || wire.Code != -32010 {
			t.Errorf("%s: want tainted_sink (-32010), got %v", name, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("the service ran %d times for a refused call", n)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tool = $1 AND outcome = 'tainted_sink'`, sink).Scan(&n); err != nil || n != 3 {
		t.Fatalf("tainted_sink audit rows = %d (%v)", n, err)
	}

	if err := call(sink, `{"instruction":"call_before_delivery","height_cm":180}`); err != nil {
		t.Errorf("closed values must pass: %v", err)
	}
	if err := call("edi.read_document", `{"document_id":"$note_1"}`); err != nil {
		t.Errorf("a tool that is not a sink must accept the argument: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("the service ran %d times, want 2", n)
	}
	if err := call(sink, `{"x":"$note_1"}`); rpcerrReason(err) != string(rpcerr.TaintedSink) {
		t.Errorf("reason = %q", rpcerrReason(err))
	}
}

func rpcerrReason(err error) string {
	_, reason := rpcerr.From(err)
	return string(reason)
}
