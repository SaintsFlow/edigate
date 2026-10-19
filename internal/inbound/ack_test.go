package inbound_test

// Wave 2-9c: the 997 the gateway sends back for every group of an incoming 850 or 810.
// It is queued in the transaction that stores the interchange, with control numbers from the
// same counter as the 855 and the 856, and it reports syntax only.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/SaintsFlow/edigate/internal/edi/x12"
)

// sentAcks returns the 997 queued for a partner, parsed with the strict parser, oldest first.
func (r *rig) sentAcks(partnerID string) []*x12.Interchange {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(),
		`SELECT raw FROM edi_interchanges WHERE direction = 'out' AND counterparty_id = $1 ORDER BY isa13`, partnerID)
	r.must(err)
	defer rows.Close()
	var out []*x12.Interchange
	for rows.Next() {
		var raw []byte
		r.must(rows.Scan(&raw))
		ic, err := x12.Parse(raw)
		r.must(err)
		out = append(out, ic)
	}
	r.must(rows.Err())
	return out
}

// segments writes the segments of the only set of the 997 as text, one per line.
func segments(ic *x12.Interchange) string {
	var b strings.Builder
	for _, s := range ic.Groups[0].Sets[0].Segments {
		b.WriteString(s.ID)
		for _, e := range s.Elements {
			b.WriteString("*" + strings.Join(e.Parts, ">"))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestT_PARSE_01_RefusedElementGetsA997WithAK5RAndAK4(t *testing.T) {
	r := newRig(t)
	me, tok := r.fresh("ta", "customer")
	body := exchange(t, "850_min.edi", me, "000000901")
	body = bytes.Replace(body, []byte("N1*BY*TEST BUYER*92*B001~"), []byte("N1*BY*TEST*BUYER*92*B001~"), 1)

	wantStatus(t, r.post(tok, me, gatewayID, "<a1@test>", body), 200)
	acks := r.sentAcks(me)
	if len(acks) != 1 {
		t.Fatalf("%d acknowledgments, want 1", len(acks))
	}
	ack := acks[0]
	if ack.SenderID() != gatewayID || ack.ReceiverID() != me || ack.Groups[0].GS.Value(1) != "FA" || ack.Groups[0].Sets[0].ST.Value(1) != "997" {
		t.Fatalf("envelope: %s -> %s", ack.SenderID(), ack.ReceiverID())
	}
	if errs := ack.Groups[0].Sets[0].Errors; len(errs) != 0 {
		t.Fatalf("the 997 of the gateway has set errors: %+v", errs)
	}
	got := segments(ack)
	// N1 is the third segment of the set (ST, BEG, N1) and its own loop. The split name makes
	// N103 too long ("BUYER" for ID 66) and the shifted value makes N104 too long too.
	for _, want := range []string{"AK1*PO*1\n", "AK2*850*0001\n", "AK3*N1*3*N1*8\n", "AK4*3*66*5*BUYER\n", "AK5*R*5\n", "AK9*R*1*1*0\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the 997 lacks %q:\n%s", want, got)
		}
	}
	if n := r.count(`SELECT count(*) FROM outbox WHERE effect_type = 'edi.send_997' AND status = 'pending' AND idem_key IS NULL`); n != 1 {
		t.Errorf("%d pending 997 in the outbox, want 1", n)
	}
	if n := r.count(`SELECT count(*) FROM outbound_docs WHERE st01 = '997' AND state = 'queued' AND saga_id IS NULL AND partner_id = $1`, me); n != 1 {
		t.Errorf("%d queued 997 documents, want 1", n)
	}
}

func TestT_PARSE_01_TwoSetsOneRefusedGiveAK9P(t *testing.T) {
	r := newRig(t)
	me, tok := r.fresh("ta", "customer")
	body := bytes.Replace(exchange(t, "850_two_sets.edi", me, "000000902"), []byte("SE*8*0002~"), []byte("SE*9*0002~"), 1)

	wantStatus(t, r.post(tok, me, gatewayID, "<a2@test>", body), 200)
	acks := r.sentAcks(me)
	if len(acks) != 1 {
		t.Fatalf("%d acknowledgments, want 1", len(acks))
	}
	got := segments(acks[0])
	for _, want := range []string{"AK1*PO*3\n", "AK2*850*0001\nAK5*A\n", "AK2*850*0002\nAK5*R*4\n", "AK9*P*2*2*1\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the 997 lacks %q:\n%s", want, got)
		}
	}
}

// A set refused for the role of the partner is right in syntax: the 997 says A. The refusal
// is in the stored document, not in the acknowledgment.
func TestAckForASetRefusedByRoleIsAccepted(t *testing.T) {
	r := newRig(t)
	me, tok := r.fresh("ta", "supplier") // a supplier may send an 810 and a 997, not an 850
	wantStatus(t, r.post(tok, me, gatewayID, "<a3@test>", exchange(t, "850_min.edi", me, "000000903")), 200)
	if n := r.count(`SELECT count(*) FROM edi_documents WHERE status = 'rejected' AND errors::text LIKE '%role_not_allowed%'`); n != 1 {
		t.Fatalf("%d sets refused for the role, want 1", n)
	}
	acks := r.sentAcks(me)
	if len(acks) != 1 || !strings.Contains(segments(acks[0]), "AK5*A\n") || !strings.Contains(segments(acks[0]), "AK9*A*1*1*1\n") {
		t.Fatalf("the 997 for a set refused by role: %v", acks)
	}
}

// An 810 gets a 997 as well, with the numbers of the partner's counter: the second reply goes
// out with the next ISA13, GS06 and ST02.
func TestAckNumbersComeFromTheCounterOfThePartner(t *testing.T) {
	r := newRig(t)
	me, tok := r.fresh("ta", "supplier")
	wantStatus(t, r.post(tok, me, gatewayID, "<a4@test>", exchange(t, "810_min.edi", me, "000000904")), 200)
	other := bytes.Replace(exchange(t, "810_min.edi", me, "000000905"), []byte("INV"), []byte("IN2"), 1)
	wantStatus(t, r.post(tok, me, gatewayID, "<a5@test>", other), 200)
	acks := r.sentAcks(me)
	if len(acks) != 2 {
		t.Fatalf("%d acknowledgments, want 2", len(acks))
	}
	for i, want := range []string{"000000001", "000000002"} {
		if acks[i].ControlNumber() != want || acks[i].Groups[0].GS.Value(6) != string(want[len(want)-1]) {
			t.Errorf("ack %d: ISA13 %s GS06 %s", i, acks[i].ControlNumber(), acks[i].Groups[0].GS.Value(6))
		}
	}
	if acks[0].Groups[0].Sets[0].ST.Value(2) != "0001" || acks[1].Groups[0].Sets[0].ST.Value(2) != "0002" {
		t.Error("the ST02 of the replies are not 0001 and 0002")
	}
	if !strings.Contains(segments(acks[0]), "AK1*IN*") {
		t.Errorf("an 810 group is acknowledged with AK1*IN:\n%s", segments(acks[0]))
	}
}

// A repeat of the same interchange gets the same MDN and no second 997; a 997 from the
// partner is stored and never answered.
func TestNoReplyForARepeatNorFor997(t *testing.T) {
	r := newRig(t)
	me, tok := r.fresh("ta", "customer")
	body := exchange(t, "850_min.edi", me, "000000906")
	wantStatus(t, r.post(tok, me, gatewayID, "<a6@test>", body), 200)
	wantStatus(t, r.post(tok, me, gatewayID, "<a6b@test>", body), 200)
	if n := len(r.sentAcks(me)); n != 1 {
		t.Fatalf("%d acknowledgments after a repeat, want 1", n)
	}

	you, tok2 := r.fresh("ta", "customer")
	wantStatus(t, r.post(tok2, you, gatewayID, "<a7@test>", exchange(t, "997_accepted.edi", you, "000000907")), 200)
	if n := r.count(`SELECT count(*) FROM edi_documents WHERE partner_id = $1 AND st01 = '997'`, you); n != 1 {
		t.Fatalf("%d stored 997, want 1", n)
	}
	if n := len(r.sentAcks(you)); n != 0 {
		t.Fatalf("a 997 was answered with %d acknowledgments", n)
	}
}
