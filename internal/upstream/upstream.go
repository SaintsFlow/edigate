// Package upstream lets the gateway pass the tools of other MCP servers to the agent
// without trusting them (flow P8, controls M2 and M4).
//
// Input: the list of servers from the config, the public key of the administrator,
// and the pins in the table manifests. Output: for the agent, the tools of each
// server under the name up.<server>.<tool>, only those whose description is pinned
// and signed; for a call, the result of the upstream with the label
// {"edigate/taint": "upstream:<server>"} in _meta.
//
// Every tools/list of the agent and every call reads tools/list of the upstream
// again and compares the SHA-256 of each description (canonical, RFC 8785) with its
// pin. No pin, a signature that does not verify, or another hash: the tool is
// switched off (not listed, a call answers manifest_drift) and the event is audited
// with both hashes. A server that does not answer lists nothing.
//
// The gateway calls only the addresses of the config, never follows a redirect, and
// sends to an upstream only the header the config names. The token of the client of
// the gateway is never passed on: the upstream client is built from the config alone.
package upstream

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
)

// Prefix is put before every upstream tool name; the gateway chooses it, the server cannot.
const Prefix = "up."

// callTimeout is the limit for one request to an upstream (TL-06).
const callTimeout = 15 * time.Second

// maxListPages stops an upstream that never ends its tool list.
const maxListPages = 10

// AuthHeader names a header the gateway sends to an upstream and the environment
// variable that holds its value. The value is not in the config file.
type AuthHeader struct {
	Name string `yaml:"name"`
	Env  string `yaml:"env"`
}

// Server is one allowed upstream server.
type Server struct {
	Name              string      `yaml:"name"`
	URL               string      `yaml:"url"`
	AuthHeader        *AuthHeader `yaml:"auth_header"`
	QuarantineResults bool        `yaml:"quarantine_results"`
}

var (
	nameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	headerRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// Check validates one entry of the config.
func (s Server) Check() error {
	if !nameRe.MatchString(s.Name) {
		return fmt.Errorf("upstream %q: the name must match %s", s.Name, nameRe)
	}
	u, err := url.Parse(s.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("upstream %q: url must be an http or https address", s.Name)
	}
	if a := s.AuthHeader; a != nil && (!headerRe.MatchString(a.Name) || a.Env == "") {
		return fmt.Errorf("upstream %q: auth_header needs a header name and an environment variable", s.Name)
	}
	return nil
}

// CheckAll validates the whole list: every entry and no repeated name.
func CheckAll(servers []Server) error {
	seen := map[string]bool{}
	for _, s := range servers {
		if err := s.Check(); err != nil {
			return err
		}
		if seen[s.Name] {
			return fmt.Errorf("upstream %q is listed twice", s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}

// Actor is who triggered a read of the upstream; the audit rows carry it.
type Actor struct {
	TenantID    string
	PrincipalID string
	TraceID     string
}

// States of a tool against its pin.
const (
	StateOK           = "ok"
	StateNoPin        = "no_pin"
	StateBadSignature = "bad_signature"
	StateChanged      = "changed"
)

// Tool is one tool as the upstream describes it now, with the verdict on its pin.
type Tool struct {
	Server string
	Live   *mcp.Tool
	Hash   [32]byte
	Pin    *store.Manifest // nil: no pin
	State  string

	Quarantine bool // the server has quarantine_results
}

// Enabled reports whether the agent may see and call the tool.
func (t Tool) Enabled() bool { return t.State == StateOK }

// PublicName is the name the agent sees.
func (t Tool) PublicName() string { return Prefix + t.Server + "." + t.Live.Name }

// Public is the tool as the agent sees it: the prefixed name and the five members that
// the pin covers, nothing else of what the upstream sent.
//
// For a server with quarantine_results the output schema is not shown: a structured result
// may be dropped (see Shape), and a client that expects one by the schema would fail.
func (t Tool) Public() *mcp.Tool {
	pub := &mcp.Tool{
		Name: t.PublicName(), Description: t.Live.Description, InputSchema: t.Live.InputSchema,
		OutputSchema: t.Live.OutputSchema, Annotations: t.Live.Annotations,
	}
	if t.Quarantine {
		pub.OutputSchema = nil
	}
	return pub
}

// Gateway reads and calls the configured upstream servers.
type Gateway struct {
	servers map[string]Server
	order   []string
	secrets map[string]string // header value by server name
	pub     ed25519.PublicKey
	store   *store.Store
	log     *slog.Logger

	mu       sync.Mutex
	reported map[string]bool // drift already audited, by server|tool|live hash
}

// New builds the gateway. getenv reads the secrets named by auth_header.env. Without the
// public key no description can be verified, so the servers are switched off (a warning
// says so): nothing of an upstream reaches the agent. A key that is not a key, or a
// missing secret, is refused.
func New(servers []Server, pubKeyB64 string, st *store.Store, log *slog.Logger, getenv func(string) string) (*Gateway, error) {
	if err := CheckAll(servers); err != nil {
		return nil, err
	}
	g := &Gateway{
		servers: map[string]Server{}, secrets: map[string]string{}, store: st, log: log, reported: map[string]bool{},
	}
	if len(servers) == 0 {
		return g, nil
	}
	if pubKeyB64 == "" {
		log.Warn("upstream servers are off: EDIGATE_MANIFEST_PUBKEY is empty", "servers", len(servers))
		return g, nil
	}
	pub, err := ParsePublicKey(pubKeyB64)
	if err != nil {
		return nil, fmt.Errorf("EDIGATE_MANIFEST_PUBKEY: %w", err)
	}
	g.pub = pub
	for _, s := range servers {
		g.servers[s.Name] = s
		g.order = append(g.order, s.Name)
		if s.AuthHeader != nil {
			v := getenv(s.AuthHeader.Env)
			if v == "" {
				return nil, fmt.Errorf("upstream %q: the variable %s is empty", s.Name, s.AuthHeader.Env)
			}
			g.secrets[s.Name] = v
		}
	}
	return g, nil
}

// Servers returns the configured servers in the order of the config.
func (g *Gateway) Servers() []Server {
	out := make([]Server, 0, len(g.order))
	for _, n := range g.order {
		out = append(out, g.servers[n])
	}
	return out
}

// Refresh reads every server and returns all their tools with the verdict on each pin. A
// tool that is not ok is reported (audited when the actor is known, logged otherwise). A
// server that does not answer is skipped.
func (g *Gateway) Refresh(ctx context.Context, actor *Actor) []Tool {
	var out []Tool
	for _, name := range g.order {
		tools, err := g.Inspect(ctx, g.servers[name])
		if err != nil {
			g.log.Warn("upstream not available", "server", name, "error", err.Error())
			continue
		}
		g.report(ctx, actor, tools)
		out = append(out, tools...)
	}
	return out
}

// Inspect reads tools/list of one server and compares each description with its pin.
func (g *Gateway) Inspect(ctx context.Context, srv Server) ([]Tool, error) {
	live, err := g.fetch(ctx, srv)
	if err != nil {
		return nil, err
	}
	pins, err := g.store.Manifests(ctx, srv.Name)
	if err != nil {
		return nil, err
	}
	out := make([]Tool, 0, len(live))
	for _, lt := range live {
		hash, err := Hash(lt)
		if err != nil {
			g.log.Warn("upstream tool skipped: cannot be hashed", "server", srv.Name, "tool", lt.Name)
			continue
		}
		t := Tool{Server: srv.Name, Live: lt, Hash: hash, State: StateNoPin, Quarantine: srv.QuarantineResults}
		if p, ok := pins[lt.Name]; ok {
			t.Pin = &p
			t.State = g.verdict(srv.Name, hash, p)
		}
		out = append(out, t)
	}
	return out, nil
}

// verdict compares the live hash with a pin: the signature of the pin first, then the hash.
func (g *Gateway) verdict(server string, live [32]byte, p store.Manifest) string {
	if len(p.SHA256) != sha256.Size {
		return StateBadSignature
	}
	var pinned [32]byte
	copy(pinned[:], p.SHA256)
	switch {
	case !Verify(g.pub, server, pinned, p.Signature):
		return StateBadSignature
	case pinned != live:
		return StateChanged
	}
	return StateOK
}

// report audits the tools that are switched off, once for each state of the description.
func (g *Gateway) report(ctx context.Context, actor *Actor, tools []Tool) {
	for _, t := range tools {
		prefix := t.Server + "|" + t.Live.Name + "|"
		key := prefix + hex.EncodeToString(t.Hash[:]) + "|" + t.State
		g.mu.Lock()
		seen := g.reported[key]
		if t.Enabled() {
			for k := range g.reported { // a tool that is fine again may be reported again
				if strings.HasPrefix(k, prefix) {
					delete(g.reported, k)
				}
			}
		} else {
			g.reported[key] = true
		}
		g.mu.Unlock()
		if t.Enabled() || seen {
			continue
		}
		g.log.Warn("manifest drift: the tool is switched off", "server", t.Server, "tool", t.Live.Name, "state", t.State)
		if actor == nil {
			continue
		}
		pinned := ""
		if t.Pin != nil {
			pinned = hex.EncodeToString(t.Pin.SHA256)
		}
		detail, _ := json.Marshal(map[string]string{
			"server": t.Server, "tool": t.Live.Name, "state": t.State,
			"pinned_sha256": pinned, "live_sha256": hex.EncodeToString(t.Hash[:]),
		})
		err := g.store.AppendAudit(ctx, store.AuditEvent{
			TenantID: actor.TenantID, EventType: "manifest_drift", TraceID: actor.TraceID, PrincipalID: actor.PrincipalID,
			Tool: t.PublicName(), PayloadSHA256: t.Hash[:], Outcome: string(rpcerr.ManifestDrift), Detail: detail,
		})
		if err != nil {
			g.log.Error("audit write failed", "event", "manifest_drift", "error", err.Error())
		}
	}
}

// Resolve finds the upstream tool behind a name the agent used and checks its pin now. A
// server that is not in the config is forbidden, a tool the server does not have is
// not_found, and a tool that is not ok is manifest_drift.
func (g *Gateway) Resolve(ctx context.Context, publicName string, actor Actor) (Tool, error) {
	server, tool, ok := SplitName(publicName)
	srv, known := g.servers[server]
	if !ok || !known {
		return Tool{}, rpcerr.New(rpcerr.Forbidden, "")
	}
	tools, err := g.Inspect(ctx, srv)
	if err != nil {
		g.log.Warn("upstream not available", "server", server, "error", err.Error())
		return Tool{}, rpcerr.New(rpcerr.Internal, "internal error")
	}
	for _, t := range tools {
		if t.Live.Name != tool {
			continue
		}
		if !t.Enabled() {
			g.report(ctx, &actor, []Tool{t})
			return Tool{}, rpcerr.New(rpcerr.ManifestDrift, "")
		}
		return t, nil
	}
	return Tool{}, rpcerr.New(rpcerr.NotFound, "")
}

// SplitName splits up.<server>.<tool> into server and tool. The tool name may hold dots.
func SplitName(public string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(public, Prefix)
	if !found {
		return "", "", false
	}
	server, tool, found = strings.Cut(rest, ".")
	return server, tool, found && server != "" && tool != ""
}

// Call runs the tool at the upstream and returns the result as it came.
func (g *Gateway) Call(ctx context.Context, t Tool, args json.RawMessage) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cs, err := g.connect(ctx, g.servers[t.Server])
	if err != nil {
		return nil, err
	}
	defer cs.Close()
	var arguments any = map[string]any{}
	if len(args) > 0 {
		arguments = args
	}
	return cs.CallTool(ctx, &mcp.CallToolParams{Name: t.Live.Name, Arguments: arguments})
}

// fetch reads the whole tools/list of a server.
func (g *Gateway) fetch(ctx context.Context, srv Server) ([]*mcp.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cs, err := g.connect(ctx, srv)
	if err != nil {
		return nil, err
	}
	defer cs.Close()
	return listAll(ctx, cs)
}

// FetchTools reads the live tool list of a server straight from the config; edigatectl
// uses it to pin. secret is the value of the auth header ("" when the server has none).
func FetchTools(ctx context.Context, srv Server, secret string) ([]*mcp.Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	cs, err := connect(ctx, srv, secret)
	if err != nil {
		return nil, err
	}
	defer cs.Close()
	return listAll(ctx, cs)
}

func listAll(ctx context.Context, cs *mcp.ClientSession) ([]*mcp.Tool, error) {
	var out []*mcp.Tool
	cursor := ""
	for range maxListPages {
		res, err := cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
	return nil, errors.New("the upstream tool list does not end")
}

func (g *Gateway) connect(ctx context.Context, srv Server) (*mcp.ClientSession, error) {
	return connect(ctx, srv, g.secrets[srv.Name])
}

// connect opens a session to the server. The HTTP client talks only to the origin of the
// configured URL, follows no redirect, and adds only the configured header.
func connect(ctx context.Context, srv Server, secret string) (*mcp.ClientSession, error) {
	u, err := url.Parse(srv.URL)
	if err != nil {
		return nil, err
	}
	rt := originTransport{origin: u.Scheme + "://" + u.Host}
	if srv.AuthHeader != nil {
		rt.header, rt.value = srv.AuthHeader.Name, secret
	}
	hc := &http.Client{
		Timeout: callTimeout, Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	transport := &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: hc, DisableStandaloneSSE: true}
	client := mcp.NewClient(&mcp.Implementation{Name: "edigate", Version: "0.1.0-dev"}, nil)
	return client.Connect(ctx, transport, nil)
}

// originTransport refuses any request that is not for the configured origin and sets the
// one configured header.
type originTransport struct {
	origin, header, value string
}

func (t originTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme+"://"+r.URL.Host != t.origin {
		return nil, errors.New("request to an address outside the config")
	}
	r = r.Clone(r.Context())
	if t.header != "" {
		r.Header.Set(t.header, t.value)
	}
	return http.DefaultTransport.RoundTrip(r)
}
