package server

// Wave 2-9b: knobs of the partner emulator per tenant (the id the gateway sends under, AS2-From).
// A slot of the benchmark sets "no 997" or "drop the MDN" for itself without touching the others.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func control(t *testing.T, method, url, payload string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader([]byte(payload)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestEmulatorKnobsPerTenant(t *testing.T) {
	s, ts, _ := newEmulator(t) // process-wide ack_status is "none"
	if code, out := control(t, "POST", ts.URL+"/control", `{"tenant_id":"ZZ:ZZSLOT1","ack_status":"R","mdn_drop":1}`); code != 200 || out["ack_status"] != "R" {
		t.Fatalf("set: %d %v", code, out)
	}
	if k := s.currentFor("ZZ:ZZSLOT1"); k.AckStatus != "R" || k.MDNDropRate != 1 {
		t.Errorf("slot 1 knobs: %+v", k)
	}
	if k := s.currentFor("ZZ:ZZSLOT2"); k.AckStatus != "none" || k.MDNDropRate != 0 {
		t.Errorf("slot 2 should keep the process-wide knobs: %+v", k)
	}
	if k := s.current(); k.AckStatus != "none" {
		t.Errorf("the process-wide knobs changed: %+v", k)
	}
	// A transmission of slot 1 gets no MDN (mdn_drop 1); one of slot 2 gets it.
	post := func(from string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/as2", bytes.NewReader(asn855()))
		req.Header.Set("AS2-From", from)
		req.Header.Set("AS2-To", "ZZ:ZZTESTBUYER")
		req.Header.Set("Message-Id", "<"+from+"@t>")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("ZZ:ZZSLOT1"); code != http.StatusServiceUnavailable {
		t.Errorf("slot 1 MDN: HTTP %d, want 503 (dropped)", code)
	}
	if code := post("ZZ:ZZSLOT2"); code != http.StatusOK {
		t.Errorf("slot 2 MDN: HTTP %d, want 200", code)
	}
	if code, out := control(t, "DELETE", ts.URL+"/control/ZZ:ZZSLOT1", ""); code != 200 || out["status"] != "reset" {
		t.Fatalf("delete: %d %v", code, out)
	}
	if k := s.currentFor("ZZ:ZZSLOT1"); k.AckStatus != "none" {
		t.Errorf("slot 1 after the reset: %+v", k)
	}
	if code, _ := control(t, "POST", ts.URL+"/control", `{"tenant_id":"ZZ:ZZSLOT1","ack_status":"Z"}`); code != 400 {
		t.Errorf("a bad status: HTTP %d, want 400", code)
	}
	if code, _ := control(t, "POST", ts.URL+"/control", `{"ack_status":"E"}`); code != 200 || s.current().AckStatus != "E" {
		t.Errorf("a patch without a tenant must change the process-wide knobs")
	}
}
