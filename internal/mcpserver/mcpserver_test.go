package mcpserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/edge"
	"github.com/SaintsFlow/edigate/internal/mcpserver"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/store/storetest"
)

type bearer struct {
	token string
	trace *string // last Edigate-Trace-Id seen in a response
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil && resp.Header.Get("Edigate-Trace-Id") != "" {
		*b.trace = resp.Header.Get("Edigate-Trace-Id")
	}
	return resp, err
}

// End to end: token -> edge -> SDK -> tool -> audit row, with a real database.
func TestListInboxEndToEnd(t *testing.T) {
	st, pool := storetest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := st.AddTenant(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatal(err)
	}
	token, sum, err := store.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.IssuePrincipal(ctx, store.Principal{ID: "agent-a", TenantID: "tenant-a", Kind: "agent"}, sum); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := mcpserver.New(st, log)
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	ts := httptest.NewServer(edge.New(ctx, st, sdk, log))
	defer ts.Close()

	var trace string
	transport := &mcp.StreamableClientTransport{Endpoint: ts.URL, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: bearer{token: token, trace: &trace}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "edi.list_inbox", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(res.StructuredContent)
	if string(got) != `{"documents":[]}` {
		t.Fatalf("structuredContent = %s", got)
	}
	if len(res.Content) != 1 {
		t.Fatalf("want one text content, got %d", len(res.Content))
	}
	if text, ok := res.Content[0].(*mcp.TextContent); !ok || text.Text != `{"documents":[]}` {
		t.Fatalf("text content = %#v", res.Content[0])
	}

	var tenant, principal, outcome, auditTrace string
	err = pool.QueryRow(ctx,
		`SELECT tenant_id, principal_id, outcome, trace_id FROM audit_log WHERE tool = 'edi.list_inbox' AND outcome = 'ok'`).
		Scan(&tenant, &principal, &outcome, &auditTrace)
	if err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if tenant != "tenant-a" || principal != "agent-a" || auditTrace != trace {
		t.Errorf("audit row = %s %s trace %s, response trace %s", tenant, principal, auditTrace, trace)
	}

	// A bad input is a schema_invalid error and is audited too.
	_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "edi.list_inbox", Arguments: map[string]any{"limit": 1000}})
	wire, ok := asWire(err)
	if !ok || wire.Code != -32602 {
		t.Fatalf("want JSON-RPC -32602, got %v", err)
	}
	var data struct{ Reason string }
	_ = json.Unmarshal(wire.Data, &data)
	if data.Reason != "schema_invalid" {
		t.Errorf("reason = %q", data.Reason)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE outcome = 'schema_invalid' AND tenant_id = 'tenant-a'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("schema_invalid audit rows = %d (%v)", n, err)
	}
}

func asWire(err error) (*jsonrpc.Error, bool) {
	var w *jsonrpc.Error
	return w, errors.As(err, &w)
}
