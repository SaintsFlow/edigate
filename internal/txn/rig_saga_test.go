package txn_test

// The rig of the tests of wave 2-7 (sagas, outgoing documents, MDN and 997): the rig of the
// earlier waves plus mock-wms, partner-emulator and the saga runner. The relay and the
// runner are stepped by the test, so that it controls when anything happens; the partner
// answers on its own (its 997 comes asynchronously, as it does on the stand).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/txn/saga"
	partnerserver "github.com/SaintsFlow/edigate/mocks/partner/server"
	wmsserver "github.com/SaintsFlow/edigate/mocks/wms/server"
)

type sagaRig struct {
	*rig
	wms        *wmsserver.Server
	partner    *partnerserver.Server
	partnerURL string
	runner     *saga.Runner
	partnerID  string
	ptoken     string
	agent      *mcp.ClientSession
	op1, op2   string // tokens of two operators
}

func newSagaRig(t *testing.T) *sagaRig {
	t.Helper()
	r := newRig(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	r.must(wmsserver.Migrate(ctx, r.pool))
	wms := wmsserver.New(r.pool, wmsserver.Config{})
	wmsTS := httptest.NewServer(wms.Handler())
	t.Cleanup(wmsTS.Close)

	r.must(partnerserver.Migrate(ctx, r.pool))
	partnerID, ptoken := r.partner("ta")
	tokens := filepath.Join(t.TempDir(), "partner_tokens.json")
	raw, err := json.Marshal(map[string]string{partnerID: ptoken})
	r.must(err)
	r.must(os.WriteFile(tokens, raw, 0o600))
	partner := partnerserver.New(r.pool, partnerserver.Config{
		Knobs: partnerserver.Knobs{AckStatus: "A"}, GatewayURL: r.url, TokensFile: tokens, Log: log,
	})
	partnerTS := httptest.NewServer(partner.Handler())
	t.Cleanup(func() { partnerTS.Close(); partner.Close() })

	effects := adapters.Mux{Onec: adapters.NewOnec(r.mockURL), WMS: adapters.NewWMS(wmsTS.URL)}
	r.relay.Adapter = effects
	r.relay.AS2 = adapters.NewAS2(partnerTS.URL)
	r.relay.MDNRepeatAfter = 30 * time.Millisecond
	runner := &saga.Runner{
		Store: r.st, Adapter: effects, Facts: adapters.NewWMS(wmsTS.URL), Policy: r.rec,
		Scale: 1e6, AckTimeout: time.Hour, Log: log,
	}
	_, atoken := r.token("ta", "agent", "")
	_, op1 := r.token("ta", "operator", "")
	_, op2 := r.token("ta", "operator", "")
	return &sagaRig{
		rig: r, wms: wms, partner: partner, partnerURL: partnerTS.URL, runner: runner,
		partnerID: partnerID, ptoken: ptoken, agent: r.session(atoken), op1: op1, op2: op2,
	}
}

// knobs sets the knobs of the partner (the JSON of POST /control).
func (s *sagaRig) knobs(body string) {
	s.t.Helper()
	resp, err := http.Post(s.partnerURL+"/control", "application/json", strings.NewReader(body))
	s.must(err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		answer, _ := io.ReadAll(resp.Body)
		s.t.Fatalf("control %s: HTTP %d %s", body, resp.StatusCode, answer)
	}
}

// pump runs one round: the relay delivers what is due, the runner looks at the sagas.
func (s *sagaRig) pump() {
	s.t.Helper()
	for range 50 {
		did, err := s.relay.Step(context.Background())
		s.must(err)
		if !did {
			break
		}
	}
	_, err := s.runner.Pass(context.Background())
	s.must(err)
}

// until pumps until the condition holds, for at most 20 seconds.
func (s *sagaRig) until(what string, cond func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		s.pump()
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for: %s", what)
}

var refCounter = 0

// newOrder receives an 850 of the partner with the given lines (133.00 by default) and
// returns the document id and the business reference.
func (s *sagaRig) newOrder(lines ...orderLine) (doc, ref string) {
	s.t.Helper()
	refCounter++
	ref = "PO-" + strconv.Itoa(refCounter) + "-" + strconv.FormatInt(time.Now().UnixNano()%100000, 10)
	if len(lines) == 0 {
		lines = []orderLine{{"SKU-1", "40", "1.00"}, {"SKU-2", "36.5", "1.00"}} // 40 x 1.50 + 36.5 x 2.00 = 133.00
	}
	return s.send850("ta", s.partnerID, s.ptoken, nextISA13(), "1", ref, lines...), ref
}

// tool calls a tool as the agent and decodes the answer; a wire error fails the test.
func (s *sagaRig) tool(name string, args map[string]any) answer {
	s.t.Helper()
	raw, wire := s.call(s.agent, name, args)
	if wire != nil {
		s.t.Fatalf("%s: %v", name, wire)
	}
	var a answer
	s.must(json.Unmarshal(raw, &a))
	return a
}

// createOrder runs erp.create_sales_order and waits until the order is in mock-1c.
func (s *sagaRig) createOrder(doc string) answer {
	s.t.Helper()
	a := s.tool("erp.create_sales_order", map[string]any{"source_document_id": doc})
	if a.Status != "accepted" {
		s.t.Fatalf("create order: %+v", a)
	}
	s.until("the order is created", func() bool { return s.txnStatus(s.agent, a.TxnID) == "completed" })
	return a
}

// sagaState reads the state of the saga of a business reference.
func (s *sagaRig) sagaState(kind, ref string) string {
	s.t.Helper()
	return s.text(`SELECT state FROM sagas WHERE kind = $1 AND business_key->>1 = $2`, kind, ref)
}

// docState reads the state of the outgoing document of a set (855 or 856) of a reference.
func (s *sagaRig) docState(st01, ref string) string {
	s.t.Helper()
	return s.text(`SELECT d.state FROM outbound_docs d JOIN saga_steps st ON st.saga_id = d.saga_id AND st.step_no = d.step_no
	               JOIN sagas sg ON sg.id = st.saga_id WHERE d.st01 = $1 AND sg.business_key->>1 = $2`, st01, ref)
}

// received counts the transmissions the partner took of a set of a reference, and how
// many different ISA13 values they carried.
func (s *sagaRig) received(st01, ref string) (transmissions, interchanges int) {
	s.t.Helper()
	s.must(s.pool.QueryRow(context.Background(),
		`SELECT count(*), count(DISTINCT isa13) FROM mock_partner.received WHERE st01 = $1 AND business_ref = $2`, st01, ref).
		Scan(&transmissions, &interchanges))
	return
}

// settleDoc waits until the outgoing document of the reference is in the state.
func (s *sagaRig) settleDoc(st01, ref, state string) {
	s.t.Helper()
	s.until(fmt.Sprintf("the %s of %s is %s", st01, ref, state), func() bool {
		var got string
		err := s.pool.QueryRow(context.Background(),
			`SELECT d.state FROM outbound_docs d JOIN sagas sg ON sg.id = d.saga_id
			 WHERE d.st01 = $1 AND sg.business_key->>1 = $2`, st01, ref).Scan(&got)
		return err == nil && got == state
	})
}

// send997 posts a 997 as the partner: group is AK102, sets are ST02 and AK501 pairs, ak9 the
// group verdict. isa13 is the number of the interchange.
func (s *sagaRig) send997(partnerID, token, isa13, group string, ak9 string, sets ...[2]string) (int, string) {
	s.t.Helper()
	body := "AK1*PR*" + group + "~"
	for _, set := range sets {
		body += "AK2*855*" + set[0] + "~AK5*" + set[1] + "~"
	}
	n := strconv.Itoa(len(sets))
	body += "AK9*" + ak9 + "*" + n + "*" + n + "*" + n + "~"
	segments := strings.Count(body, "~") + 2
	pad := func(id string) string {
		name := strings.TrimPrefix(id, "ZZ:")
		return name + strings.Repeat(" ", 15-len(name))
	}
	raw := "ISA*00*          *00*          *ZZ*" + pad(partnerID) + "*ZZ*" + pad(gateways["ta"]) + "*261018*1200*U*00401*" + isa13 + "*0*T*>~" +
		"GS*FA*" + strings.TrimPrefix(partnerID, "ZZ:") + "*" + strings.TrimPrefix(gateways["ta"], "ZZ:") + "*20261018*1200*" + isa13[3:] + "*X*004010~" +
		"ST*997*0001~" + body + "SE*" + strconv.Itoa(segments) + "*0001~GE*1*" + isa13[3:] + "~IEA*1*" + isa13 + "~"
	code, answer := s.postInbound(raw, "ta", partnerID, token, isa13)
	return code, string(answer)
}

// docNumbers returns (gs06, st02) of the outgoing document of a set and reference.
func (s *sagaRig) docNumbers(st01, ref string) (gs06, st02 string) {
	s.t.Helper()
	s.must(s.pool.QueryRow(context.Background(),
		`SELECT d.gs06, d.st02 FROM outbound_docs d JOIN sagas sg ON sg.id = d.saga_id
		 WHERE d.st01 = $1 AND sg.business_key->>1 = $2`, st01, ref).Scan(&gs06, &st02))
	return
}

// rawOf returns the bytes the gateway stored for the outgoing document.
func (s *sagaRig) rawOf(st01, ref string) []byte {
	s.t.Helper()
	var raw []byte
	s.must(s.pool.QueryRow(context.Background(),
		`SELECT i.raw FROM edi_interchanges i JOIN outbound_docs d ON d.tenant_id = i.tenant_id AND d.isa13 = i.isa13
		 AND i.direction = 'out' AND i.counterparty_id = d.partner_id
		 JOIN sagas sg ON sg.id = d.saga_id WHERE d.st01 = $1 AND sg.business_key->>1 = $2`, st01, ref).Scan(&raw))
	return raw
}

// auditCount counts the audit rows of an event type of the tenant "ta".
func (s *sagaRig) auditCount(event string) int {
	s.t.Helper()
	return s.count(`SELECT count(*) FROM audit_log WHERE tenant_id = 'ta' AND event_type = $1`, event)
}

// ids runs a query that returns one text column and gives the values.
func (s *sagaRig) ids(sql string) []string {
	s.t.Helper()
	rows, err := s.pool.Query(context.Background(), sql)
	s.must(err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		s.must(rows.Scan(&v))
		out = append(out, v)
	}
	s.must(rows.Err())
	return out
}

// partnerOf is a customer partner of a tenant.
func partnerOf(tenant, id string) store.Partner {
	return store.Partner{TenantID: tenant, PartnerID: id, Role: "customer", Name: id}
}
