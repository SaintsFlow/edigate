package server

// Tests of wave 2-9a for the emulator shared by several gateway processes (slots of the
// benchmark): the 997 goes to the address of the header Edigate-Reply-To, with the token
// found under that address, and the free letters of the open tools.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SaintsFlow/edigate/internal/store/storetest"
)

// gatewayStub records the tokens of the 997 posted to it.
type gatewayStub struct {
	mu     sync.Mutex
	tokens []string
	ts     *httptest.Server
}

func newGatewayStub(t *testing.T) *gatewayStub {
	g := &gatewayStub{}
	g.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.tokens = append(g.tokens, r.Header.Get("Authorization"))
		g.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(g.ts.Close)
	return g
}

func (g *gatewayStub) got() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.tokens...)
}

func (g *gatewayStub) waitFor(t *testing.T, n int) []string {
	t.Helper()
	for range 300 {
		if got := g.got(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the stub got %v, want %d requests", g.got(), n)
	return nil
}

func transmitFrom(t *testing.T, url, replyTo string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/as2", bytes.NewReader(asn855()))
	req.Header.Set("AS2-From", "ZZ:ZZTESTSUPPL")
	req.Header.Set("AS2-To", "ZZ:ZZTESTBUYER")
	req.Header.Set("Message-Id", "<m1@edigate>")
	if replyTo != "" {
		req.Header.Set(ReplyToHeader, replyTo)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// The 997 goes to the address in the header, with the token of "<address> <partner>"; without the
// header it goes to GATEWAY_URL with the token of "<partner>". Another process gets nothing.
func TestEmulatorAnswersTheProcessThatSent(t *testing.T) {
	_, pool := storetest.New(t)
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	a, b, fallback := newGatewayStub(t), newGatewayStub(t), newGatewayStub(t)
	tokens := filepath.Join(t.TempDir(), "tokens.json")
	raw, _ := json.Marshal(map[string]string{
		a.ts.URL + " ZZ:ZZTESTBUYER": "token-a", b.ts.URL + " ZZ:ZZTESTBUYER": "token-b", "ZZ:ZZTESTBUYER": "token-default",
	})
	if err := os.WriteFile(tokens, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(pool, Config{Knobs: Knobs{AckStatus: "A"}, GatewayURL: fallback.ts.URL + "/", TokensFile: tokens})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })

	for _, reply := range []string{a.ts.URL, b.ts.URL + "/", ""} {
		if code := transmitFrom(t, ts.URL, reply); code != http.StatusOK {
			t.Fatalf("transmission with reply-to %q: HTTP %d", reply, code)
		}
	}
	if got := a.waitFor(t, 1); len(got) != 1 || got[0] != "Bearer token-a" {
		t.Errorf("process A got %v", got)
	}
	if got := b.waitFor(t, 1); len(got) != 1 || got[0] != "Bearer token-b" {
		t.Errorf("process B got %v", got)
	}
	if got := fallback.waitFor(t, 1); len(got) != 1 || got[0] != "Bearer token-default" {
		t.Errorf("GATEWAY_URL got %v", got)
	}
}

// A value in the header that is not a base address is refused and nothing is recorded.
func TestEmulatorRefusesABadReplyAddress(t *testing.T) {
	_, ts, count := newEmulator(t)
	for _, bad := range []string{"ftp://host", "http://user:pw@host:1", "http://host:1/path", "http://host:1/?q=1", "not a url", "http://"} {
		if code := transmitFrom(t, ts.URL, bad); code != http.StatusBadRequest {
			t.Errorf("reply-to %q: HTTP %d, want 400", bad, code)
		}
	}
	if n := count(`SELECT count(*) FROM mock_partner.received`); n != 0 {
		t.Errorf("%d rows recorded for refused transmissions", n)
	}
}

// A letter without a template is a free letter: the subject and the body are kept whole and
// the oracle of a leak can read them; it needs an address and a body.
func TestEmulatorTakesAFreeLetter(t *testing.T) {
	_, ts, count := newEmulator(t)
	free := map[string]string{"tenant_id": "ta", "external_id": "f1", "to": "x@buyer.test", "subject": "Prices", "body": "SKU-1 1.50 CANARY-7"}
	if code, _ := postEmail(t, ts.URL, free); code != http.StatusCreated {
		t.Fatalf("free letter: HTTP %d", code)
	}
	if n := count(`SELECT count(*) FROM mock_partner.received WHERE kind = 'email' AND recipient = 'x@buyer.test' AND template_id IS NULL
	               AND convert_from(body, 'UTF8') = E'Prices\n\nSKU-1 1.50 CANARY-7'`); n != 1 {
		t.Error("the free letter is not stored as written")
	}
	empty := map[string]string{"tenant_id": "ta", "external_id": "f2", "to": "x@buyer.test", "subject": "Prices"}
	if code, _ := postEmail(t, ts.URL, empty); code != http.StatusBadRequest {
		t.Errorf("a free letter without a body: HTTP %d, want 400", code)
	}
}
