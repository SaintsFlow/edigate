package server_test

// Wave 2-9b: knobs per tenant over POST /control. A knob for one tenant does not touch another
// tenant, wins over the process-wide value, and DELETE /control/{tenant_id} puts the tenant back.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SaintsFlow/edigate/internal/store/storetest"
	"github.com/SaintsFlow/edigate/mocks/onec/server"
)

func orderOf(tenant, ext, ref string) string {
	return strings.NewReplacer(`"ta"`, `"`+tenant+`"`, `"EXT"`, `"`+ext+`"`, `"PO-1"`, `"`+ref+`"`).Replace(order)
}

func TestMockOnecKnobsPerTenant(t *testing.T) {
	_, pool := storetest.New(t)
	ctx := context.Background()
	if err := server.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(pool, server.Config{}).Handler())
	defer ts.Close()
	count := func(tenant string) (n int) {
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM mock_onec.sales_orders WHERE tenant_id = $1`, tenant).Scan(&n)
		return n
	}

	// Tenant ta loses every answer after the commit; tenant tb is not touched.
	if code, body := do(t, "POST", ts.URL+"/control", `{"tenant_id":"ta","fail_after_commit_rate":1}`); code != 200 || !strings.Contains(body, `"fail_after_commit_rate":1`) {
		t.Fatalf("set: %d %s", code, body)
	}
	if code, _ := do(t, "POST", ts.URL+"/hs/orders", orderOf("ta", "A1", "PO-A")); code != 500 || count("ta") != 1 {
		t.Errorf("tenant ta: HTTP %d, orders %d, want 500 and 1", code, count("ta"))
	}
	if code, _ := do(t, "POST", ts.URL+"/hs/orders", orderOf("tb", "B1", "PO-B")); code != 201 {
		t.Errorf("tenant tb: HTTP %d, want 201", code)
	}

	// A prefix refused for ta only; it is added to the knobs already set.
	do(t, "POST", ts.URL+"/control", `{"tenant_id":"ta","reject_ref_prefix":"REJ"}`)
	if code, _ := do(t, "POST", ts.URL+"/hs/orders", orderOf("ta", "A2", "REJ-1")); code != 422 {
		t.Errorf("ta with the refused prefix: HTTP %d, want 422", code)
	}
	if code, _ := do(t, "POST", ts.URL+"/hs/orders", orderOf("tb", "B2", "REJ-1")); code != 201 {
		t.Errorf("tb with the same prefix: HTTP %d, want 201", code)
	}

	// A delay for ta only.
	do(t, "POST", ts.URL+"/control", `{"tenant_id":"ta","latency_ms":300}`)
	start := time.Now()
	do(t, "POST", ts.URL+"/hs/orders", orderOf("tb", "B3", "PO-B3"))
	if time.Since(start) > 200*time.Millisecond {
		t.Errorf("tb was delayed by the knob of ta: %v", time.Since(start))
	}
	start = time.Now()
	do(t, "POST", ts.URL+"/hs/orders", orderOf("ta", "A3", "PO-A3"))
	if time.Since(start) < 300*time.Millisecond {
		t.Errorf("ta was not delayed: %v", time.Since(start))
	}

	// DELETE puts ta back.
	if code, _ := do(t, "DELETE", ts.URL+"/control/ta", ""); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := do(t, "POST", ts.URL+"/hs/orders", orderOf("ta", "A4", "REJ-2")); code != 201 {
		t.Errorf("ta after the reset: HTTP %d, want 201", code)
	}
	if code, body := do(t, "GET", ts.URL+"/control?tenant_id=ta", ""); code != 200 || strings.Contains(body, "fail_after") {
		t.Errorf("knobs after the reset: %d %s", code, body)
	}
}

func TestMockOnecControlRefusesBadInput(t *testing.T) {
	_, pool := storetest.New(t)
	if err := server.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(pool, server.Config{}).Handler())
	defer ts.Close()
	for _, body := range []string{
		`{"fail_after_commit_rate":1}`,                  // no tenant
		`{"tenant_id":"ta","fail_after_commit_rate":2}`, // out of range
		`{"tenant_id":"ta","latency_ms":-1}`,            // negative
		`{"tenant_id":"ta","ship_qty_factor":"2"}`,      // a knob of mock-wms
		`{"tenant_id":"ta","unknown":1}`,                // unknown member
	} {
		if code, _ := do(t, "POST", ts.URL+"/control", body); code != 400 {
			t.Errorf("%s: HTTP %d, want 400", body, code)
		}
	}
}
