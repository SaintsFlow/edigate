package edge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/store"
)

// fakeFinder is a principal lookup that counts its calls.
type fakeFinder struct {
	calls atomic.Int64
	known map[string]store.Principal // key: string(token sha256)
}

func (f *fakeFinder) FindPrincipal(_ context.Context, sum []byte) (store.Principal, error) {
	f.calls.Add(1)
	if p, ok := f.known[string(sum)]; ok {
		return p, nil
	}
	return store.Principal{}, store.ErrNotFound
}

type rig struct {
	finder   *fakeFinder
	server   http.Handler
	token    string // valid agent token
	operator string // valid operator token
	seen     atomic.Int64
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &rig{finder: &fakeFinder{known: map[string]store.Principal{}}}
	r.token = r.add("agent-1", "agent")
	r.operator = r.add("op-1", "operator")
	next := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.seen.Add(1)
		// The SDK would put this on RequestExtra.TokenInfo; here we read the same context value.
		body, _ := io.ReadAll(req.Body) // the body must still be readable
		if len(body) == 0 {
			t.Error("body was not returned to the request")
		}
		w.WriteHeader(http.StatusOK)
	})
	r.server = NewGate(ctx, r.finder, slog.New(slog.NewTextHandler(io.Discard, nil))).MCP(next)
	return r
}

// add registers a principal and returns its freshly generated token.
func (r *rig) add(id, kind string) string {
	token, sum, err := store.NewToken()
	if err != nil {
		panic(err)
	}
	r.finder.known[string(sum)] = store.Principal{ID: id, TenantID: "t1", Kind: kind}
	return token
}

const callBody = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"edi.list_inbox","arguments":{}}}`

type reqOpts struct {
	addr    string
	token   string
	headers map[string]string
	body    string
}

func (r *rig) do(o reqOpts) *httptest.ResponseRecorder {
	if o.body == "" {
		o.body = callBody
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(o.body))
	if o.addr == "" {
		o.addr = "192.0.2.1:4000"
	}
	req.RemoteAddr = o.addr
	if o.token != "" {
		req.Header.Set("Authorization", "Bearer "+o.token)
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.server.ServeHTTP(rec, req)
	return rec
}

type errBody struct {
	ID    json.RawMessage `json:"id"`
	Error struct {
		Code int `json:"code"`
		Data struct {
			Reason string `json:"reason"`
		} `json:"data"`
	} `json:"error"`
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) errBody {
	t.Helper()
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("response is not JSON-RPC: %q", rec.Body.String())
	}
	return e
}

func TestT_EDGE_01_NoToken(t *testing.T) {
	r := newRig(t)
	rec := r.do(reqOpts{})
	e := decode(t, rec)
	if rec.Code != 401 || e.Error.Code != -32030 || e.Error.Data.Reason != "unauthorized" {
		t.Fatalf("got %d %+v", rec.Code, e)
	}
	if rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Error("missing WWW-Authenticate: Bearer")
	}
	if rec.Header().Get("Edigate-Trace-Id") == "" {
		t.Error("missing trace id header")
	}
	if r.seen.Load() != 0 {
		t.Error("request reached the next handler")
	}
}

func TestT_EDGE_01_RevokedOrUnknownToken(t *testing.T) {
	r := newRig(t)
	revoked, _, _ := store.NewToken() // not in the finder: a revoked token looks the same
	rec := r.do(reqOpts{token: revoked})
	e := decode(t, rec)
	if rec.Code != 401 || e.Error.Data.Reason != "unauthorized" {
		t.Fatalf("got %d %+v", rec.Code, e)
	}
}

func TestT_EDGE_01_OperatorForbidden(t *testing.T) {
	r := newRig(t)
	rec := r.do(reqOpts{token: r.operator})
	e := decode(t, rec)
	if rec.Code != 403 || e.Error.Code != -32031 || e.Error.Data.Reason != "forbidden" {
		t.Fatalf("got %d %+v", rec.Code, e)
	}
}

func TestT_EDGE_01_HeaderMismatch(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"Mcp-Name differs", map[string]string{"Mcp-Method": "tools/call", "Mcp-Name": "edi.read_document"}},
		{"Mcp-Method differs", map[string]string{"Mcp-Method": "tools/list"}},
		{"new version without Mcp-Name", map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call"}},
		{"new version without both", map[string]string{"Mcp-Protocol-Version": "2026-07-28"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			rec := r.do(reqOpts{token: r.token, headers: c.headers})
			e := decode(t, rec)
			if rec.Code != 400 || e.Error.Code != -32020 || e.Error.Data.Reason != "schema_invalid" {
				t.Fatalf("got %d %+v", rec.Code, e)
			}
			if string(e.ID) != "7" {
				t.Errorf("error id = %s, want 7", e.ID)
			}
			if r.seen.Load() != 0 {
				t.Error("request reached the next handler")
			}
		})
	}
}

func TestT_EDGE_01_MatchingHeadersPass(t *testing.T) {
	r := newRig(t)
	rec := r.do(reqOpts{token: r.token, headers: map[string]string{
		"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": "edi.list_inbox",
	}})
	if rec.Code != 200 || r.seen.Load() != 1 {
		t.Fatalf("got %d, next calls %d", rec.Code, r.seen.Load())
	}
}

func TestT_EDGE_01_Batch(t *testing.T) {
	r := newRig(t)
	rec := r.do(reqOpts{token: r.token, body: `[` + callBody + `]`})
	e := decode(t, rec)
	if rec.Code != 400 || e.Error.Code != -32602 || e.Error.Data.Reason != "schema_invalid" {
		t.Fatalf("got %d %+v", rec.Code, e)
	}
}

func TestT_EDGE_01_BodyTooLarge(t *testing.T) {
	r := newRig(t)
	big := `{"jsonrpc":"2.0","id":1,"method":"x","params":{"pad":"` + strings.Repeat("a", maxBody) + `"}}`
	rec := r.do(reqOpts{token: r.token, body: big})
	e := decode(t, rec)
	if rec.Code != 413 || e.Error.Data.Reason != "schema_invalid" {
		t.Fatalf("got %d %+v", rec.Code, e)
	}
}

func TestT_EDGE_01_IdentityReachesSDK(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	token, sum, _ := store.NewToken()
	finder := &fakeFinder{known: map[string]store.Principal{string(sum): {ID: "agent-1", TenantID: "t1", Kind: "agent"}}}
	var got Identity
	var ok bool
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	server.AddTool(&mcp.Tool{Name: "ping", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			got, ok = IdentityFrom(req.Extra.TokenInfo)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	ts := httptest.NewServer(NewGate(ctx, finder, slog.New(slog.NewTextHandler(io.Discard, nil))).MCP(sdk))
	defer ts.Close()

	transport := &mcp.StreamableClientTransport{Endpoint: ts.URL, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ping", Arguments: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if !ok || got.PrincipalID != "agent-1" || got.TenantID != "t1" || len(got.TraceID) != 32 {
		t.Fatalf("handler saw %+v ok=%v", got, ok)
	}
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// T-RATE-02: six failed checks from one address in a second: the sixth and seventh get 429
// and the principal lookup (the database) is not called for them.
func TestT_RATE_02_AddressBucket(t *testing.T) {
	r := newRig(t)
	bad, _, _ := store.NewToken()
	for i := 1; i <= 5; i++ {
		if rec := r.do(reqOpts{token: bad}); rec.Code != 401 {
			t.Fatalf("attempt %d: got %d, want 401", i, rec.Code)
		}
	}
	before := r.finder.calls.Load()
	for i := 6; i <= 7; i++ {
		rec := r.do(reqOpts{token: bad})
		e := decode(t, rec)
		if rec.Code != 429 || e.Error.Code != -32029 || e.Error.Data.Reason != "rate_limited" {
			t.Fatalf("attempt %d: got %d %+v", i, rec.Code, e)
		}
	}
	if r.finder.calls.Load() != before {
		t.Error("principal lookup was called for a rate-limited address")
	}
	// A valid token from the blocked address is also refused until the bucket refills.
	if rec := r.do(reqOpts{token: r.token}); rec.Code != 429 {
		t.Errorf("blocked address with valid token: got %d, want 429", rec.Code)
	}
	// Another address is not affected.
	if rec := r.do(reqOpts{token: r.token, addr: "192.0.2.99:1"}); rec.Code != 200 {
		t.Errorf("other address: got %d, want 200", rec.Code)
	}
}

// T-RATE-02: good checks do not spend the bucket of the address (agents behind one NAT).
func TestT_RATE_02_SuccessfulChecksDoNotSpendAddressBucket(t *testing.T) {
	r := newRig(t)
	// One hundred valid requests from one address, each from its own principal so
	// that the per-principal bucket is not the limit.
	for i := 0; i < 100; i++ {
		token := r.add(fmt.Sprintf("agent-n%d", i), "agent")
		if rec := r.do(reqOpts{token: token}); rec.Code != 200 {
			t.Fatalf("request %d: got %d, want 200", i+1, rec.Code)
		}
	}
	bad, _, _ := store.NewToken()
	for i := 1; i <= 5; i++ {
		if rec := r.do(reqOpts{token: bad}); rec.Code != 401 {
			t.Fatalf("after 20 good requests, bad attempt %d: got %d, want 401", i, rec.Code)
		}
	}
}

// T-RATE-01: a principal has 20 requests in a second; the 21st is rate_limited. Another
// principal is not slowed down by it.
func TestT_RATE_01_PrincipalBucket(t *testing.T) {
	r := newRig(t)
	started := time.Now()
	var codes [21]int
	var last errBody
	for i := range codes {
		rec := r.do(reqOpts{token: r.token})
		codes[i] = rec.Code
		if i == 20 {
			last = decode(t, rec)
		}
	}
	if time.Since(started) > 40*time.Millisecond {
		t.Skip("the machine is too slow for a test about one second")
	}
	for i := 0; i < 20; i++ {
		if codes[i] != 200 {
			t.Fatalf("request %d: got %d, want 200", i+1, codes[i])
		}
	}
	if codes[20] != 429 || last.Error.Code != -32029 || last.Error.Data.Reason != "rate_limited" {
		t.Fatalf("request 21: got %d %+v, want 429 rate_limited", codes[20], last)
	}
	other := r.add("agent-2", "agent")
	if rec := r.do(reqOpts{token: other}); rec.Code != 200 {
		t.Errorf("another principal: got %d, want 200", rec.Code)
	}
}
