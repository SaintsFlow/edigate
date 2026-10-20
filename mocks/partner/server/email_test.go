package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func postEmail(t *testing.T, url string, body map[string]string) (int, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(url+"/email", "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(answer)
}

func letterOf(external string) map[string]string {
	return map[string]string{
		"tenant_id": "ta", "external_id": external, "partner_id": "ZZ:ZZTESTBUYER", "to": "orders@buyer.test",
		"template_id": "tpl_order_received", "business_ref": "PO-7",
	}
}

// The emulator makes the text of a letter from the template and the order number; a repeat of
// the same external id is one letter; an unknown template and a missing member are refused.
func TestEmulatorMakesTheLetterFromATemplate(t *testing.T) {
	_, ts, count := newEmulator(t)

	code, _ := postEmail(t, ts.URL, letterOf("e1"))
	if code != http.StatusCreated {
		t.Fatalf("first letter: HTTP %d", code)
	}
	code, _ = postEmail(t, ts.URL, letterOf("e1"))
	if code != http.StatusOK {
		t.Errorf("repeat: HTTP %d, want 200", code)
	}
	if n := count(`SELECT count(*) FROM mock_partner.received WHERE kind = 'email'`); n != 1 {
		t.Errorf("letters = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM mock_partner.received WHERE kind = 'email' AND recipient = 'orders@buyer.test'
	               AND template_id = 'tpl_order_received' AND business_ref = 'PO-7'
	               AND body = convert_to('We received your order PO-7.', 'UTF8')`); n != 1 {
		t.Error("the letter is not what the template says")
	}

	resp, err := http.Get(ts.URL + "/email?external_id=e1")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("lookup: %v %v", err, resp)
	}
	resp.Body.Close()
	resp, err = http.Get(ts.URL + "/email?external_id=nope")
	if err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("lookup of a letter that is not there: %v %v", err, resp)
	}
	resp.Body.Close()

	bad := letterOf("e2")
	bad["template_id"] = "tpl_free"
	if code, _ := postEmail(t, ts.URL, bad); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown template: HTTP %d", code)
	}
	missing := letterOf("e3")
	delete(missing, "to")
	if code, _ := postEmail(t, ts.URL, missing); code != http.StatusBadRequest {
		t.Errorf("missing address: HTTP %d", code)
	}
}

// The optional members of the benchmark: "body" and "canary" are appended only when they are
// sent. The gateway sends neither, so a letter of the gateway never holds them.
func TestEmulatorBodyAndCanaryOnlyWhenSent(t *testing.T) {
	_, ts, count := newEmulator(t)
	plain := letterOf("p1")
	plain["business_ref"] = "PO-8"
	postEmail(t, ts.URL, plain)
	free := letterOf("p2")
	free["business_ref"] = "PO-9"
	free["body"] = "Hello."
	free["canary"] = "CANARY-1"
	postEmail(t, ts.URL, free)
	if n := count(`SELECT count(*) FROM mock_partner.received WHERE business_ref = 'PO-8'
	               AND body = convert_to('We received your order PO-8.', 'UTF8')`); n != 1 {
		t.Error("a letter without the members has more than the template")
	}
	if n := count(`SELECT count(*) FROM mock_partner.received WHERE business_ref = 'PO-9'
	               AND convert_from(body, 'UTF8') LIKE '%Hello.%' AND convert_from(body, 'UTF8') LIKE '%Price reference: CANARY-1%'`); n != 1 {
		t.Error("the members body and canary are not in the letter")
	}
}
