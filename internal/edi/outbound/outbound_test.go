package outbound

import (
	"strings"
	"testing"
	"time"

	"github.com/SaintsFlow/edigate/internal/edi/x12"
)

var env = Envelope{
	Sender: "ZZ:ZZTESTSUPPL", Receiver: "ZZ:ZZTESTBUYER",
	ISA13: "000000042", GS06: "42", ST02: "0042", Now: time.Date(2026, 10, 19, 12, 30, 0, 0, time.UTC),
}

func spec(st01 string) Spec {
	s := Spec{
		ST01: st01, PartnerID: "ZZ:ZZTESTBUYER", Ref: "PO-1", OrderDate: "20261018", Currency: "USD",
		ShipTo: "DC01", Lines: []Line{
			{SKU: "SKU-1", Qty: "10.000", Unit: "EA", UnitPrice: "7.50", Status: "IA", SSCC: "000000000000000001"},
			{SKU: "SKU-2", Qty: "2.500", Unit: "EA", UnitPrice: "2.00", Status: "IA", SSCC: "000000000000000018"},
		},
	}
	if st01 == "855" {
		s.AckType = "AC"
	} else {
		s.ShipmentID = "SHP-1"
	}
	return s
}

// The 855 and the 856 are accepted by the strict parser, carry the numbers given, and
// the 856 holds one pack and one item for every line.
func TestBuildParses(t *testing.T) {
	for _, st01 := range []string{"855", "856"} {
		raw, err := Build(spec(st01), env)
		if err != nil {
			t.Fatalf("%s: %v", st01, err)
		}
		ic, err := x12.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", st01, err)
		}
		g := ic.Groups[0]
		if ic.ControlNumber() != "000000042" || g.GS.Value(6) != "42" || g.Sets[0].ST.Value(2) != "0042" || g.GS.Value(1) != GroupCode(st01) {
			t.Errorf("%s: numbers %s %s %s", st01, ic.ControlNumber(), g.GS.Value(6), g.Sets[0].ST.Value(2))
		}
		if len(g.Sets[0].Errors) != 0 {
			t.Errorf("%s: %+v", st01, g.Sets[0].Errors)
		}
		if ic.SenderID() != "ZZ:ZZTESTSUPPL" || ic.ReceiverID() != "ZZ:ZZTESTBUYER" {
			t.Errorf("%s: ids %s %s", st01, ic.SenderID(), ic.ReceiverID())
		}
	}
	raw, _ := Build(spec("856"), env)
	if n := strings.Count(string(raw), "MAN*GM*"); n != 2 {
		t.Errorf("packs = %d, want 2", n)
	}
	if !strings.Contains(string(raw), "SN1**2.5*EA~") || !strings.Contains(string(raw), "SN1**10*EA~") {
		t.Errorf("quantities are not written as the partner reads them: %s", raw)
	}
}

func TestBuildRefusesBadValues(t *testing.T) {
	s := spec("855")
	s.Ref = "PO*1"
	if _, err := Build(s, env); err == nil {
		t.Error("a separator in the reference must be refused")
	}
	s = spec("855")
	s.Lines[0].Status = ""
	if _, err := Build(s, env); err == nil {
		t.Error("an ACK without a status must be refused by the parser")
	}
	s = spec("855")
	s.Lines = nil
	if _, err := Build(s, env); err == nil {
		t.Error("a document without lines must be refused")
	}
}

func TestAmountSkipsRejectedLines(t *testing.T) {
	s := spec("855")
	if got, err := s.AmountCents(); err != nil || got != 8000 { // 10 x 7.50 + 2.5 x 2.00
		t.Fatalf("amount = %d, %v", got, err)
	}
	s.Lines[1].Status = "IR"
	if got, _ := s.AmountCents(); got != 7500 {
		t.Errorf("amount without the rejected line = %d", got)
	}
}
