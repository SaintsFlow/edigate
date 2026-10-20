package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SaintsFlow/edigate/internal/edi/outbound"
	"github.com/SaintsFlow/edigate/internal/store/storetest"
)

func newEmulator(t *testing.T) (*Server, *httptest.Server, func(q string) int) {
	t.Helper()
	_, pool := storetest.New(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s := New(pool, Config{Knobs: Knobs{AckStatus: "none"}, GatewayURL: "http://127.0.0.1:1", TokensFile: "/nonexistent"})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	count := func(q string) int {
		var n int
		if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return s, ts, count
}

func asn855() []byte {
	raw, err := outbound.Build(outbound.Spec{
		ST01: "855", PartnerID: "ZZ:ZZTESTBUYER", Ref: "PO-1", OrderDate: "20261018", Currency: "USD", AckType: "AC",
		Lines: []outbound.Line{{SKU: "SKU-1", Qty: "1.000", Unit: "EA", UnitPrice: "1.00", Status: "IA"}},
	}, outbound.Envelope{Sender: "ZZ:ZZTESTSUPPL", Receiver: "ZZ:ZZTESTBUYER", ISA13: "000000007", GS06: "7", ST02: "0007", Now: time.Now()})
	if err != nil {
		panic(err)
	}
	return raw
}

func transmit(t *testing.T, url string, body []byte) (int, map[string]string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/as2", bytes.NewReader(body))
	req.Header.Set("AS2-From", "ZZ:ZZTESTSUPPL")
	req.Header.Set("AS2-To", "ZZ:ZZTESTBUYER")
	req.Header.Set("Message-Id", "<m1@edigate>")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// Every transmission is recorded, also a repeat with the same ISA13; the MDN names our
// Message-Id; a transmission that does not parse gets an error MDN and is not recorded.
func TestEmulatorRecordsEveryTransmission(t *testing.T) {
	_, ts, count := newEmulator(t)
	body := asn855()
	for range 2 {
		code, mdn := transmit(t, ts.URL, body)
		if code != http.StatusOK || mdn["disposition"] != "processed" || mdn["original_message_id"] != "<m1@edigate>" || len(mdn["mic_sha256"]) != 64 {
			t.Fatalf("mdn: %d %v", code, mdn)
		}
	}
	if n, d := count(`SELECT count(*) FROM mock_partner.received`), count(`SELECT count(DISTINCT isa13) FROM mock_partner.received`); n != 2 || d != 1 {
		t.Errorf("rows %d, different ISA13 %d; want 2 and 1", n, d)
	}
	bad := bytes.Replace(body, []byte("BAK*00*AC*PO-1"), []byte("BAK*00*AC"), 1)
	_, mdn := transmit(t, ts.URL, bad)
	if !strings.Contains(mdn["disposition"], "Error") {
		t.Errorf("a broken document must get an error MDN: %v", mdn)
	}
	if n := count(`SELECT count(*) FROM mock_partner.received`); n != 2 {
		t.Errorf("a broken document was recorded: %d rows", n)
	}
}

// The knobs change at run time, only from the stand's addresses, and only to values in range.
func TestEmulatorControl(t *testing.T) {
	s, ts, _ := newEmulator(t)
	post := func(body string) int {
		resp, err := http.Post(ts.URL+"/control", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(`{"ack_status":"R","ack_delay_ms":5,"mdn_delay_ms":6,"mdn_drop":0.5}`); code != http.StatusOK {
		t.Fatalf("control: %d", code)
	}
	if k := s.current(); k.AckStatus != "R" || k.AckDelayMS != 5 || k.MDNDelayMS != 6 || k.MDNDropRate != 0.5 {
		t.Errorf("knobs: %+v", k)
	}
	for _, bad := range []string{`{"ack_status":"X"}`, `{"mdn_drop":2}`, `{"ack_delay_ms":-1}`, `{"nope":1}`} {
		if code := post(bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", bad, code)
		}
	}
	if k := s.current(); k.AckStatus != "R" {
		t.Errorf("a refused change altered the knobs: %+v", k)
	}
	// An address outside the stand is refused.
	req := httptest.NewRequest(http.MethodPost, "/control", strings.NewReader(`{"ack_status":"A"}`))
	req.RemoteAddr = "203.0.113.5:4000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("from outside the stand: %d, want 403", rec.Code)
	}
	for addr, want := range map[string]bool{"127.0.0.1:1": true, "172.18.0.5:1": true, "[::1]:1": true, "8.8.8.8:1": false, "bad": false} {
		if fromStand(addr) != want {
			t.Errorf("fromStand(%s) = %v", addr, !want)
		}
	}
}

// The verdicts of the 997: P gives a mixed result for two sets, and A, E, R give one verdict each.
func TestVerdicts(t *testing.T) {
	cases := []struct {
		status string
		n      int
		ak5    string
		ak9    string
		ok     int
	}{
		{"A", 2, "AA", "A", 2}, {"E", 1, "E", "E", 1}, {"R", 2, "RR", "R", 0}, {"P", 3, "ARA", "P", 2}, {"P", 1, "A", "A", 1},
	}
	for _, c := range cases {
		ak5, ak9, ok := verdicts(c.status, c.n)
		if strings.Join(ak5, "") != c.ak5 || ak9 != c.ak9 || ok != c.ok {
			t.Errorf("%s/%d: %v %s %d", c.status, c.n, ak5, ak9, ok)
		}
	}
}
