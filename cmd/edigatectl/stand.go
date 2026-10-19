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
	neturl "net/url"
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
		PartnerID      string   `json:"partner_id"`
		Role           string   `json:"role"`
		Name           string   `json:"name"`
		AutoMaxAmount  string   `json:"auto_max_amount"`
		DailyAutoMax   string   `json:"daily_auto_max"`
		ForecastAmount string   `json:"forecast_amount"`
		Contacts       []string `json:"contacts"`
		ShipToCodes    []string `json:"ship_to_codes"`
		SupplierOrders []struct {
			OrderNumber string `json:"order_number"`
			Total       string `json:"total"`
			Lines       []struct {
				SKU       string      `json:"sku"`
				Qty       json.Number `json:"qty"`
				UnitPrice string      `json:"unit_price"`
			} `json:"lines"`
		} `json:"supplier_orders"`
	} `json:"partners"`
	PriceList []struct {
		SKU       string `json:"sku"`
		Unit      string `json:"unit"`
		UnitPrice string `json:"unit_price"`
	} `json:"price_list"`
}

var (
	reMoney = regexp.MustCompile(`^[0-9]{1,16}(\.[0-9]{1,2})?$`)
	reQty   = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,3})?$`)
	reUnit  = regexp.MustCompile(`^[A-Z0-9]{2}$`)
	reCur   = regexp.MustCompile(`^[A-Z]{3}$`)
)

// fixturesLoad is "fixtures load <file> [--partner-tokens-out <file>]". The file may come
// before or after the flag.
func fixturesLoad(ctx context.Context, args []string, out io.Writer) error {
	const form = "fixtures: expected: load <fixtures.json> [--partner-tokens-out <file>]"
	var path string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("fixtures load", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tokensOut := fs.String("partner-tokens-out", "", "")
	if err := fs.Parse(args); err != nil {
		return usageError{form}
	}
	if path == "" && fs.NArg() == 1 {
		path = fs.Arg(0)
	} else if fs.NArg() != 0 || path == "" {
		return usageError{form}
	}
	return loadFixtures(ctx, path, *tokensOut, out)
}

// loadFixtures reads and checks the file, then writes it in one transaction. With
// tokensOut set it then gives a token to every customer partner of the file that has none
// and writes them to that file as JSON {"<partner id>": "<token>"}: partner-emulator reads
// it to post its 997. A token that was issued before cannot be read back (only its hash is
// stored), so the entries already in the file are kept.
func loadFixtures(ctx context.Context, path, tokensOut string, out io.Writer) error {
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
		if tokensOut == "" {
			return nil
		}
		return writePartnerTokens(ctx, st, fx, tokensOut, out)
	})
}

// writePartnerTokens issues a token to each customer partner of the fixtures that has no
// active one and adds it to the JSON file.
func writePartnerTokens(ctx context.Context, st *store.Store, fx store.Fixtures, path string, out io.Writer) error {
	tokens := map[string]string{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &tokens); err != nil {
			return fmt.Errorf("the tokens file %s is not a JSON object: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var ids []string
	for _, p := range fx.Partners {
		if p.Role == "customer" {
			ids = append(ids, p.PartnerID)
		}
	}
	fresh, err := st.IssueMissingPartnerTokens(ctx, fx.TenantID, ids)
	if err != nil {
		return fmt.Errorf("issue partner tokens: %w", err)
	}
	for id, token := range fresh {
		tokens[id] = token
	}
	raw, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil { // the emulator runs as another user in its container
		return err
	}
	fmt.Fprintf(out, "partner tokens: %d issued, %d in %s\n", len(fresh), len(tokens), path)
	return nil
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
		if !(p.ForecastAmount == "" || reMoney.MatchString(p.ForecastAmount)) {
			return store.Fixtures{}, fmt.Errorf("bad forecast_amount of partner %q", p.PartnerID)
		}
		fp := store.FixturePartner{
			PartnerID: p.PartnerID, Role: p.Role, Name: p.Name, AutoMaxAmount: p.AutoMaxAmount,
			DailyAutoMax: p.DailyAutoMax, ForecastAmount: p.ForecastAmount, Contacts: p.Contacts, ShipToCodes: p.ShipToCodes,
		}
		for _, o := range p.SupplierOrders {
			if o.OrderNumber == "" || !reMoney.MatchString(o.Total) || len(o.Lines) == 0 {
				return store.Fixtures{}, fmt.Errorf("bad supplier order %q of partner %q", o.OrderNumber, p.PartnerID)
			}
			so := store.FixtureSupplierOrder{OrderNumber: o.OrderNumber, Total: o.Total}
			for _, l := range o.Lines {
				if l.SKU == "" || !reQty.MatchString(l.Qty.String()) || !reMoney.MatchString(l.UnitPrice) {
					return store.Fixtures{}, fmt.Errorf("bad line in supplier order %q", o.OrderNumber)
				}
				so.Lines = append(so.Lines, store.FixtureSupplierLine{SKU: l.SKU, Qty: l.Qty.String(), UnitPrice: l.UnitPrice})
			}
			fp.SupplierOrders = append(fp.SupplierOrders, so)
		}
		fx.Partners = append(fx.Partners, fp)
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
	return opsCall(ctx, http.MethodPost, *url, "/ops/txn/"+fs.Arg(0)+"/reopen", *token, out)
}

// opsCall sends one request to /ops/ with the token of an operator. Only 200 is
// success; the HTTP status and the answer are printed either way.
func opsCall(ctx context.Context, method, baseURL, path, token string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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

var reUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// approvals is the operator's view of the approval requests over /ops/:
//
//	approvals list    --url URL --token T [--status pending|approved|rejected|expired]
//	approvals approve --url URL --token T <id>
//	approvals reject  --url URL --token T <id>
func approvals(ctx context.Context, args []string, out io.Writer) error {
	const form = "approvals: expected: list|approve|reject --url URL --token OPERATOR_TOKEN [--status S] [id]"
	if len(args) == 0 {
		return usageError{form}
	}
	action := args[0]
	fs := flag.NewFlagSet("approvals "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	url := fs.String("url", "", "")
	token := fs.String("token", "", "")
	status := fs.String("status", "pending", "")
	if err := fs.Parse(args[1:]); err != nil || *url == "" || *token == "" {
		return usageError{form}
	}
	switch action {
	case "list":
		if fs.NArg() != 0 {
			return usageError{form}
		}
		return opsCall(ctx, http.MethodGet, *url, "/ops/approvals?status="+neturl.QueryEscape(*status), *token, out)
	case "approve", "reject":
		if fs.NArg() != 1 || !reUUID.MatchString(fs.Arg(0)) {
			return usageError{"approvals " + action + ": the id of a request (a UUID) is required"}
		}
		return opsCall(ctx, http.MethodPost, *url, "/ops/approvals/"+fs.Arg(0)+"/"+action, *token, out)
	}
	return usageError{form}
}
