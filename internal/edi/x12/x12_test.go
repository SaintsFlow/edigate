package x12

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func testdata(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// change replaces the first occurrence of old in a test file.
func change(t testing.TB, name, old, replacement string) []byte {
	t.Helper()
	s := string(testdata(t, name))
	if !strings.Contains(s, old) {
		t.Fatalf("%s has no %q", name, old)
	}
	return []byte(strings.Replace(s, old, replacement, 1))
}

func mustParse(t testing.TB, raw []byte) *Interchange {
	t.Helper()
	ic, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return ic
}

func wantEnvelope(t *testing.T, raw []byte, reason string) {
	t.Helper()
	_, err := Parse(raw)
	var ee *EnvelopeError
	if !errors.As(err, &ee) {
		t.Fatalf("want an EnvelopeError, got %v", err)
	}
	if reason != "" && ee.Reason != reason {
		t.Fatalf("envelope reason = %q (%s), want %q", ee.Reason, ee.Detail, reason)
	}
}

// onlySet returns the single set of a one-group, one-set interchange.
func onlySet(t *testing.T, raw []byte) *Set {
	t.Helper()
	ic := mustParse(t, raw)
	if len(ic.Groups) != 1 || len(ic.Groups[0].Sets) != 1 {
		t.Fatalf("want one group with one set")
	}
	return &ic.Groups[0].Sets[0]
}

// wantRejected requires a rejected set and one error with the given codes.
func wantRejected(t *testing.T, s *Set, ak304, ak403 string) {
	t.Helper()
	if len(s.Errors) == 0 {
		t.Fatal("the set was accepted")
	}
	for _, e := range s.Errors {
		if (ak304 == "" || e.AK304 == ak304) && (ak403 == "" || e.AK403 == ak403) {
			return
		}
	}
	t.Fatalf("no error with AK304=%q AK403=%q in %+v", ak304, ak403, s.Errors)
}

var testFiles = []string{
	"850_min.edi", "850_full.edi", "850_two_sets.edi", "two_groups_850_997.edi", "810_full.edi",
	"810_min.edi", "997_accepted.edi", "997_rejected.edi", "997_partial.edi", "850_alt_delims.edi",
}

// K3: every reference file parses without errors and is written back byte for byte.
func TestRoundTrip(t *testing.T) {
	for _, name := range testFiles {
		t.Run(name, func(t *testing.T) {
			raw := testdata(t, name)
			ic := mustParse(t, raw)
			for _, g := range ic.Groups {
				for _, s := range g.Sets {
					if len(s.Errors) != 0 {
						t.Fatalf("set %s has errors %+v", s.ST.Value(2), s.Errors)
					}
				}
			}
			if got := Write(ic); !bytes.Equal(got, raw) {
				t.Fatalf("Write(Parse(f)) differs from f:\n%s\n%s", got, raw)
			}
		})
	}
}

func TestParseStructure(t *testing.T) {
	ic := mustParse(t, testdata(t, "850_full.edi"))
	if ic.SenderID() != "ZZ:ZZTESTBUYER" || ic.ReceiverID() != "ZZ:ZZTESTSUPPL" || ic.ControlNumber() != "000000002" {
		t.Fatalf("ids: %s %s %s", ic.SenderID(), ic.ReceiverID(), ic.ControlNumber())
	}
	s := ic.Groups[0].Sets[0]
	if s.ST.Position != 1 || s.Segments[0].Position != 2 || s.SE.Position != 24 {
		t.Fatalf("positions: %d %d %d", s.ST.Position, s.Segments[0].Position, s.SE.Position)
	}
	paths := map[int]string{}
	for _, seg := range s.Segments {
		paths[seg.Position] = seg.Path
	}
	want := map[int]string{
		2: "BEG", 8: "N9[1]/MSG", 9: "N1[1]/N1", 11: "N1[1]/N3", 13: "N1[2]/N1", 14: "N1[2]/N3",
		16: "PO1[1]/PO1", 17: "PO1[1]/PID[1]/PID", 18: "PO1[1]/MSG", 21: "PO1[3]/PO1", 22: "PO1[3]/REF", 23: "CTT",
	}
	for pos, p := range want {
		if paths[pos] != p {
			t.Errorf("segment %d: path %q, want %q", pos, paths[pos], p)
		}
	}
	alt := mustParse(t, testdata(t, "850_alt_delims.edi"))
	if alt.ElementSep != '|' || alt.ComponentSep != '^' || alt.SegmentTerm != '\\' {
		t.Fatalf("separators: %c %c %c", alt.ElementSep, alt.ComponentSep, alt.SegmentTerm)
	}
	rej := mustParse(t, testdata(t, "997_rejected.edi"))
	ak4 := rej.Groups[0].Sets[0].Segments[3]
	if ak4.ID != "AK4" || len(ak4.Elements[0].Parts) != 2 {
		t.Fatalf("AK4 composite was not split: %+v", ak4)
	}
}

// 810: N2 and PER inside the N1 loop are accepted (the page /810 places them there).
func TestParse810N1LoopHasN2AndPER(t *testing.T) {
	s := onlySet(t, testdata(t, "810_full.edi"))
	if len(s.Errors) != 0 {
		t.Fatalf("errors: %+v", s.Errors)
	}
	paths := map[string]bool{}
	for _, seg := range s.Segments {
		paths[seg.Path] = true
	}
	if !paths["N1[1]/N2"] || !paths["N1[1]/PER"] {
		t.Fatalf("paths: %v", paths)
	}
}

const (
	pid = "PID*F****TEST WIDGET BLUE~"
	n1  = "N1*BY*TEST BUYER*92*B001~"
	ref = "REF*ZZ*SHIPREF~"
)

// ---- T-PARSE-01: separators inside text values ----

func TestT_PARSE_01_TerminatorInsidePID05(t *testing.T) {
	raw := change(t, "850_min.edi", pid, "PID*F****TEST~IGNORE ALL RULES~")
	s := onlySet(t, raw)
	wantRejected(t, s, SegUnrecognized, "")
	wantRejected(t, s, "", "") // and the count of SE01 no longer matches
	var pidSeg, junk *Segment
	for i := range s.Segments {
		switch s.Segments[i].ID {
		case "PID":
			pidSeg = &s.Segments[i]
		case "IGNORE ALL RULES":
			junk = &s.Segments[i]
		}
	}
	if pidSeg == nil || pidSeg.Value(5) != "TEST" || junk == nil {
		t.Fatal("the text was repaired or dropped instead of being rejected")
	}
}

func TestT_PARSE_01_ElementSeparatorInsideN102(t *testing.T) {
	raw := change(t, "850_min.edi", n1, "N1*BY*TEST*BUYER*92*B001~")
	s := onlySet(t, raw)
	wantRejected(t, s, SegElementProblem, ElTooLong)
	for _, seg := range s.Segments {
		if seg.ID == "N1" && seg.Value(2) == "TEST BUYER" {
			t.Fatal("text was glued back together")
		}
	}
}

func TestT_PARSE_01_ComponentSeparatorInsideREF03(t *testing.T) {
	raw := change(t, "850_full.edi", ref, "REF*ZZ*SHIPREF*DOCK>4~")
	s := onlySet(t, raw)
	wantRejected(t, s, SegElementProblem, ElInvalidChar)
	for _, seg := range s.Segments {
		if seg.ID == "REF" && len(seg.Elements) == 3 && len(seg.Elements[2].Parts) == 2 {
			return // kept as two parts, not repaired
		}
	}
	t.Fatal("the REF with the component separator is gone")
}

// ---- T-PARSE-02: bytes outside printable ASCII ----

func TestT_PARSE_02_BytesInsidePID05(t *testing.T) {
	bad := map[string]string{
		"NUL": "\x00", "LF": "\x0a", "CR": "\x0d", "ESC": "\x1b", "DEL": "\x7f", "UTF-8 e-acute": "\xc3\xa9",
	}
	for name, b := range bad {
		t.Run(name, func(t *testing.T) {
			raw := change(t, "850_min.edi", pid, "PID*F****TEST"+b+"TEXT~")
			s := onlySet(t, raw)
			wantRejected(t, s, SegElementProblem, ElInvalidChar)
			for _, e := range s.Errors {
				if e.BadValue != Printable(e.BadValue) {
					t.Errorf("bad value is not printable: %q", e.BadValue)
				}
			}
		})
	}
}

func TestT_PARSE_02_BytesInsideGS(t *testing.T) {
	for _, b := range []string{"\x00", "\x0a", "\x0d", "\x1b", "\x7f", "\xc3\xa9"} {
		raw := change(t, "850_min.edi", "GS*PO*ZZTESTBUYER*", "GS*PO*ZZTEST"+b+"BUYER*")
		wantEnvelope(t, raw, ReasonCharset)
	}
}

func TestT_PARSE_02_ByteInsideISAAndAfterIEA(t *testing.T) {
	wantEnvelope(t, change(t, "850_min.edi", "ZZTESTSUPPL    *", "ZZTESTSUPP\x01    *"), ReasonCharset)
	wantEnvelope(t, append(testdata(t, "850_min.edi"), '\n'), ReasonStructure) // not even a final newline
}

// ---- T-PARSE-03: envelope and counts ----

func TestT_PARSE_03_IsaFaults(t *testing.T) {
	cases := map[string][]byte{
		"ISA of 105 bytes":    change(t, "850_min.edi", "ZZTESTSUPPL    *", "ZZTESTSUPPL   *"),
		"ISA of 107 bytes":    change(t, "850_min.edi", "ZZTESTSUPPL    *", "ZZTESTSUPPL     *"),
		"ISA12 is 00501":      change(t, "850_min.edi", "*00401*", "*00501*"),
		"ISA11 is ^":          change(t, "850_min.edi", "*U*00401", "*^*00401"),
		"ISA14 is 2":          change(t, "850_min.edi", "*000000001*0*T*", "*000000001*2*T*"),
		"ISA15 is X":          change(t, "850_min.edi", "*0*T*>", "*0*X*>"),
		"ISA13 has a letter":  change(t, "850_min.edi", "*000000001*0*T", "*00000000A*0*T"),
		"separators equal":    change(t, "850_min.edi", "*T*>~", "*T***"),
		"letter as separator": change(t, "850_min.edi", "*T*>~", "*T*A~"),
		"digit as terminator": change(t, "850_min.edi", "*T*>~", "*T*>5"),
		"ISA09 not a date":    change(t, "850_min.edi", "*261018*", "*261318*"),
		"ISA10 not a time":    change(t, "850_min.edi", "*1200*U", "*2500*U"),
		"too short":           []byte("ISA*00*"),
		"empty":               nil,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) { wantEnvelope(t, raw, "") })
	}
}

func TestT_PARSE_03_SetCountsRejectTheSet(t *testing.T) {
	s := onlySet(t, change(t, "850_min.edi", "SE*8*0001", "SE*9*0001"))
	wantRejected(t, s, "", "")
	if s.Errors[0].AK502 != SetSegmentCount {
		t.Fatalf("AK502 = %s, want %s", s.Errors[0].AK502, SetSegmentCount)
	}
	s = onlySet(t, change(t, "850_min.edi", "SE*8*0001", "SE*8*0002"))
	if len(s.Errors) != 1 || s.Errors[0].AK502 != SetControlMismatch {
		t.Fatalf("errors: %+v", s.Errors)
	}
}

func TestT_PARSE_03_GroupAndInterchangeCounts(t *testing.T) {
	wantEnvelope(t, change(t, "850_min.edi", "GE*1*1~", "GE*2*1~"), ReasonCounts)
	wantEnvelope(t, change(t, "850_min.edi", "GE*1*1~", "GE*1*2~"), ReasonCounts)
	wantEnvelope(t, change(t, "850_min.edi", "IEA*1*", "IEA*2*"), ReasonCounts)
	wantEnvelope(t, change(t, "850_min.edi", "IEA*1*000000001", "IEA*1*000000002"), ReasonCounts)
}

func TestEnvelopeStructure(t *testing.T) {
	wantEnvelope(t, change(t, "850_min.edi", "SE*8*0001~", ""), ReasonStructure)                    // no SE
	wantEnvelope(t, change(t, "850_min.edi", "GE*1*1~", ""), ReasonStructure)                       // no GE
	wantEnvelope(t, change(t, "850_min.edi", "IEA*1*000000001~", ""), ReasonStructure)              // no IEA
	wantEnvelope(t, change(t, "850_min.edi", "GS*PO*", "GS*PO*X*"), ReasonGS)                       // GS too long
	wantEnvelope(t, change(t, "850_min.edi", "*004010~", "*005010~"), ReasonGS)                     // wrong version
	wantEnvelope(t, change(t, "850_min.edi", "*20261018*1200*1*X", "*20261318*1200*1*X"), ReasonGS) // GS04 not a date
	wantEnvelope(t, change(t, "850_min.edi", "IEA*1*000000001~", "IEA*1*000000001*~"), ReasonStructure)
	two := change(t, "850_two_sets.edi", "GE*2*3~", "GE*2*3~GS*PO*ZZTESTBUYER*ZZTESTSUPPL*20261018*1200*3*X*004010~GE*0*3~")
	wantEnvelope(t, two, ReasonDuplicate)
}

// ---- T-PARSE-04: limits ----

func TestT_PARSE_04_SegmentLimit(t *testing.T) {
	isa := string(testdata(t, "850_min.edi"))[:106]
	wantEnvelope(t, []byte(isa+strings.Repeat("REF*ZZ*X~", 100000)), ReasonLimit)
}

// A set that makes exactly 100 000 segments in the interchange is accepted.
func TestT_PARSE_04_SegmentLimitBoundary(t *testing.T) {
	const lines = 99992 // 8 other segments (ISA, GS, ST, BEG, CTT, SE, GE, IEA) + lines = 100 000
	var b strings.Builder
	b.WriteString(string(testdata(t, "850_min.edi"))[:106])
	b.WriteString("GS*PO*ZZTESTBUYER*ZZTESTSUPPL*20261018*1200*1*X*004010~ST*850*0001~BEG*00*SA*PO12345**20261018~")
	b.WriteString(strings.Repeat("PO1*1*1*EA~", lines))
	b.WriteString("CTT*1~SE*99996*0001~GE*1*1~IEA*1*000000001~")
	s := onlySet(t, []byte(b.String()))
	if len(s.Errors) != 0 {
		t.Fatalf("errors: %+v", s.Errors[:1])
	}
}

func TestT_PARSE_04_SizeLimit(t *testing.T) {
	wantEnvelope(t, bytes.Repeat([]byte("A"), MaxBytes+1), ReasonLimit)
}

func TestT_PARSE_04_ElementLongerThanDictionary(t *testing.T) {
	raw := change(t, "850_min.edi", "BEG*00*SA*PO12345*", "BEG*00*SA*PO123456789012345678901234*")
	wantRejected(t, onlySet(t, raw), SegElementProblem, ElTooLong)
}

func TestT_PARSE_04_ErrorsAreCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString(string(testdata(t, "850_min.edi"))[:106])
	b.WriteString("GS*PO*ZZTESTBUYER*ZZTESTSUPPL*20261018*1200*1*X*004010~ST*850*0001~BEG*00*SA*PO12345**20261018~")
	b.WriteString(strings.Repeat("REF*ZZ*X*Y*Z~", 500))
	b.WriteString("CTT*1~SE*504*0001~GE*1*1~IEA*1*000000001~")
	s := onlySet(t, []byte(b.String()))
	if len(s.Errors) != maxErrors {
		t.Fatalf("%d errors, want the cap %d", len(s.Errors), maxErrors)
	}
}

func FuzzParse(f *testing.F) {
	for _, name := range testFiles {
		f.Add(testdata(f, name))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		ic, err := Parse(data)
		if err != nil {
			return
		}
		for _, g := range ic.Groups {
			for _, s := range g.Sets {
				if len(s.Errors) != 0 {
					return
				}
			}
		}
		out := Write(ic)
		again, err := Parse(out)
		if err != nil {
			t.Fatalf("Parse(Write(ic)) failed: %v", err)
		}
		if !reflect.DeepEqual(ic, again) {
			t.Fatal("Parse(Write(ic)) is not equal to ic")
		}
	})
}

// ---- grammar ----

// wrap builds a one-set interchange around body (segments ending in "~"); SE01
// is computed. gs01 and st01 are given so that mismatches can be tested.
func wrap(t testing.TB, gs01, st01, body string) []byte {
	t.Helper()
	isa := string(testdata(t, "850_min.edi"))[:106]
	count := strings.Count(body, "~") + 2
	return []byte(isa + "GS*" + gs01 + "*ZZTESTBUYER*ZZTESTSUPPL*20261018*1200*1*X*004010~ST*" + st01 + "*0001~" +
		body + "SE*" + strconv.Itoa(count) + "*0001~GE*1*1~IEA*1*000000001~")
}

const order850 = "BEG*00*SA*PO12345**20261018~" + n1 + "PO1*1*10*EA*5.25**UP*012345678905~" + pid + "CTT*1~"

func TestGrammarAcceptsTheMinimalSet(t *testing.T) {
	if s := onlySet(t, wrap(t, "PO", "850", order850)); len(s.Errors) != 0 {
		t.Fatalf("errors: %+v", s.Errors)
	}
}

func TestGrammarFaults(t *testing.T) {
	manyN1 := strings.Repeat(n1, 201)
	cases := []struct {
		name  string
		body  string
		ak304 string
	}{
		{"NTE is not in an 850", "BEG*00*SA*PO12345**20261018~NTE*GEN*A NOTE~" + n1 + "PO1*1*1*EA~CTT*1~", SegNotInSet},
		{"ADV is not supported", "BEG*00*SA*PO12345**20261018~ADV*X~PO1*1*1*EA~CTT*1~", SegUnrecognized},
		{"BEG is missing", n1 + "PO1*1*1*EA~CTT*1~", SegMandatoryMiss},
		{"PO1 is missing", "BEG*00*SA*PO12345**20261018~CTT*0~", SegMandatoryMiss},
		{"CTT is missing", "BEG*00*SA*PO12345**20261018~PO1*1*1*EA~", SegMandatoryMiss},
		{"segment after CTT", order850 + n1, SegNotInSequence},
		{"two BEG", "BEG*00*SA*PO12345**20261018~BEG*00*SA*PO12345**20261018~PO1*1*1*EA~CTT*1~", SegOverMax},
		{"two CUR", "BEG*00*SA*PO12345**20261018~CUR*BY*USD~CUR*BY*USD~PO1*1*1*EA~CTT*1~", SegOverMax},
		{"201 N1 loops", "BEG*00*SA*PO12345**20261018~" + manyN1 + "PO1*1*1*EA~CTT*1~", SegLoopOverMax},
		{"PER in an 850 after N1 is a place error", "BEG*00*SA*PO12345**20261018~N1*BY*X~PO1*1*1*EA~PER*BD~CTT*1~", SegNotInSequence},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantRejected(t, onlySet(t, wrap(t, "PO", "850", c.body)), c.ak304, "")
		})
	}
}

func TestGrammarMissingAndUnplacedPositions(t *testing.T) {
	s := onlySet(t, wrap(t, "PO", "850", "BEG*00*SA*PO12345**20261018~CTT*0~"))
	e := s.Errors[0]
	if e.SegmentID != "PO1" || e.Position != 3 || e.AK304 != SegMandatoryMiss || e.AK502 != SetSegmentsInError {
		t.Fatalf("error: %+v", e)
	}
	s = onlySet(t, wrap(t, "PO", "850", "BEG*00*SA*PO12345**20261018~PO1*1*1*EA~"))
	e = s.Errors[0]
	if e.SegmentID != "CTT" || e.Position != 4 { // SE stands at position 4
		t.Fatalf("error: %+v", e)
	}
}

func TestSetLevelFaults(t *testing.T) {
	s := onlySet(t, wrap(t, "PO", "856", order850))
	if len(s.Errors) == 0 || s.Errors[0].AK502 != SetNotSupported {
		t.Fatalf("856: %+v", s.Errors)
	}
	s = onlySet(t, wrap(t, "IN", "850", order850))
	if len(s.Errors) != 1 || s.Errors[0].AK502 != SetInvalidID {
		t.Fatalf("GS01 does not fit ST01: %+v", s.Errors)
	}
}

func TestDuplicateST02InAGroup(t *testing.T) {
	raw := bytes.Replace(testdata(t, "850_two_sets.edi"), []byte("ST*850*0002~"), []byte("ST*850*0001~"), 1)
	raw = bytes.Replace(raw, []byte("SE*8*0002~"), []byte("SE*8*0001~"), 1)
	sets := mustParse(t, raw).Groups[0].Sets
	if len(sets[0].Errors) != 0 {
		t.Fatalf("first set: %+v", sets[0].Errors)
	}
	if len(sets[1].Errors) != 1 || sets[1].Errors[0].AK502 != SetControlNotUnq {
		t.Fatalf("second set: %+v", sets[1].Errors)
	}
}

// A fault in one set does not stop the others.
func TestOtherSetsAreAccepted(t *testing.T) {
	raw := bytes.Replace(testdata(t, "850_two_sets.edi"), []byte("SE*8*0002~"), []byte("SE*9*0002~"), 1)
	sets := mustParse(t, raw).Groups[0].Sets
	if len(sets[0].Errors) != 0 || len(sets[1].Errors) == 0 {
		t.Fatalf("errors: %+v / %+v", sets[0].Errors, sets[1].Errors)
	}
}

func TestElementFaults(t *testing.T) {
	cases := []struct {
		name string
		seg  string // replaces the BEG segment
		code string
	}{
		{"trailing empty element", "BEG*00*SA*PO12345**20261018*~", ElTooMany},
		{"extra element", "BEG*00*SA*PO12345**20261018*A*B*C*D*E*F*G*H*I~", ElTooMany},
		{"mandatory element missing", "BEG*00*SA~", ElMandatoryMissing},
		{"mandatory element empty", "BEG*00**PO12345**20261018~", ElMandatoryMissing},
		{"element too short", "BEG*0*SA*PO12345**20261018~", ElTooShort},
		{"impossible date", "BEG*00*SA*PO12345**20261340~", ElInvalidDate},
		{"date with letters", "BEG*00*SA*PO12345**2026101A~", ElInvalidChar},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := strings.Replace(order850, "BEG*00*SA*PO12345**20261018~", c.seg, 1)
			s := onlySet(t, wrap(t, "PO", "850", body))
			wantRejected(t, s, SegElementProblem, c.code)
			if s.Errors[0].SegmentID != "BEG" || s.Errors[0].Position != 2 {
				t.Fatalf("error: %+v", s.Errors[0])
			}
		})
	}
	other := map[string]string{
		"CTT*1A~":                ElInvalidChar,
		"CTT*0000001~":           ElTooLong,
		"DTM*002*2561~":          ElTooShort, // an 8-digit DT with 4 digits
		"DTM*002*20261101*2561~": ElInvalidTime,
	}
	for seg, code := range other {
		body := strings.Replace(order850, "CTT*1~", seg, 1)
		if strings.HasPrefix(seg, "DTM") {
			body = "BEG*00*SA*PO12345**20261018~" + seg + n1 + "PO1*1*1*EA~CTT*1~"
		}
		wantRejected(t, onlySet(t, wrap(t, "PO", "850", body)), SegElementProblem, code)
	}
}

func TestPO1NumbersAreChecked(t *testing.T) {
	for _, bad := range []string{"1.2.3", "--1", "1e5", "+5", "1,5"} {
		body := strings.Replace(order850, "PO1*1*10*", "PO1*1*"+bad+"*", 1)
		wantRejected(t, onlySet(t, wrap(t, "PO", "850", body)), SegElementProblem, ElInvalidChar)
	}
	for _, good := range []string{"10", "-3", "0.5", ".5", "7.", "123456789012345"} {
		body := strings.Replace(order850, "PO1*1*10*", "PO1*1*"+good+"*", 1)
		if s := onlySet(t, wrap(t, "PO", "850", body)); len(s.Errors) != 0 {
			t.Errorf("%q: %+v", good, s.Errors)
		}
	}
}

func TestCheckValueTypes(t *testing.T) {
	cases := []struct {
		def  elemDef
		v    string
		want string
	}{
		{el("", mand, tN0, 1, 3), "123", ""},
		{el("", mand, tN0, 1, 3), "-12", ""}, // the sign is not part of the length
		{el("", mand, tN0, 1, 3), "-123", ""},
		{el("", mand, tN0, 1, 3), "1234", ElTooLong},
		{el("", mand, tN0, 2, 3), "1", ElTooShort},
		{el("", mand, tN0, 1, 3), "-", ElInvalidChar},
		{el("", mand, tN2, 1, 15), "5250", ""},
		{el("", mand, tR, 1, 4), "12.34", ""}, // the point is not part of the length
		{el("", mand, tR, 1, 4), "12.345", ElTooLong},
		{el("", mand, tDT, 8, 8), "20240229", ""},
		{el("", mand, tDT, 8, 8), "20230229", ElInvalidDate},
		{el("", mand, tDT, 6, 6), "261018", ""},
		{el("", mand, tDT, 6, 6), "261018", ""},
		{el("", mand, tTM, 4, 8), "2359", ""},
		{el("", mand, tTM, 4, 8), "235959", ""},
		{el("", mand, tTM, 4, 8), "23595999", ""},
		{el("", mand, tTM, 4, 8), "2400", ElInvalidTime},
		{el("", mand, tTM, 4, 8), "1260", ElInvalidTime},
		{el("", mand, tTM, 4, 8), "12345", ElInvalidTime},
		{el("", mand, tAN, 1, 5), "AB CD", ""},
		{el("", mand, tAN, 1, 5), "AB\tCD", ElInvalidChar},
		{el("", mand, tID, 2, 2), "é", ElInvalidChar},
	}
	for _, c := range cases {
		if got := checkValue(c.def, c.v); got != c.want {
			t.Errorf("checkValue(%v, %q) = %q, want %q", c.def.typ, c.v, got, c.want)
		}
	}
}

// Write builds bytes from the structure: it pads the ISA to its fixed width.
func TestWriteFromStructure(t *testing.T) {
	text := func(v string) Element { return Element{Parts: []string{v}} }
	ic := &Interchange{
		ElementSep: '*', ComponentSep: '>', SegmentTerm: '~',
		ISA: Segment{ID: "ISA", Elements: []Element{
			text("00"), text(""), text("00"), text(""), text("ZZ"), text("ZZTESTSUPPL"), text("ZZ"), text("ZZTESTBUYER"),
			text("261018"), text("1200"), text("U"), text("00401"), text("7"), text("0"), text("T"),
		}},
		Groups: []Group{{
			GS: Segment{ID: "GS", Elements: []Element{text("FA"), text("ZZTESTSUPPL"), text("ZZTESTBUYER"), text("20261018"), text("1200"), text("1"), text("X"), text("004010")}},
			Sets: []Set{{
				ST: Segment{ID: "ST", Elements: []Element{text("997"), text("0001")}},
				Segments: []Segment{
					{ID: "AK1", Elements: []Element{text("PO"), text("1")}},
					{ID: "AK2", Elements: []Element{text("850"), text("0001")}},
					{ID: "AK5", Elements: []Element{text("A")}},
					{ID: "AK9", Elements: []Element{text("A"), text("1"), text("1"), text("1")}},
				},
				SE: Segment{ID: "SE", Elements: []Element{text("6"), text("0001")}},
			}},
			GE: Segment{ID: "GE", Elements: []Element{text("1"), text("1")}},
		}},
		IEA: Segment{ID: "IEA", Elements: []Element{text("1"), text("000000007")}},
	}
	raw := Write(ic)
	if len(raw) < 106 || string(raw[:106]) != "ISA*00*          *00*          *ZZ*ZZTESTSUPPL    *ZZ*ZZTESTBUYER    *261018*1200*U*00401*000000007*0*T*>~" {
		t.Fatalf("ISA: %q", raw[:106])
	}
	parsed := mustParse(t, raw)
	if s := parsed.Groups[0].Sets[0]; len(s.Errors) != 0 {
		t.Fatalf("errors: %+v", s.Errors)
	}
	if !bytes.Equal(Write(parsed), raw) {
		t.Fatal("second round differs")
	}
}

func TestPrintable(t *testing.T) {
	if got := Printable("A\x00B\xc3\xa9\x7f"); got != "A?B???" {
		t.Fatalf("got %q", got)
	}
	if got := badValue(strings.Repeat("x", 200)); len(got) != 99 {
		t.Fatalf("bad value length %d", len(got))
	}
}
