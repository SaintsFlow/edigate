// Package edge is the gate in front of the gateway's two HTTP entrances: /mcp
// for agents and /edi/inbound for partners (the second is in internal/inbound).
//
// Input: every HTTP request to either entrance. Output: either an error response
// in the gateway's single format (internal/rpcerr) or the request passed on with
// the caller's identity attached.
//
// Gate holds what both entrances share: the bucket per client address (spent
// only by failed token checks), the bearer token -> principal step (tenant and
// kind come only from the database) and the buckets per principal and per
// partner. The /mcp handler (Gate.MCP) adds, in this order, all before the SDK
// sees the request:
//  1. only kind "agent" may use /mcp
//  2. bucket per principal
//  3. body size, single JSON-RPC object, Mcp-Method / Mcp-Name against the body
//
// Every request gets a trace id (header Edigate-Trace-Id). The identity reaches
// tool handlers as the SDK's RequestExtra.TokenInfo; see Identity.
package edge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/time/rate"

	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
)

const (
	maxBody     = 1 << 20 // 1 MiB
	traceHeader = "Edigate-Trace-Id"
	// From this MCP-Protocol-Version on, tools/call must carry Mcp-Method and Mcp-Name.
	headerRuleVersion = "2026-07-28"
	bucketTTL         = 10 * time.Minute
	sweepEvery        = time.Minute
)

// PrincipalFinder looks a principal up by token hash. It returns
// store.ErrNotFound for unknown and revoked tokens.
type PrincipalFinder interface {
	FindPrincipal(ctx context.Context, tokenSHA256 []byte) (store.Principal, error)
}

// Identity is who is calling and under which trace id.
type Identity struct {
	PrincipalID string
	TenantID    string
	Kind        string
	TraceID     string
}

// IdentityFrom reads the identity the edge put into the SDK's TokenInfo.
func IdentityFrom(ti *auth.TokenInfo) (Identity, bool) {
	if ti == nil || ti.UserID == "" {
		return Identity{}, false
	}
	str := func(k string) string { s, _ := ti.Extra[k].(string); return s }
	id := Identity{PrincipalID: ti.UserID, TenantID: str("tenant_id"), Kind: str("kind"), TraceID: str("trace_id")}
	return id, id.TenantID != "" && id.TraceID != ""
}

type identityKey struct{}

// Gate is the part of the edge that /mcp and /edi/inbound share.
type Gate struct {
	finder    PrincipalFinder
	log       *slog.Logger
	byAddr    *buckets // 5 failed token checks per second per address
	byPrinc   *buckets // 20 requests per second per principal on /mcp
	byPartner *buckets // 2 interchanges per second per partner on /edi/inbound
}

// NewGate builds the gate. The sweep of old buckets stops when ctx ends.
func NewGate(ctx context.Context, finder PrincipalFinder, log *slog.Logger) *Gate {
	g := &Gate{finder: finder, log: log, byAddr: newBuckets(5, 5), byPrinc: newBuckets(20, 20), byPartner: newBuckets(2, 2)}
	go func() {
		t := time.NewTicker(sweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				g.byAddr.sweep(bucketTTL)
				g.byPrinc.sweep(bucketTTL)
				g.byPartner.sweep(bucketTTL)
			}
		}
	}()
	return g
}

// Caller is an authenticated principal and the trace id of the request.
type Caller struct {
	Principal store.Principal
	TraceID   string
}

// Authenticate runs the steps both entrances share: sets the trace id header,
// checks the bucket of the client address and resolves the bearer token. On
// failure it writes the error response and returns false.
func (g *Gate) Authenticate(w http.ResponseWriter, r *http.Request) (Caller, bool) {
	trace := newTraceID()
	w.Header().Set(traceHeader, trace)

	addrBucket := g.byAddr.get(clientAddr(r))
	if addrBucket.Tokens() < 1 {
		g.Reject(w, trace, "", rpcerr.RateLimited, nil)
		return Caller{}, false
	}
	p, found, err := g.principalFor(r)
	switch {
	case err != nil:
		g.log.Error("principal lookup failed", "trace_id", trace, "error", err.Error())
		g.Reject(w, trace, "", rpcerr.Internal, nil)
		return Caller{}, false
	case !found:
		addrBucket.Allow() // only a failed check spends the address bucket
		g.Reject(w, trace, "", rpcerr.Unauthorized, nil)
		return Caller{}, false
	}
	return Caller{Principal: p, TraceID: trace}, true
}

// AllowPartner takes one token from the bucket of a partner (2 interchanges per
// second, burst 2) and reports whether there was one.
func (g *Gate) AllowPartner(tenantID, partnerID string) bool {
	return g.byPartner.get(tenantID + "|" + partnerID).Allow()
}

// Reject logs and writes an error in the gateway's single format.
func (g *Gate) Reject(w http.ResponseWriter, trace, principal string, reason rpcerr.Reason, id json.RawMessage) {
	g.log.Info("edge reject", "trace_id", trace, "principal_id", principal, "reason", string(reason))
	if reason == rpcerr.Unauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	rpcerr.Write(w, reason, "", id)
}

// RejectTooLarge answers a body over the limit: HTTP 413 and schema_invalid.
func (g *Gate) RejectTooLarge(w http.ResponseWriter, trace, principal string) {
	g.log.Info("edge reject", "trace_id", trace, "principal_id", principal, "reason", string(rpcerr.SchemaInvalid))
	rpcerr.WriteStatus(w, http.StatusRequestEntityTooLarge, rpcerr.New(rpcerr.SchemaInvalid, "request body too large"), nil)
}

// Handler is the /mcp middleware.
type Handler struct {
	gate  *Gate
	inner http.Handler
}

// MCP wraps the SDK handler with the /mcp checks.
func (g *Gate) MCP(next http.Handler) *Handler {
	// The SDK reads identity through its own bearer middleware. Our edge has
	// already checked the token, so this verifier only hands over what we stored.
	verifier := func(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		ti, _ := ctx.Value(identityKey{}).(*auth.TokenInfo)
		if ti == nil {
			return nil, auth.ErrInvalidToken
		}
		return ti, nil
	}
	return &Handler{gate: g, inner: auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(next)}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, ok := h.gate.Authenticate(w, r)
	if !ok {
		return
	}
	p, trace := c.Principal, c.TraceID
	if p.Kind != "agent" {
		h.gate.Reject(w, trace, p.ID, rpcerr.Forbidden, nil)
		return
	}
	if !h.gate.byPrinc.get(p.ID).Allow() {
		h.gate.Reject(w, trace, p.ID, rpcerr.RateLimited, nil)
		return
	}

	if r.Method == http.MethodPost {
		if !h.checkBody(w, r, trace, p.ID) {
			return
		}
	}

	ti := &auth.TokenInfo{
		UserID: p.ID,
		Extra:  map[string]any{"tenant_id": p.TenantID, "kind": p.Kind, "trace_id": trace},
	}
	ctx := context.WithValue(r.Context(), identityKey{}, ti)
	h.inner.ServeHTTP(w, r.WithContext(ctx))
}

// principalFor resolves the Bearer token. found is false for a missing header,
// a wrong format, an unknown hash and a revoked token; err only for database trouble.
func (g *Gate) principalFor(r *http.Request) (p store.Principal, found bool, err error) {
	fields := strings.Fields(r.Header.Get("Authorization"))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || !strings.HasPrefix(fields[1], store.TokenPrefix) {
		return store.Principal{}, false, nil
	}
	p, err = g.finder.FindPrincipal(r.Context(), store.HashToken(fields[1]))
	if errors.Is(err, store.ErrNotFound) {
		return store.Principal{}, false, nil
	}
	if err != nil {
		return store.Principal{}, false, err
	}
	return p, true, nil
}

// rpcCall is the part of a JSON-RPC request the edge looks at.
type rpcCall struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// checkBody reads the body (at most 1 MiB), requires one JSON-RPC object,
// compares the MCP headers with it and puts the body back for the SDK.
// It writes the error response itself and reports whether the request may go on.
func (h *Handler) checkBody(w http.ResponseWriter, r *http.Request, trace, principal string) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.gate.RejectTooLarge(w, trace, principal)
			return false
		}
		h.gate.Reject(w, trace, principal, rpcerr.SchemaInvalid, nil)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		h.gate.Reject(w, trace, principal, rpcerr.SchemaInvalid, nil) // empty, batch or not an object
		return false
	}
	var call rpcCall
	if err := json.Unmarshal(trimmed, &call); err != nil {
		h.gate.Reject(w, trace, principal, rpcerr.SchemaInvalid, nil)
		return false
	}
	if msg := headerMismatch(r.Header, call); msg != "" {
		h.gate.log.Info("edge reject", "trace_id", trace, "principal_id", principal, "reason", string(rpcerr.SchemaInvalid), "code", -32020)
		rpcerr.WriteStatus(w, http.StatusBadRequest, rpcerr.HeaderMismatch(msg), call.ID)
		return false
	}
	return true
}

// headerMismatch returns a message when Mcp-Method or Mcp-Name disagree with the
// body, or are missing where the protocol version requires them; "" when fine.
func headerMismatch(h http.Header, call rpcCall) string {
	method := h.Get("Mcp-Method")
	if method != "" && method != call.Method {
		return "Mcp-Method does not match the request body"
	}
	if call.Method != "tools/call" {
		return ""
	}
	var params struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(call.Params, &params)
	name := h.Get("Mcp-Name")
	if name != "" && name != params.Name {
		return "Mcp-Name does not match the tool name in the request body"
	}
	if h.Get("Mcp-Protocol-Version") >= headerRuleVersion && h.Get("Mcp-Protocol-Version") != "" {
		if method == "" || name == "" {
			return "Mcp-Method and Mcp-Name are required for tools/call"
		}
	}
	return ""
}

func newTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// clientAddr is the peer address without the port. X-Forwarded-For is not read:
// a client could write anything there.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// buckets keeps one token bucket per key and forgets keys not seen for a while.
type buckets struct {
	mu    sync.Mutex
	m     map[string]*bucket
	rate  rate.Limit
	burst int
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

func newBuckets(r rate.Limit, burst int) *buckets {
	return &buckets{m: map[string]*bucket{}, rate: r, burst: burst}
}

func (b *buckets) get(key string) *rate.Limiter {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.m[key]
	if !ok {
		e = &bucket{lim: rate.NewLimiter(b.rate, b.burst)}
		b.m[key] = e
	}
	e.seen = time.Now()
	return e.lim
}

func (b *buckets) sweep(older time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, e := range b.m {
		if time.Since(e.seen) > older {
			delete(b.m, k)
		}
	}
}
