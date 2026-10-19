package txn_test

// Wave 2-9c: the 997 of the gateway for an incoming 850 goes out through the relay like any
// document: AS2 with an MDN, the emulator keeps it in mock_partner.received (st01 = '997') and
// never answers it with a 997. It is delivered when its MDN counts and belongs to no saga.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SaintsFlow/edigate/internal/adapters"
	"github.com/SaintsFlow/edigate/internal/txn"
)

// The open modes (plain, reqhash) run a relay that delivers the 997 and nothing else: the 997 is a property
// of the intake, the same in every mode, and the effects of those modes do not go through the outbox.
func TestRelayOfTheOpenModesDeliversOnlyThe997(t *testing.T) {
	s := newSagaRig(t)
	s.keepAcks = true
	doc, _ := s.newOrder()
	a := s.tool("erp.create_sales_order", map[string]any{"source_document_id": doc}) // a pending onec.create_order row
	if a.Status != "accepted" {
		t.Fatalf("order: %+v", a)
	}
	open := &txn.Relay{Store: s.st, Workers: 1, AS2: s.relay.AS2, Only: []string{adapters.EffectSend997}}
	for range 5 {
		if _, err := open.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.count(`SELECT count(*) FROM outbound_docs WHERE st01 = '997' AND state = 'delivered'`); n != 1 {
		t.Errorf("delivered 997 = %d, want 1", n)
	}
	if n := s.count(`SELECT count(*) FROM outbox WHERE effect_type = 'onec.create_order' AND status = 'pending'`); n != 1 {
		t.Errorf("the order row was touched by the relay of the open modes: pending rows = %d, want 1", n)
	}
}

func TestAckOf850IsSentWithAnMDNAndNeverAnswered(t *testing.T) {
	s := newSagaRig(t)
	s.keepAcks = true
	s.newOrder()

	s.until("the 997 is delivered", func() bool {
		return s.count(`SELECT count(*) FROM outbound_docs WHERE st01 = '997' AND state = 'delivered' AND mdn_ok`) == 1
	})
	if n := s.count(`SELECT count(*) FROM mock_partner.received WHERE st01 = '997' AND kind = 'as2'`); n != 1 {
		t.Fatalf("the partner got %d 997, want 1", n)
	}
	var body string
	s.must(s.pool.QueryRow(context.Background(), `SELECT convert_from(body, 'UTF8') FROM mock_partner.received WHERE st01 = '997'`).Scan(&body))
	if !strings.Contains(body, "AK5*A~") || !strings.Contains(body, "AK9*A*1*1*1~") {
		t.Errorf("the 997 the partner got: %s", body)
	}
	// Give an answer time to arrive (the emulator answers on its own thread): none may come.
	time.Sleep(400 * time.Millisecond)
	if n := s.count(`SELECT count(*) FROM edi_documents WHERE st01 = '997'`); n != 0 {
		t.Fatalf("the partner answered the 997 with %d 997", n)
	}
	if n := s.count(`SELECT count(*) FROM outbound_docs WHERE st01 = '997' AND saga_id IS NOT NULL`); n != 0 {
		t.Errorf("a 997 belongs to a saga")
	}
}
