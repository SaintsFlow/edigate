package server

// Wave 2-9b: knobs of mock-wms per tenant over POST /control, including ship_qty_factor.

import (
	"net/http"
	"strings"
	"testing"
)

func send(t *testing.T, method, url, payload string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func TestWMSKnobsPerTenant(t *testing.T) {
	_, ts, count := newMock(t, Config{})
	tb := strings.Replace(strings.Replace(body, `"ta"`, `"tb"`, 1), `"ab12"`, `"cd34"`, 1)

	if code, out := send(t, "POST", ts.URL+"/control", `{"tenant_id":"ta","ship_qty_factor":"0.5","fail_after_commit_rate":1}`); code != 200 {
		t.Fatalf("set: %d %s", code, out)
	}
	// ta: the answer is lost after the commit, and the shipped quantity is halved.
	if code, _ := post(t, ts.URL+"/shipments", body); code != http.StatusInternalServerError {
		t.Errorf("ta shipment: HTTP %d, want 500", code)
	}
	// tb: nothing changes.
	code, out := post(t, ts.URL+"/shipments", tb)
	if code != http.StatusCreated || !strings.Contains(out, `"qty":"10.000"`) {
		t.Errorf("tb shipment: HTTP %d %s", code, out)
	}
	if n := count(`SELECT count(*) FROM mock_wms.shipments WHERE tenant_id = 'ta' AND lines->0->>'qty' = '5.000'`); n != 1 {
		t.Errorf("ta shipments with half the quantity: %d, want 1", n)
	}
	if code, _ := send(t, "DELETE", ts.URL+"/control/ta", ""); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	again := strings.Replace(body, `"ab12"`, `"ef56"`, 1)
	if code, out := post(t, ts.URL+"/shipments", again); code != http.StatusCreated || !strings.Contains(out, `"qty":"10.000"`) {
		t.Errorf("ta after the reset: HTTP %d %s", code, out)
	}
	if code, _ := send(t, "POST", ts.URL+"/control", `{"tenant_id":"ta","ship_qty_factor":"-1"}`); code != 400 {
		t.Errorf("a bad factor: HTTP %d, want 400", code)
	}
}
