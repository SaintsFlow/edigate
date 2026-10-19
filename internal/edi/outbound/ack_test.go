package outbound

import (
	"strings"
	"testing"

	"github.com/SaintsFlow/edigate/internal/edi/x12"
)

func ackBody(t *testing.T, a AckSpec) string {
	t.Helper()
	raw, err := Build997(a, env)
	if err != nil {
		t.Fatal(err)
	}
	ic, err := x12.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ic.Groups[0].Sets[0].Errors) != 0 {
		t.Fatalf("the 997 has set errors: %+v", ic.Groups[0].Sets[0].Errors)
	}
	return string(raw)
}

// Accepted, refused with a segment error, refused for the set as a whole: AK5 and AK9 follow.
func TestBuild997Verdicts(t *testing.T) {
	a := AckSpec{PartnerID: "ZZ:ZZTESTBUYER", GroupCode: "PO", GroupControl: "17", Sets: []AckSet{
		{ID: "850", Control: "0001"},
		{ID: "850", Control: "0002", Errors: []x12.Error{
			{AK502: "5", SegmentID: "N1", Position: 3, LoopID: "N1", AK304: "8", ElementPos: 3, ElementRef: "66", AK403: "5", BadValue: "BUYER"},
			{AK502: "5", SegmentID: "N1", Position: 3, LoopID: "N1", AK304: "8", ElementPos: 5, ElementRef: "706", AK403: "5", BadValue: "B001"},
			{AK502: "5", SegmentID: "PO1", Position: 5, AK304: "3"},
		}},
		{ID: "850", Control: "0003", Errors: []x12.Error{{AK502: "4"}}},
	}}
	got := ackBody(t, a)
	for _, want := range []string{
		"GS*FA*", "AK1*PO*17~", "AK2*850*0001~AK5*A~",
		"AK2*850*0002~AK3*N1*3*N1*8~AK4*3*66*5*BUYER~AK4*5*706*5*B001~AK3*PO1*5**3~AK5*R*5~",
		"AK2*850*0003~AK5*R*4~", "AK9*P*3*3*1~",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the 997 lacks %q:\n%s", want, got)
		}
	}
	a.Sets = a.Sets[:1]
	if got := ackBody(t, a); !strings.Contains(got, "AK9*A*1*1*1~") {
		t.Errorf("all accepted: %s", got)
	}
	a.Sets = []AckSet{{ID: "850", Control: "0003", Errors: []x12.Error{{AK502: "4"}}}}
	if got := ackBody(t, a); !strings.Contains(got, "AK9*R*1*1*0~") {
		t.Errorf("all refused: %s", got)
	}
}

// Values copied from the partner are made to fit: a long or odd ST01, an ST02 with a separator, a
// segment id of seven letters and a bad value with separators must not make the 997 unreadable.
func TestBuild997FitsStrangeValues(t *testing.T) {
	a := AckSpec{PartnerID: "ZZ:ZZTESTBUYER", GroupCode: "PO", GroupControl: "1", Sets: []AckSet{{
		ID: "ABCDEFGHIJKLMNOP", Control: "x*y", Errors: []x12.Error{
			{AK502: "5", SegmentID: "ZZZZZZZ", Position: 2, AK304: "1"},
			{AK502: "5", SegmentID: "REF", Position: 4, AK304: "8", ElementPos: 3, ComponentPos: 2, ElementRef: "352", AK403: "6", BadValue: "a>b*c~d"},
		},
	}}}
	got := ackBody(t, a)
	for _, want := range []string{"AK2*ABC*x?y0~", "AK3*ZZZ*2**1~", "AK4*3>2*352*6*a?b?c?d~"} {
		if !strings.Contains(got, want) {
			t.Errorf("the 997 lacks %q:\n%s", want, got)
		}
	}
}
