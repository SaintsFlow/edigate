package main

// Commands of the stand: "fixtures load" fills a tenant from the file of the
// corpus generator, "txn reopen" is the operator action over /ops/.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/SaintsFlow/edigate/internal/store"
)

// fixturesFile is the part of bench/out/<corpus>/fixtures.json the gateway reads.
// The mail templates in the file are not stored yet.
type fixturesFile struct {
	Tenant struct {
		ID            string `json:"id"`
		InterchangeID string `json:"interchange_id"`
		Currency      string `json:"currency"`
	} `json:"tenant"`
	Partners []struct {
		PartnerID     string   `json:"partner_id"`
		Role          string   `json:"role"`
		Name          string   `json:"name"`
		AutoMaxAmount string   `json:"auto_max_amount"`
		DailyAutoMax  string   `json:"daily_auto_max"`
		Contacts      []string `json:"contacts"`
		ShipToCodes   []string `json:"ship_to_codes"`
	} `json:"partners"`
	PriceList []struct {
		SKU       string `json:"sku"`
		Unit      string `json:"unit"`
		UnitPrice string `json:"unit_price"`
	} `json:"price_list"`
}

var (
	reMoney = regexp.MustCompile(`^[0-9]{1,16}(\.[0-9]{1,2})?$`)
	reUnit  = regexp.MustCompile(`^[A-Z0-9]{2}$`)
	reCur   = regexp.MustCompile(`^[A-Z]{3}$`)
)

// fixturesLoad reads and checks the file, then writes it in one transaction.
func fixturesLoad(ctx context.Context, path string, out io.Writer) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var f fixturesFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("fixtures file: %w", err)
	}
	fx, err := f.toStore()
	if err != nil {
		return fmt.Errorf("fixtures file: %w", err)
	}
	return withStore(ctx, func(st *store.Store) error {
		if err := st.LoadFixtures(ctx, fx); err != nil {
			return fmt.Errorf("load fixtures: %w", err)
		}
		fmt.Fprintf(out, "fixtures loaded: tenant %s, %d partners, %d prices\n", fx.TenantID, len(fx.Partners), len(fx.Prices))
		return nil
	})
}

func (f fixturesFile) toStore() (store.Fixtures, error) {
	t := f.Tenant
	if t.ID == "" || !reCur.MatchString(t.Currency) || (t.InterchangeID != "" && !qualifiedID.MatchString(t.InterchangeID)) {
		return store.Fixtures{}, errors.New("tenant needs an id, a currency and, if given, a valid interchange_id")
	}
	fx := store.Fixtures{TenantID: t.ID, InterchangeID: t.InterchangeID, Currency: t.Currency}
	for _, p := range f.Partners {
		if !qualifiedID.MatchString(p.PartnerID) || (p.Role != "customer" && p.Role != "supplier") || p.Name == "" ||
			!(p.AutoMaxAmount == "" || reMoney.MatchString(p.AutoMaxAmount)) || !(p.DailyAutoMax == "" || reMoney.MatchString(p.DailyAutoMax)) {
			return store.Fixtures{}, fmt.Errorf("bad partner %q", p.PartnerID)
		}
		fx.Partners = append(fx.Partners, store.FixturePartner{
			PartnerID: p.PartnerID, Role: p.Role, Name: p.Name, AutoMaxAmount: p.AutoMaxAmount,
			DailyAutoMax: p.DailyAutoMax, Contacts: p.Contacts, ShipToCodes: p.ShipToCodes,
		})
	}
	for _, p := range f.PriceList {
		if p.SKU == "" || !reUnit.MatchString(p.Unit) || !reMoney.MatchString(p.UnitPrice) {
			return store.Fixtures{}, fmt.Errorf("bad price list row %q", p.SKU)
		}
		fx.Prices = append(fx.Prices, store.FixturePrice{SKU: p.SKU, Unit: p.Unit, UnitPrice: p.UnitPrice})
	}
	return fx, nil
}

// txnReopen asks the gateway to reopen a failed key. Only 200 is success; the
// HTTP status and the answer are printed.
func txnReopen(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("txn reopen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	url := fs.String("url", "", "")
	token := fs.String("token", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *url == "" || *token == "" {
		return usageError{"txn reopen: --url, --token and a key are required"}
	}
	if key, err := hex.DecodeString(fs.Arg(0)); err != nil || len(key) != 32 {
		return usageError{"txn reopen: the key must be 64 hex characters"}
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(*url, "/")+"/ops/txn/"+fs.Arg(0)+"/reopen", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+*token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "HTTP %d\n%s", resp.StatusCode, answer)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the server answered HTTP %d", resp.StatusCode)
	}
	return nil
}
