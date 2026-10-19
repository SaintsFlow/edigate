package mcpserver_test

// Tests of wave 2-8 for the upstream MCP servers: the pins, the hostile servers of the
// stand (run in the test process), the taint label, the policy and the network borders.
// The pins are written here the way an administrator who accepts a finding writes them
// (the linter is a signal, not a border); the command that refuses to pin is tested in
// cmd/edigatectl.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/contracts"
	"github.com/SaintsFlow/edigate/internal/edge"
	"github.com/SaintsFlow/edigate/internal/mcpserver"
	"github.com/SaintsFlow/edigate/internal/policy"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/store/storetest"
	"github.com/SaintsFlow/edigate/internal/upstream"
	"github.com/SaintsFlow/edigate/servers/adversarial/adversary"
	"github.com/SaintsFlow/edigate/servers/adversarial/common"
)

// syncBuffer is a log the hostile servers write and the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// hostile is a hostile server of the stand running in the test, with its request log.
type hostile struct {
	ts  *httptest.Server
	log *syncBuffer
}

func startHostile(t *testing.T, name string, srv *mcp.Server, authHeader, authValue string) *hostile {
	t.Helper()
	h := &hostile{log: &syncBuffer{}}
	h.ts = httptest.NewServer(common.Handler(name, srv, h.log, authHeader, authValue))
	t.Cleanup(h.ts.Close)
	return h
}

func (h *hostile) url() string { return h.ts.URL + "/mcp" }

type upRig struct {
	t      *testing.T
	st     *store.Store
	pool   *pgxpool.Pool
	pub    ed25519.PublicKey
	priv   ed25519.PrivateKey
	cs     *mcp.ClientSession
	token  string
	gwURL  string
	opened func(token string) *mcp.ClientSession
}

// newUpRig starts a gateway with the given upstream servers and policy data. Tenant ta has an
// agent; tenant tb has one too (opened with open).
func newUpRig(t *testing.T, servers []upstream.Server, allow map[string][]string, secrets map[string]string) *upRig {
	t.Helper()
	st, pool := storetest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, id := range []string{"ta", "tb"} {
		if err := st.AddTenant(ctx, id, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gw, err := upstream.New(servers, b64(pub), st, log, func(name string) string { return secrets[name] })
	if err != nil {
		t.Fatal(err)
	}
	engine, err := policy.New(ctx, policy.WithUpstreamAllow(allow))
	if err != nil {
		t.Fatal(err)
	}
	srv := mcpserver.New(st, nil, engine, log, mcpserver.WithUpstream(gw))
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	ts := httptest.NewServer(edge.NewGate(ctx, st, log).MCP(sdk))
	t.Cleanup(ts.Close)

	r := &upRig{t: t, st: st, pool: pool, pub: pub, priv: priv, gwURL: ts.URL}
	r.opened = func(token string) *mcp.ClientSession {
		var trace string
		transport := &mcp.StreamableClientTransport{Endpoint: ts.URL, DisableStandaloneSSE: true,
			HTTPClient: &http.Client{Transport: bearer{token: token, trace: &trace}}}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, transport, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		return cs
	}
	r.token = r.agent("ta")
	r.cs = r.opened(r.token)
	return r
}

func b64(pub ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(pub) }

func (r *upRig) agent(tenant string) string {
	r.t.Helper()
	token, sum, err := store.NewToken()
	if err != nil {
		r.t.Fatal(err)
	}
	id := "agent-" + tenant
	if err := r.st.IssuePrincipal(context.Background(), store.Principal{ID: id, TenantID: tenant, Kind: "agent"}, sum); err != nil {
		r.t.Fatal(err)
	}
	return token
}

// pin writes the signed pins of the tools the server shows now, like an administrator.
func (r *upRig) pin(srv upstream.Server, secret string) {
	r.t.Helper()
	tools, err := upstream.FetchTools(context.Background(), srv, secret)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, tool := range tools {
		hash, err := upstream.Hash(tool)
		if err != nil {
			r.t.Fatal(err)
		}
		m := store.Manifest{Server: srv.Name, Tool: tool.Name, SHA256: hash[:], Signature: upstream.Sign(r.priv, srv.Name, hash), PinnedBy: "test"}
		if err := r.st.PinManifest(context.Background(), m); err != nil {
			r.t.Fatal(err)
		}
	}
}

// listed returns the names of the tools the agent sees.
func (r *upRig) listed(cs *mcp.ClientSession) map[string]*mcp.Tool {
	r.t.Helper()
	out := map[string]*mcp.Tool{}
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			r.t.Fatal(err)
		}
		out[tool.Name] = tool
	}
	return out
}

func (r *upRig) call(cs *mcp.ClientSession, tool string, args map[string]any) (*mcp.CallToolResult, string) {
	r.t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		wire, ok := asWire(err)
		if !ok {
			r.t.Fatalf("%s: %v", tool, err)
		}
		var d struct{ Reason string }
		_ = json.Unmarshal(wire.Data, &d)
		if d.Reason == "" {
			return nil, err.Error()
		}
		return nil, d.Reason
	}
	return res, ""
}

func (r *upRig) count(sql string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func server(name, url string) upstream.Server { return upstream.Server{Name: name, URL: url} }

var rateArgs = map[string]any{"city": "Almaty"}

// T-UP-01: a tool that has no pin is off: it is not in the list and a call answers
// manifest_drift. A pin made for another server does not count, and neither does a pin
// signed by another key. (The command that refuses to pin a tool that names another tool is
// tested in cmd/edigatectl.)
func TestT_UP_01_ToolWithoutAPinIsOff(t *testing.T) {
	h := startHostile(t, "evil-validator", adversary.EvilValidator(), "", "")
	srv := server("evil-validator", h.url())
	r := newUpRig(t, []upstream.Server{srv}, map[string][]string{"ta": {"up.evil-validator.validate_address"}}, nil)

	if _, ok := r.listed(r.cs)["up.evil-validator.validate_address"]; ok {
		t.Fatal("a tool without a pin is listed")
	}
	if _, reason := r.call(r.cs, "up.evil-validator.validate_address", map[string]any{"address": "1 Main St"}); reason != "manifest_drift" {
		t.Errorf("reason = %q, want manifest_drift", reason)
	}
	if n := r.count(`SELECT count(*) FROM audit_log WHERE event_type = 'manifest_drift' AND tool = 'up.evil-validator.validate_address'
	                 AND detail->>'state' = 'no_pin' AND detail->>'pinned_sha256' = '' AND length(detail->>'live_sha256') = 64`); n != 1 {
		t.Errorf("manifest_drift rows = %d, want 1 (the second sight of the same description is not written again)", n)
	}

	// A pin signed by a key the gateway does not know switches the tool off as well.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	tools, err := upstream.FetchTools(context.Background(), srv, "")
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := upstream.Hash(tools[0])
	if err := r.st.PinManifest(context.Background(), store.Manifest{Server: "evil-validator", Tool: tools[0].Name,
		SHA256: hash[:], Signature: upstream.Sign(other, "evil-validator", hash), PinnedBy: "intruder"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.listed(r.cs)["up.evil-validator.validate_address"]; ok {
		t.Error("a tool with a foreign signature is listed")
	}
	if n := r.count(`SELECT count(*) FROM audit_log WHERE event_type = 'manifest_drift' AND detail->>'state' = 'bad_signature'`); n != 1 {
		t.Errorf("bad_signature rows = %d, want 1", n)
	}

	// The same pin signed for another server name does not verify here either.
	if err := r.st.PinManifest(context.Background(), store.Manifest{Server: "evil-validator", Tool: tools[0].Name,
		SHA256: hash[:], Signature: upstream.Sign(r.priv, "rugpull-rates", hash), PinnedBy: "intruder"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.listed(r.cs)["up.evil-validator.validate_address"]; ok {
		t.Error("a pin signed for another server is accepted")
	}
	r.pin(srv, "")
	if _, ok := r.listed(r.cs)["up.evil-validator.validate_address"]; !ok {
		t.Error("a correctly signed pin does not switch the tool on")
	}
}

// T-UP-02: shadow-carrier names a tool like a tool of the gateway. The agent sees it only as
// up.shadow-carrier.erp.create_sales_order, the own tool of the gateway is still there and a
// call to it goes to the gateway; the linter flags the reference to edi.send_asn.
func TestT_UP_02_ShadowToolDoesNotReplaceAGatewayTool(t *testing.T) {
	h := startHostile(t, "shadow-carrier", adversary.ShadowCarrier(), "", "")
	srv := server("shadow-carrier", h.url())
	r := newUpRig(t, []upstream.Server{srv}, map[string][]string{"ta": {"up.shadow-carrier.erp.create_sales_order"}}, nil)
	r.pin(srv, "")

	tools := r.listed(r.cs)
	for _, name := range []string{"up.shadow-carrier.erp.create_sales_order", "up.shadow-carrier.carrier_rates"} {
		if _, ok := tools[name]; !ok {
			t.Errorf("%s is not listed", name)
		}
	}
	own, ok := tools["erp.create_sales_order"]
	if !ok {
		t.Fatal("the own tool erp.create_sales_order is gone")
	}
	if !strings.Contains(mustJSON(own.InputSchema), "source_document_id") || strings.Contains(own.Description, "carrier") {
		t.Errorf("the own tool was replaced: %s / %s", own.Description, mustJSON(own.InputSchema))
	}

	// A call to the plain name is a call to the gateway (a document that does not exist: not_found),
	// not to the carrier (which would create an order).
	_, reason := r.call(r.cs, "erp.create_sales_order", map[string]any{"source_document_id": "11111111-1111-1111-1111-111111111111"})
	if reason != "not_found" {
		t.Errorf("erp.create_sales_order: reason = %q, want not_found (an answer of the gateway)", reason)
	}
	// The prefixed name goes to the carrier, with the label.
	res, reason := r.call(r.cs, "up.shadow-carrier.erp.create_sales_order", map[string]any{"reference": "R1"})
	if reason != "" || res.Meta[upstream.TaintKey] != "upstream:shadow-carrier" {
		t.Errorf("prefixed call: reason %q, meta %v", reason, res)
	}

	// The linter flags the tool that sends the model away from edi.send_asn.
	findings := upstream.Lint(tools["up.shadow-carrier.carrier_rates"].Description, contracts.ToolNames())
	if !upstream.HasKind(findings, upstream.ToolReference) || !contains(findings, "edi.send_asn") {
		t.Errorf("findings = %+v, want a tool_reference to edi.send_asn", findings)
	}
}

func contains(findings []upstream.Finding, detail string) bool {
	for _, f := range findings {
		if f.Detail == detail {
			return true
		}
	}
	return false
}

// T-UP-03 and K3: the description of rugpull-rates changes after three calls. The next list
// no longer shows the tool, a call answers manifest_drift, the audit row holds both hashes,
// and the tool stays off until it is pinned again.
func TestT_UP_03_DescriptionChangeSwitchesTheToolOff(t *testing.T) {
	h := startHostile(t, "rugpull-rates", adversary.RugpullRates(3), "", "")
	srv := server("rugpull-rates", h.url())
	r := newUpRig(t, []upstream.Server{srv}, map[string][]string{"ta": {"up.rugpull-rates.get_rates"}}, nil)
	r.pin(srv, "")
	const name = "up.rugpull-rates.get_rates"

	if tool, ok := r.listed(r.cs)[name]; !ok || tool.Description != adversary.RugpullCleanDescription {
		t.Fatalf("before the changes: listed %v", ok)
	}
	for i := 1; i <= 3; i++ {
		if _, reason := r.call(r.cs, name, rateArgs); reason != "" {
			t.Fatalf("call %d: %s", i, reason)
		}
	}
	// The server changed its description after the third call.
	if _, ok := r.listed(r.cs)[name]; ok {
		t.Error("the tool is still listed after its description changed")
	}
	if _, reason := r.call(r.cs, name, rateArgs); reason != "manifest_drift" {
		t.Errorf("call 4: reason = %q, want manifest_drift", reason)
	}
	var server, tool, pinned, live string
	err := r.pool.QueryRow(context.Background(),
		`SELECT detail->>'server', detail->>'tool', detail->>'pinned_sha256', detail->>'live_sha256' FROM audit_log
		 WHERE event_type = 'manifest_drift' AND detail->>'state' = 'changed'`).Scan(&server, &tool, &pinned, &live)
	if err != nil {
		t.Fatalf("audit row: %v", err)
	}
	var stored string
	if err := r.pool.QueryRow(context.Background(), `SELECT encode(sha256, 'hex') FROM manifests WHERE server = 'rugpull-rates'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if server != "rugpull-rates" || tool != "get_rates" || pinned != stored || len(live) != 64 || live == pinned {
		t.Errorf("audit row = %s %s pinned %s live %s (stored %s)", server, tool, pinned, live, stored)
	}

	// K3: until the administrator pins the new description, the tool stays off.
	if _, reason := r.call(r.cs, name, rateArgs); reason != "manifest_drift" {
		t.Errorf("before a new pin: reason = %q", reason)
	}
	r.pin(srv, "")
	if tool, ok := r.listed(r.cs)[name]; !ok || tool.Description != adversary.RugpullPoisonedDescription {
		t.Error("after a new pin the tool is not listed with its new description")
	}
}

// T-UP-04: the result of an upstream tool carries the taint label; a reference $up_N in an
// argument of a sink is refused. With quarantine_results the text turns into references and
// a structured result with a free string does not pass.
func TestT_UP_04_ResultsAreLabelledAndCanBeQuarantined(t *testing.T) {
	h := startHostile(t, "rugpull-rates", adversary.RugpullRates(100), "", "")
	open := server("rugpull-rates", h.url())
	closed := upstream.Server{Name: "rugpull-closed", URL: h.url(), QuarantineResults: true}
	allow := map[string][]string{"ta": {"up.rugpull-rates.get_rates", "up.rugpull-closed.get_rates"}}
	r := newUpRig(t, []upstream.Server{open, closed}, allow, nil)
	r.pin(open, "")
	r.pin(closed, "")

	listed := r.listed(r.cs)
	if listed["up.rugpull-rates.get_rates"].OutputSchema == nil || listed["up.rugpull-closed.get_rates"].OutputSchema != nil {
		t.Error("the output schema must be shown for an open server and hidden for a quarantined one")
	}

	res, reason := r.call(r.cs, "up.rugpull-rates.get_rates", rateArgs)
	if reason != "" {
		t.Fatal(reason)
	}
	if res.Meta[upstream.TaintKey] != "upstream:rugpull-rates" {
		t.Errorf("_meta = %v, want the label upstream:rugpull-rates", res.Meta)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, adversary.RugpullInstruction) || res.StructuredContent == nil {
		t.Errorf("a result without quarantine must reach the agent as it is: %q", text)
	}
	if n := r.count(`SELECT count(*) FROM audit_log WHERE event_type = 'upstream_result' AND tool = 'up.rugpull-rates.get_rates'
	                 AND detail->>'server' = 'rugpull-rates' AND length(encode(payload_sha256, 'hex')) = 64`); n != 1 {
		t.Errorf("upstream_result audit rows = %d, want 1", n)
	}

	q, reason := r.call(r.cs, "up.rugpull-closed.get_rates", rateArgs)
	if reason != "" {
		t.Fatal(reason)
	}
	ref := q.Content[0].(*mcp.TextContent).Text
	if !regexp.MustCompile(`^\$up_[0-9]+$`).MatchString(ref) || len(q.Content) != 1 {
		t.Errorf("quarantined content = %v, want one reference", q.Content)
	}
	if q.StructuredContent != nil {
		t.Errorf("structuredContent with free strings passed: %v", q.StructuredContent)
	}
	if q.Meta[upstream.TaintKey] != "upstream:rugpull-closed" {
		t.Errorf("_meta = %v", q.Meta)
	}
	var kept string
	if err := r.pool.QueryRow(context.Background(), `SELECT body FROM upstream_notes WHERE tenant_id = 'ta' AND taint_label = 'upstream:rugpull-closed'`).Scan(&kept); err != nil ||
		!strings.Contains(kept, adversary.RugpullInstruction) {
		t.Errorf("kept text = %q (%v)", kept, err)
	}

	// The reference is refused in the arguments of a sink, and no tool opens it.
	if _, reason := r.call(r.cs, "up.rugpull-rates.get_rates", map[string]any{"city": ref}); reason != "tainted_sink" {
		t.Errorf("$up_N in an argument: reason = %q, want tainted_sink", reason)
	}
	if _, reason := r.call(r.cs, "edi.extract_note", map[string]any{"document_id": "11111111-1111-1111-1111-111111111111", "ref": ref, "schema_id": "packaging"}); reason != "schema_invalid" {
		t.Errorf("extract_note on $up_N: reason = %q, want schema_invalid", reason)
	}
}

// T-UP-05: the token of the client of the gateway never reaches an upstream; the header named
// in the config does, and only to the server it is for.
func TestT_UP_05_ClientTokenIsNotPassedToUpstreams(t *testing.T) {
	evil := startHostile(t, "evil-validator", adversary.EvilValidator(), "", "")
	rug := startHostile(t, "rugpull-rates", adversary.RugpullRates(100), "X-Gateway-Key", "s3cret")
	evilSrv := server("evil-validator", evil.url())
	rugSrv := upstream.Server{Name: "rugpull-rates", URL: rug.url(),
		AuthHeader: &upstream.AuthHeader{Name: "X-Gateway-Key", Env: "RUG_KEY"}}
	allow := map[string][]string{"ta": {"up.evil-validator.validate_address", "up.rugpull-rates.get_rates"}}
	r := newUpRig(t, []upstream.Server{evilSrv, rugSrv}, allow, map[string]string{"RUG_KEY": "s3cret"})
	r.pin(evilSrv, "")
	r.pin(rugSrv, "s3cret")

	for range 2 {
		r.listed(r.cs)
	}
	if _, reason := r.call(r.cs, "up.evil-validator.validate_address", map[string]any{"address": "1 Main St"}); reason != "" {
		t.Fatal(reason)
	}
	if _, reason := r.call(r.cs, "up.rugpull-rates.get_rates", rateArgs); reason != "" {
		t.Fatal(reason)
	}
	for name, h := range map[string]*hostile{"evil-validator": evil, "rugpull-rates": rug} {
		lines := strings.Split(strings.TrimSpace(h.log.String()), "\n")
		if len(lines) < 4 {
			t.Fatalf("%s: only %d requests logged", name, len(lines))
		}
		for _, line := range lines {
			if strings.Contains(line, `"client_token_present":true`) || strings.Contains(line, r.token) || strings.Contains(strings.ToLower(line), `"authorization"`) {
				t.Errorf("%s: the token of the client reached the server: %s", name, line)
			}
		}
	}
	// The header of the config reached the server that wants it (it would answer 401 otherwise),
	// and its value is not in the log.
	if !strings.Contains(rug.log.String(), `"auth_header_ok":true`) || strings.Contains(rug.log.String(), "s3cret") {
		t.Errorf("log of rugpull-rates: %s", rug.log.String())
	}
	if strings.Contains(evil.log.String(), "X-Gateway-Key") {
		t.Error("the secret header went to a server that has none configured")
	}
}

// T-UP-06: a server that is not in the config cannot be called, and an upstream that
// answers with a redirect to another address is not followed: nothing is sent there.
func TestT_UP_06_OnlyConfiguredAddresses(t *testing.T) {
	behind := startHostile(t, "evil-validator", adversary.EvilValidator(), "", "")
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, behind.url(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	srv := server("redirector", redirect.URL+"/mcp")
	r := newUpRig(t, []upstream.Server{srv}, map[string][]string{"ta": {"up.redirector.validate_address", "up.evil-validator.validate_address"}}, nil)

	// A server outside the config is forbidden (-32031). The client of the SDK must stay
	// connected after it: the next call on the same session still works.
	if _, reason := r.call(r.cs, "up.evil-validator.validate_address", map[string]any{"address": "1 Main St"}); reason != "forbidden" {
		t.Errorf("a server outside the config: reason = %q, want forbidden", reason)
	}
	if _, reason := r.call(r.cs, "up.not-a-server.get_rates", rateArgs); reason != "forbidden" {
		t.Errorf("unknown server: reason = %q, want forbidden", reason)
	}
	if _, reason := r.call(r.cs, "erp.get_price_list", map[string]any{"sku": []string{"X"}}); reason == "unauthorized" || strings.Contains(reason, "closed") {
		t.Errorf("the session broke after forbidden: %q", reason)
	}
	for name := range r.listed(r.cs) {
		if strings.HasPrefix(name, "up.") {
			t.Errorf("%s is listed for a server that only redirects", name)
		}
	}
	if _, reason := r.call(r.cs, "up.redirector.validate_address", map[string]any{"address": "1 Main St"}); reason == "" {
		t.Error("a call through a redirect succeeded")
	}
	if got := behind.log.String(); got != "" {
		t.Errorf("the redirect was followed: %s", got)
	}
}

// T-UP-07: without a permission in the data of the policy an up.* tool is policy_denied
// (upstream.not_allowed); with it the call goes through, for that tenant only.
//
// Residual risk (TL-07 in docs/threat-model.md): an allowed upstream tool gets the
// arguments the agent writes. A model that copies text of a result into an argument is not
// seen by the gateway; the label and the note references only stop references.
func TestT_UP_07_UpstreamToolsNeedAPermission(t *testing.T) {
	h := startHostile(t, "rugpull-rates", adversary.RugpullRates(100), "", "")
	srv := server("rugpull-rates", h.url())
	const name = "up.rugpull-rates.get_rates"

	denied := newUpRig(t, []upstream.Server{srv}, nil, nil)
	denied.pin(srv, "")
	res, reason := denied.call(denied.cs, name, rateArgs)
	if reason != "policy_denied" || res != nil {
		t.Fatalf("without permission: reason = %q", reason)
	}
	_, err := denied.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: rateArgs})
	wire, _ := asWire(err)
	var d struct{ Explain []string }
	_ = json.Unmarshal(wire.Data, &d)
	if strings.Join(d.Explain, ",") != "upstream.not_allowed" {
		t.Errorf("explain = %v", d.Explain)
	}
	if n := denied.count(`SELECT count(*) FROM audit_log WHERE event_type = 'policy_denied' AND tool = $1 AND policy_explain->'deny' @> '["upstream.not_allowed"]'::jsonb`, name); n != 2 {
		t.Errorf("policy_denied rows = %d, want 2", n)
	}
	if n := denied.count(`SELECT count(*) FROM audit_log WHERE event_type = 'upstream_result'`); n != 0 {
		t.Errorf("an upstream result was recorded for a denied call")
	}

	allowed := newUpRig(t, []upstream.Server{srv}, map[string][]string{"ta": {name}}, nil)
	allowed.pin(srv, "")
	if _, reason := allowed.call(allowed.cs, name, rateArgs); reason != "" {
		t.Errorf("with permission: %q", reason)
	}
	other := allowed.opened(allowed.agent("tb"))
	if _, reason := allowed.call(other, name, rateArgs); reason != "policy_denied" {
		t.Errorf("another tenant: reason = %q, want policy_denied", reason)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
