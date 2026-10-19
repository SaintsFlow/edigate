package txn_test

// The rig of the txn tests: a gateway on a throwaway database (MCP, partner
// entrance, operator entrance), mock-1c in the same database, and a relay that
// the tests step by hand, so that every test controls when a delivery happens.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/edge"
	"github.com/SaintsFlow/edigate/internal/inbound"
	"github.com/SaintsFlow/edigate/internal/mcpserver"
	"github.com/SaintsFlow/edigate/internal/ops"
	"github.com/SaintsFlow/edigate/internal/policy"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/store/storetest"
	"github.com/SaintsFlow/edigate/internal/tools"
	"github.com/SaintsFlow/edigate/internal/txn"
	onecserver "github.com/SaintsFlow/edigate/mocks/onec/server"
)

// gateways are the ISA07:ISA08 ids of the two tenants.
var gateways = map[string]string{"ta": "ZZ:ZZTESTSUPPL", "tb": "ZZ:ZZTESTSUPPB"}

type rig struct {
	t       *testing.T
	st      *store.Store
	pool    *pgxpool.Pool
	url     string // the gateway
	mock    *onecserver.Server
	mockURL string // mock-1c
	relay   *txn.Relay
	rec     *recorder // sits in front of the policy and keeps what it was asked
	seq     atomic.Int64
}

// recorder is a Decider that remembers every call and passes it to the real policy.
type recorder struct {
	inner tools.Decider
	mu    sync.Mutex
	calls []policy.ToolCall
	who   []policy.Principal
}

func (c *recorder) Decide(ctx context.Context, p policy.Principal, call policy.ToolCall) (policy.Decision, error) {
	c.mu.Lock()
	c.calls = append(c.calls, call)
	c.who = append(c.who, p)
	c.mu.Unlock()
	return c.inner.Decide(ctx, p, call)
}

func newRig(t *testing.T) *rig {
	t.Helper()
	st, pool := storetest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &rig{t: t, st: st, pool: pool}
	for id, gw := range gateways {
		r.must(st.AddTenant(ctx, id, id, gw))
		r.exec(`INSERT INTO price_list (tenant_id, sku, unit_price, currency, unit) VALUES
		        ($1, 'SKU-1', 1.50, 'USD', 'EA'), ($1, 'SKU-2', 2.00, 'USD', 'EA')`, id)
	}
	r.must(onecserver.Migrate(ctx, pool))
	r.mock = onecserver.New(pool, onecserver.Config{})
	mockTS := httptest.NewServer(r.mock.Handler())
	t.Cleanup(mockTS.Close)
	r.mockURL = mockTS.URL

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gate := edge.NewGate(ctx, st, log)
	engine, err := policy.New(ctx)
	r.must(err)
	r.rec = &recorder{inner: engine}
	srv := mcpserver.New(st, nil, r.rec, log)
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	mux := http.NewServeMux()
	mux.Handle("/mcp", gate.MCP(sdk))
	mux.Handle("/edi/inbound", inbound.New(gate, st, log))
	mux.Handle("/ops/", ops.New(gate, st, log))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	r.url = ts.URL

	// Scale 1e6 shrinks every retry pause to the 10 ms minimum.
	r.relay = &txn.Relay{Store: st, Adapter: adapters.NewOnec(mockTS.URL), Workers: 1, Scale: 1e6, Log: log}
	return r
}

func (r *rig) must(err error) {
	r.t.Helper()
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) exec(sql string, args ...any) {
	r.t.Helper()
	_, err := r.pool.Exec(context.Background(), sql, args...)
	r.must(err)
}

func (r *rig) count(sql string, args ...any) int {
	r.t.Helper()
	var n int
	r.must(r.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func (r *rig) text(sql string, args ...any) string {
	r.t.Helper()
	var s string
	r.must(r.pool.QueryRow(context.Background(), sql, args...).Scan(&s))
	return s
}

// token issues a token of a kind for a tenant and returns the principal id and the token.
func (r *rig) token(tenant, kind, partnerID string) (id, token string) {
	r.t.Helper()
	token, sum, err := store.NewToken()
	r.must(err)
	id = fmt.Sprintf("%s-%s-%d", tenant, kind, r.seq.Add(1))
	r.must(r.st.IssuePrincipal(context.Background(), store.Principal{ID: id, TenantID: tenant, Kind: kind, PartnerID: partnerID}, sum))
	return id, token
}

// partner registers a customer partner of the tenant. The id is the same for
// every tenant on purpose: the keys of two tenants must still differ.
func (r *rig) partner(tenant string) (id, token string) {
	r.t.Helper()
	id = "ZZ:ZZTESTBUYER"
	r.must(r.st.AddPartner(context.Background(), store.Partner{TenantID: tenant, PartnerID: id, Role: "customer", Name: id}))
	r.exec(`UPDATE partners SET auto_max_amount = 5000, daily_auto_max = 50000, forecast_amount = 5000
	        WHERE tenant_id = $1 AND partner_id = $2`, tenant, id)
	r.exec(`INSERT INTO partner_ship_to (tenant_id, partner_id, code) VALUES ($1, $2, 'DC01')`, tenant, id)
	_, token = r.token(tenant, "partner", id)
	return id, token
}

// limits sets the limits of a partner in currency units.
func (r *rig) limits(tenant, partnerID string, perDocument, perDay int) {
	r.t.Helper()
	r.exec(`UPDATE partners SET auto_max_amount = $3, daily_auto_max = $4 WHERE tenant_id = $1 AND partner_id = $2`,
		tenant, partnerID, perDocument, perDay)
}

type orderLine struct{ sku, qty, price string }

// send850 posts an order of the partner in a new interchange and returns the id
// of the stored document. gs06 and isa13 are the numbers of the exchange.
func (r *rig) send850(tenant, partnerID, token, isa13, gs06, beg03 string, lines ...orderLine) string {
	r.t.Helper()
	return r.send850x(tenant, partnerID, token, isa13, gs06, beg03, "", lines...)
}

// send850x is send850 with extra segments (for example free text) after the last line.
func (r *rig) send850x(tenant, partnerID, token, isa13, gs06, beg03, extra string, lines ...orderLine) string {
	r.t.Helper()
	pad := func(id string) string {
		name := strings.TrimPrefix(id, "ZZ:")
		return name + strings.Repeat(" ", 15-len(name))
	}
	body := "BEG*00*SA*" + beg03 + "**20261018~N1*BY*TEST BUYER*92*B001~N1*ST*TEST SHIP-TO*92*DC01~"
	for i, l := range lines {
		body += fmt.Sprintf("PO1*%d*%s*EA*%s**VN*%s~", i+1, l.qty, l.price, l.sku)
	}
	body += extra
	body += "CTT*" + strconv.Itoa(len(lines)) + "~"
	segments := strings.Count(body, "~") + 2
	raw := "ISA*00*          *00*          *ZZ*" + pad(partnerID) + "*ZZ*" + pad(gateways[tenant]) + "*261018*1200*U*00401*" + isa13 + "*0*T*>~" +
		"GS*PO*" + strings.TrimPrefix(partnerID, "ZZ:") + "*" + strings.TrimPrefix(gateways[tenant], "ZZ:") + "*20261018*1200*" + gs06 + "*X*004010~" +
		"ST*850*0001~" + body + "SE*" + strconv.Itoa(segments) + "*0001~GE*1*" + gs06 + "~IEA*1*" + isa13 + "~"

	if code, answer := r.postInbound(raw, tenant, partnerID, token, isa13); code != http.StatusOK {
		r.t.Fatalf("send 850: HTTP %d: %s", code, answer)
	}
	return r.text(`SELECT d.id::text FROM edi_documents d JOIN edi_interchanges i ON i.id = d.interchange_id
	               WHERE d.tenant_id = $1 AND i.isa13 = $2 AND i.counterparty_id = $3`, tenant, isa13, partnerID)
}

var isa13Counter atomic.Int64

// nextISA13 gives a nine digit number nobody used in this process.
func nextISA13() string { return fmt.Sprintf("%09d", 100000000+isa13Counter.Add(1)) }

// session opens an MCP session as the agent with this token.
func (r *rig) session(token string) *mcp.ClientSession {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	r.t.Cleanup(cancel)
	transport := &mcp.StreamableClientTransport{Endpoint: r.url + "/mcp", DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: bearer{token}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, transport, nil)
	r.must(err)
	r.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type bearer struct{ token string }

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(req)
}

// call runs a tool on a session. It returns the structured answer or the JSON-RPC error.
func (r *rig) call(cs *mcp.ClientSession, tool string, args map[string]any) (json.RawMessage, *jsonrpc.Error) {
	r.t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		var wire *jsonrpc.Error
		if errors.As(err, &wire) {
			return nil, wire
		}
		r.t.Fatal(err)
	}
	raw, err := json.Marshal(res.StructuredContent)
	r.must(err)
	return raw, nil
}

// order calls erp.create_sales_order and decodes the answer.
func (r *rig) order(cs *mcp.ClientSession, docID string, extra ...string) (answer, *jsonrpc.Error) {
	r.t.Helper()
	args := map[string]any{"source_document_id": docID}
	if len(extra) > 0 {
		args["delivery_instruction"] = extra[0]
	}
	raw, wire := r.call(cs, "erp.create_sales_order", args)
	if wire != nil {
		return answer{}, wire
	}
	var a answer
	r.must(json.Unmarshal(raw, &a))
	return a, nil
}

type answer struct {
	Status     string `json:"status"`
	TxnID      string `json:"txn_id"`
	Reason     string `json:"reason"`
	ApprovalID string `json:"approval_id"`
}

func reasonOf(w *jsonrpc.Error) string {
	if w == nil {
		return ""
	}
	var d struct{ Reason string }
	_ = json.Unmarshal(w.Data, &d)
	return d.Reason
}

// drain steps the relay until no row is due, for at most 50 rows.
func (r *rig) drain() {
	r.t.Helper()
	for range 50 {
		did, err := r.relay.Step(context.Background())
		r.must(err)
		if !did {
			return
		}
	}
	r.t.Fatal("the outbox did not drain")
}

// orders counts the orders of a tenant in mock-1c.
func (r *rig) orders(tenant string) int {
	return r.count(`SELECT count(*) FROM mock_onec.sales_orders WHERE tenant_id = $1`, tenant)
}

func (r *rig) keyHex(txnID string) string {
	return r.text(`SELECT encode(key, 'hex') FROM idempotency_keys WHERE txn_id = $1::uuid`, txnID)
}
