package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/store/storetest"
)

const fixtures = `{
  "tenant": {"id": "stand", "interchange_id": "ZZ:ZZTESTSUPPL", "currency": "USD"},
  "partners": [
    {"partner_id": "ZZ:ZZTESTBUYER", "role": "customer", "name": "Buyer", "auto_max_amount": "5000.00", "daily_auto_max": "50000.00"},
    {"partner_id": "ZZ:ZZTESTOTHER", "role": "customer", "name": "Other", "auto_max_amount": "5000.00", "daily_auto_max": "50000.00"},
    {"partner_id": "ZZ:ZZTESTVENDOR", "role": "supplier", "name": "Vendor"}
  ],
  "price_list": [{"sku": "SKU-1", "unit": "EA", "unit_price": "1.00"}]
}`

// fixtures load --partner-tokens-out gives a token to each customer partner that has none and keeps
// the entries already in the file when it runs again; the token works as the partner's.
func TestFixturesLoadWritesPartnerTokens(t *testing.T) {
	st, pool := storetest.New(t)
	t.Setenv("EDIGATE_DB_URL", pool.Config().ConnString())
	dir := t.TempDir()
	file, tokens := filepath.Join(dir, "fixtures.json"), filepath.Join(dir, "partner_tokens.json")
	if err := os.WriteFile(file, []byte(fixtures), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var out bytes.Buffer
	for _, args := range [][]string{{file, "--partner-tokens-out", tokens}, {"--partner-tokens-out", tokens, file}} {
		out.Reset()
		if err := fixturesLoad(ctx, args, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if !strings.Contains(out.String(), "partner tokens: 0 issued, 2 in") {
		t.Errorf("the second run must issue nothing: %s", out.String())
	}
	raw, err := os.ReadFile(tokens)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["ZZ:ZZTESTBUYER"] == "" || got["ZZ:ZZTESTOTHER"] == "" || got["ZZ:ZZTESTVENDOR"] != "" {
		t.Fatalf("tokens file: %v", got)
	}
	p, err := st.FindPrincipal(ctx, store.HashToken(got["ZZ:ZZTESTBUYER"]))
	if err != nil || p.Kind != "partner" || p.PartnerID != "ZZ:ZZTESTBUYER" || p.TenantID != "stand" {
		t.Errorf("principal of the token: %+v %v", p, err)
	}

	if err := fixturesLoad(ctx, []string{file, "--bogus"}, &out); err == nil {
		t.Error("an unknown flag must be refused")
	}
	if err := fixturesLoad(ctx, nil, &out); err == nil {
		t.Error("a call without a file must be refused")
	}
}
