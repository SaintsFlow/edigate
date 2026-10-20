package server

// Wave 2-9c: the gateway sends a 997 for every group of an incoming 850 or 810. The emulator keeps
// it in mock_partner.received with st01 997, answers with an MDN, and never answers it with a 997.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SaintsFlow/edigate/internal/edi/outbound"
	"github.com/SaintsFlow/edigate/internal/store/storetest"
)

func ack997() []byte {
	raw, err := outbound.Build997(outbound.AckSpec{
		PartnerID: "ZZ:ZZTESTBUYER", GroupCode: "PO", GroupControl: "1", Sets: []outbound.AckSet{{ID: "850", Control: "0001"}},
	}, outbound.Envelope{Sender: "ZZ:ZZTESTSUPPL", Receiver: "ZZ:ZZTESTBUYER", ISA13: "000000011", GS06: "11", ST02: "0011", Now: time.Now()})
	if err != nil {
		panic(err)
	}
	return raw
}

func TestEmulatorKeepsA997AndDoesNotAnswerIt(t *testing.T) {
	_, pool := storetest.New(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts.Add(1) }))
	defer gateway.Close()
	tokens := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(tokens, []byte(`{"ZZ:ZZTESTBUYER":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(pool, Config{Knobs: Knobs{AckStatus: "A"}, GatewayURL: gateway.URL, TokensFile: tokens})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	code, mdn := transmit(t, ts.URL, ack997())
	if code != http.StatusOK || mdn["disposition"] != "processed" {
		t.Fatalf("the MDN of a 997: %d %v", code, mdn)
	}
	s.Close() // waits for the 997 that are on their way
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mock_partner.received WHERE st01 = '997' AND isa13 = '000000011' AND st02 = '0011'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("stored 997 rows: %d %v, want 1", n, err)
	}
	if got := posts.Load(); got != 0 {
		t.Fatalf("the emulator answered a 997 with %d posts to the gateway", got)
	}

	// An 855 is still answered, so the silence above is about the 997 and not about the knob.
	transmit(t, ts.URL, asn855())
	s.Close()
	if got := posts.Load(); got != 1 {
		t.Fatalf("the 855 was answered with %d posts, want 1", got)
	}
}
