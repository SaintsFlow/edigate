package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SaintsFlow/edigate/internal/store/storetest"
)

func newMock(t *testing.T, cfg Config) (*Server, *httptest.Server, func(q string) int) {
	t.Helper()
	_, pool := storetest.New(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s := New(pool, cfg)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	count := func(q string) int {
		var n int
		if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return s, ts, count
}

const body = `{"tenant_id":"ta","external_id":"ab12","order_ref":"PO-1","lines":[{"sku":"SKU-1","qty":"10.000","unit":"EA"},{"sku":"SKU-2","qty":"3","unit":"EA"}]}`

func post(t *testing.T, url, payload string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader([]byte(payload)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

// A repeated reservation changes nothing and answers 200; a release is idempotent and
// does nothing for a reservation that is not there.
func TestReserveAndReleaseAreIdempotent(t *testing.T) {
	_, ts, count := newMock(t, Config{})
	if code, _ := post(t, ts.URL+"/reservations", body); code != http.StatusCreated {
		t.Fatalf("first reserve: %d", code)
	}
	if code, _ := post(t, ts.URL+"/reservations", body); code != http.StatusOK {
		t.Fatalf("second reserve: %d", code)
	}
	if n := count(`SELECT count(*) FROM mock_wms.reservations`); n != 1 {
		t.Errorf("reservations = %d, want 1", n)
	}
	for range 2 {
		if code, _ := post(t, ts.URL+"/reservations/ab12/release", ""); code != http.StatusOK {
			t.Fatalf("release: %d", code)
		}
	}
	if n := count(`SELECT count(*) FROM mock_wms.reservations WHERE status = 'released'`); n != 1 {
		t.Errorf("released = %d, want 1", n)
	}
	if code, _ := post(t, ts.URL+"/reservations/nothing/release", ""); code != http.StatusOK {
		t.Errorf("release of nothing: %d, want 200", code)
	}
}

// A shipment gives one valid SSCC per line, the same every time, and the quantity factor
// changes the shipped quantity.
func TestShipmentFacts(t *testing.T) {
	s, ts, count := newMock(t, Config{})
	s.SetShipQtyFactor("0.5")
	if code, _ := post(t, ts.URL+"/shipments", body); code != http.StatusCreated {
		t.Fatalf("ship: %d", code)
	}
	if code, _ := post(t, ts.URL+"/shipments", body); code != http.StatusOK {
		t.Fatalf("ship again: %d", code)
	}
	if n := count(`SELECT count(*) FROM mock_wms.shipments`); n != 1 {
		t.Errorf("shipments = %d, want 1", n)
	}
	resp, err := http.Get(ts.URL + "/shipments?external_id=ab12")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		Lines []struct{ SKU, Qty string }
		SSCC  []string
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.SSCC) != 2 || got.Lines[0].Qty != "5.000" || got.Lines[1].Qty != "1.500" {
		t.Fatalf("facts: %+v", got)
	}
	for i, label := range got.SSCC {
		if len(label) != 18 || label != SSCC("ab12", i) || !validGS1(label) {
			t.Errorf("SSCC %d = %q", i, label)
		}
	}
	if got.SSCC[0] == got.SSCC[1] {
		t.Error("two lines got the same SSCC")
	}
}

func TestShipmentRefusedByPrefix(t *testing.T) {
	_, ts, count := newMock(t, Config{RejectRefPrefix: "PO-"})
	if code, _ := post(t, ts.URL+"/shipments", body); code != http.StatusUnprocessableEntity {
		t.Fatalf("ship: %d, want 422", code)
	}
	if code, _ := post(t, ts.URL+"/reservations", body); code != http.StatusCreated {
		t.Errorf("a reservation is not refused by the prefix: %d", code)
	}
	if n := count(`SELECT count(*) FROM mock_wms.shipments`); n != 0 {
		t.Errorf("shipments = %d, want 0", n)
	}
}

func TestBadRequestIsRefused(t *testing.T) {
	_, ts, _ := newMock(t, Config{})
	bad := strings.Replace(body, `"10.000"`, `"-1"`, 1)
	if code, _ := post(t, ts.URL+"/reservations", bad); code != http.StatusBadRequest {
		t.Errorf("bad quantity: %d, want 400", code)
	}
}

// validGS1 checks the check digit of an SSCC-18.
func validGS1(s string) bool {
	total := 0
	for j := 0; j < 17; j++ {
		d := int(s[16-j] - '0')
		if j%2 == 0 {
			d *= 3
		}
		total += d
	}
	return int(s[17]-'0') == (10-total%10)%10
}
