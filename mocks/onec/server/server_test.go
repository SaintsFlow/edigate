package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SaintsFlow/edigate/internal/store/storetest"
	"github.com/SaintsFlow/edigate/mocks/onec/server"
)

const order = `{"tenant_id":"ta","external_id":"EXT","partner_id":"ZZ:P","business_ref":"PO-1","ship_to":"DC01",` +
	`"lines":[{"sku":"A","qty":"1.000","unit":"EA","unit_price":"2.00"}],"total":"2.00","currency":"USD"}`

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestMockOnecIsAnIdempotentReceiver(t *testing.T) {
	_, pool := storetest.New(t)
	ctx := context.Background()
	if err := server.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := server.Migrate(ctx, pool); err != nil { // the second start changes nothing
		t.Fatal(err)
	}
	s := server.New(pool, server.Config{})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	count := func() (n int) {
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM mock_onec.sales_orders`).Scan(&n)
		return n
	}

	if code, _ := do(t, "GET", ts.URL+"/hs/orders?external_id=EXT", ""); code != 404 {
		t.Errorf("lookup of nothing: %d", code)
	}
	code, first := do(t, "POST", ts.URL+"/hs/orders", order)
	if code != 201 || !strings.Contains(first, `"external_id":"EXT"`) {
		t.Fatalf("first post: %d %s", code, first)
	}
	code, second := do(t, "POST", ts.URL+"/hs/orders", order)
	var a, b struct{ ID int64 }
	_ = json.Unmarshal([]byte(first), &a)
	_ = json.Unmarshal([]byte(second), &b)
	if code != 200 || a.ID == 0 || a.ID != b.ID || count() != 1 {
		t.Fatalf("second post: %d %s (orders %d)", code, second, count())
	}
	if code, body := do(t, "GET", ts.URL+"/hs/orders?external_id=EXT", ""); code != 200 || !strings.Contains(body, `"total":"2.00"`) {
		t.Errorf("lookup: %d %s", code, body)
	}

	// Without an external id every post is a new order (how a client without keys behaves).
	noKey := strings.Replace(order, `"external_id":"EXT",`, "", 1)
	for range 2 {
		if code, body := do(t, "POST", ts.URL+"/hs/orders", noKey); code != 201 {
			t.Fatalf("post without a key: %d %s", code, body)
		}
	}
	if count() != 3 {
		t.Errorf("orders = %d, want 3", count())
	}

	if code, _ := do(t, "POST", ts.URL+"/hs/orders", `{"tenant_id":"ta"}`); code != 400 {
		t.Errorf("bad order: %d", code)
	}
}

func TestMockOnecFaultKnobs(t *testing.T) {
	_, pool := storetest.New(t)
	ctx := context.Background()
	if err := server.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s := server.New(pool, server.Config{FailAfterCommitRate: 1, RejectRefPrefix: "REJ"})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	var n int
	count := func() int { _ = pool.QueryRow(ctx, `SELECT count(*) FROM mock_onec.sales_orders`).Scan(&n); return n }

	// Committed, answer lost: 500 and one row.
	if code, _ := do(t, "POST", ts.URL+"/hs/orders", order); code != 500 || count() != 1 {
		t.Errorf("lost answer: HTTP %d, orders %d", code, count())
	}
	// A reference with the refused prefix: 422 and no row.
	refused := strings.Replace(strings.Replace(order, `"PO-1"`, `"REJ-1"`, 1), `"EXT"`, `"EXT2"`, 1)
	if code, _ := do(t, "POST", ts.URL+"/hs/orders", refused); code != 422 || count() != 1 {
		t.Errorf("refused prefix: HTTP %d, orders %d", code, count())
	}
}
