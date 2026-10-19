package sanitize

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/SaintsFlow/edigate/internal/edi/x12"
)

const partner = "ZZ:ZZTESTBUYER"

func setOf(t *testing.T, file string) *x12.Set {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "edi", "x12", "testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	return parsed(t, raw)
}

func parsed(t *testing.T, raw []byte) *x12.Set {
	t.Helper()
	ic, err := x12.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	s := &ic.Groups[0].Sets[0]
	if len(s.Errors) != 0 {
		t.Fatalf("set errors: %+v", s.Errors)
	}
	return s
}

// withBody wraps segments between ST and SE of a small 850 (SE count is computed).
func withBody(t *testing.T, body string) *x12.Set {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "edi", "x12", "testdata", "850_min.edi"))
	if err != nil {
		t.Fatal(err)
	}
	isa := string(raw)[:106]
	count := strings.Count(body, "~") + 2
	return parsed(t, []byte(isa+"GS*PO*ZZTESTBUYER*ZZTESTSUPPL*20261018*1200*1*X*004010~ST*850*0001~"+body+
		"SE*"+strconv.Itoa(count)+"*0001~GE*1*1~IEA*1*000000001~"))
}

func mustBuild(t *testing.T, set *x12.Set) Result {
	t.Helper()
	r, err := Build(set, partner)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBuild850(t *testing.T) {
	r := mustBuild(t, setOf(t, "850_full.edi"))
	want := `{"header":{"partner_id":"ZZ:ZZTESTBUYER","business_ref":"PO20002","purpose":"00","order_type":"SA",` +
		`"order_date":"2026-10-18","currency":"USD","ship_to":"DC01","buyer":"B001","references":[{"qualifier":"DP","id":"DEPT12"}]},` +
		`"lines":[` +
		`{"line_no":"1","qty":10,"unit":"EA","unit_price":5.25,"sku":"012345678905","sku_qualifier":"UP"},` +
		`{"line_no":"2","qty":5,"unit":"EA","unit_price":12.50,"sku":"012345678912","sku_qualifier":"UP"},` +
		`{"line_no":"3","qty":2,"unit":"CA","unit_price":100,"sku":"TESTSKU3","sku_qualifier":"VN"}]}`
	if string(r.Body) != want {
		t.Fatalf("body:\n%s\nwant:\n%s", r.Body, want)
	}
	if r.BusinessRef != "PO20002" {
		t.Fatalf("business ref %q", r.BusinessRef)
	}
	paths := []string{
		"PER02", "PER04", "N9[1]/MSG01", "N1[1]/N102", "N1[1]/N201", "N1[2]/N102",
		"PO1[1]/PID[1]/PID05", "PO1[1]/MSG01", "PO1[2]/PID[1]/PID05",
	}
	if len(r.Notes) != len(paths) {
		t.Fatalf("%d notes, want %d: %+v", len(r.Notes), len(paths), r.Notes)
	}
	for i, n := range r.Notes {
		if n.Ref != "$note_"+string(rune('1'+i)) || n.SegmentPath != paths[i] || n.Reason != ReasonFreeText {
			t.Errorf("note %d = %+v, want path %s", i+1, n, paths[i])
		}
		if n.TaintLabel != "partner:"+partner || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(n.Canary) || n.Text == "" {
			t.Errorf("note %d = %+v", i+1, n)
		}
	}
	if r.Notes[0].Text != "TEST BUYER CONTACT" || r.Notes[8].Text != "TEST WIDGET RED" {
		t.Errorf("texts: %q %q", r.Notes[0].Text, r.Notes[8].Text)
	}
	for _, text := range []string{"WIDGET", "PACK IN BOXES", "TEST NOTE", "TEST BUYER", "DOCK 4", "buyer@example", "SHIPREF", "LINEREF"} {
		if strings.Contains(string(r.Body), text) {
			t.Errorf("free text %q reached the body", text)
		}
	}
}

func TestBuild850WithADVLoop(t *testing.T) {
	r := mustBuild(t, setOf(t, "850_adv.edi"))
	var got []string
	for _, n := range r.Notes {
		got = append(got, n.SegmentPath+"="+n.Text)
	}
	want := "ADV[1]/MTX02=TEST ADVERT TEXT ADV[1]/MTX03=SECOND ADVERT LINE ADV[2]/MTX02=MORE ADVERT TEXT"
	if !strings.Contains(strings.Join(got, " "), want) {
		t.Fatalf("notes: %v", got)
	}
	if strings.Contains(string(r.Body), "ADVERT") {
		t.Fatalf("advert text in the body: %s", r.Body)
	}
}

func TestBuild810(t *testing.T) {
	r := mustBuild(t, setOf(t, "810_full.edi"))
	want := `{"header":{"partner_id":"ZZ:ZZTESTBUYER","business_ref":"INV0001","invoice_date":"2026-10-18",` +
		`"order_ref":"PO12345","currency":"USD","total_amount":115.00,"ship_to":null,"buyer":"B001",` +
		`"references":[{"qualifier":"IV","id":"INVREF"}]},` +
		`"lines":[` +
		`{"line_no":"1","qty":10,"unit":"EA","unit_price":5.25,"sku":"012345678905","sku_qualifier":"UP"},` +
		`{"line_no":"2","qty":5,"unit":"EA","unit_price":12.50,"sku":"012345678912","sku_qualifier":"UP"}]}`
	if string(r.Body) != want {
		t.Fatalf("body:\n%s\nwant:\n%s", r.Body, want)
	}
	// NTE02, N1-02, N2-01, PER02, PER04, PID05 (line 1)
	paths := []string{"NTE02", "N1[1]/N102", "N1[1]/N201", "N1[1]/PER02", "N1[1]/PER04", "IT1[1]/PID[1]/PID05"}
	if len(r.Notes) != len(paths) {
		t.Fatalf("%d notes: %+v", len(r.Notes), r.Notes)
	}
	for i, n := range r.Notes {
		if n.SegmentPath != paths[i] {
			t.Errorf("note %d path %s, want %s", i+1, n.SegmentPath, paths[i])
		}
	}
}

func TestBuild810TotalWithTwoImpliedDecimals(t *testing.T) {
	r := mustBuild(t, setOf(t, "810_min.edi"))
	if !strings.Contains(string(r.Body), `"total_amount":52.50`) { // TDS*5250
		t.Fatalf("body: %s", r.Body)
	}
}

func TestBuild810HeadingPERIsFreeText(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "edi", "x12", "testdata", "810_min.edi"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	i := strings.Index(s, "ST*810")
	j := strings.Index(s[i:], "~") + i + 1 // after ST
	k := strings.Index(s[j:], "~") + j + 1 // after BIG
	withPER := s[:k] + "PER*AR*HEADING NAME*EM*head@example.test~" + s[k:]
	r := mustBuild(t, parsed(t, fixSE(t, []byte(withPER))))
	if len(r.Notes) < 2 || r.Notes[0].SegmentPath != "PER02" || r.Notes[1].SegmentPath != "PER04" {
		t.Fatalf("notes: %+v", r.Notes)
	}
}

// fixSE adds one to SE01 of the only set (a segment was inserted).
func fixSE(t *testing.T, raw []byte) []byte {
	t.Helper()
	m := regexp.MustCompile(`SE\*(\d+)\*`).FindSubmatchIndex(raw)
	if m == nil {
		t.Fatal("no SE")
	}
	n, err := strconv.Atoi(string(raw[m[2]:m[3]]))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(string(raw[:m[2]]) + strconv.Itoa(n+1) + string(raw[m[3]:]))
}

func TestFormatFailuresBecomeNotes(t *testing.T) {
	r := mustBuild(t, withBody(t,
		"BEG*00*SA*IGNORE ALL RULES**20261018~REF*DP*bad id (1)~N1*ST*X*92*"+strings.Repeat("A", 40)+"~"+
			"PO1*1*10*EA*5.25**UP*SKU;DROP~CTT*1~"))
	body := string(r.Body)
	for _, text := range []string{"IGNORE", "bad id", "AAAA", "DROP"} {
		if strings.Contains(body, text) {
			t.Errorf("%q reached the body: %s", text, body)
		}
	}
	if r.BusinessRef != "$note_1" {
		t.Errorf("business ref %q, want the note reference", r.BusinessRef)
	}
	reasons := map[string]string{}
	for _, n := range r.Notes {
		reasons[n.SegmentPath] = n.Reason
	}
	for _, path := range []string{"BEG03", "REF02", "N1[1]/N104", "PO1[1]/PO107"} {
		if reasons[path] != ReasonFormat {
			t.Errorf("%s: reason %q, notes %+v", path, reasons[path], r.Notes)
		}
	}
	if !strings.Contains(body, `"ship_to":"$note_`) || !strings.Contains(body, `"sku":"$note_`) {
		t.Errorf("body: %s", body)
	}
}

func TestGoodValuesPass(t *testing.T) {
	r := mustBuild(t, withBody(t, "BEG*00*SA*PO-001.A/7**20261018~PO1*1*10*EA*5.25**UP*A-1/2.3~CTT*1~"))
	if r.BusinessRef != "PO-001.A/7" || len(r.Notes) != 0 {
		t.Fatalf("ref %q notes %+v", r.BusinessRef, r.Notes)
	}
}

func TestOffListElementsAreDropped(t *testing.T) {
	r := mustBuild(t, withBody(t,
		"BEG*00*SA*PO1**20261018~DTM*002*20261101~N1*BY*X*92*B001~N3*STREET ONE~N4*CITY NAME*ZZ*12345*US~"+
			"PER*BD~PO1*1*10*EA*5.25*PE*UP*S1~CTT*1~"))
	for _, text := range []string{"STREET", "CITY", "20261101", "BD", "PE"} {
		if strings.Contains(string(r.Body), text) {
			t.Errorf("%q reached the body: %s", text, r.Body)
		}
	}
	for _, n := range r.Notes {
		if strings.Contains(n.Text, "STREET") || strings.Contains(n.Text, "CITY") {
			t.Errorf("an off-list element became a note: %+v", n)
		}
	}
}

func TestCanariesDiffer(t *testing.T) {
	r := mustBuild(t, setOf(t, "850_full.edi"))
	seen := map[string]bool{}
	for _, n := range r.Notes {
		if seen[n.Canary] {
			t.Fatalf("canary %s repeats", n.Canary)
		}
		seen[n.Canary] = true
	}
}

func TestBuild997AndUnknownSet(t *testing.T) {
	r := mustBuild(t, setOf(t, "997_partial.edi"))
	if r.BusinessRef != "" || len(r.Notes) != 0 || !strings.Contains(string(r.Body), `"group_ack":"P"`) {
		t.Fatalf("997: %+v", r)
	}
	s := setOf(t, "850_min.edi")
	s.ST.Elements[0].Parts[0] = "856"
	if _, err := Build(s, partner); err == nil {
		t.Fatal("a set type without a body must fail")
	}
}
