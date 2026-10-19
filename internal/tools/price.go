package tools

import (
	"context"
	"encoding/json"

	"github.com/SaintsFlow/edigate/internal/store"
)

// PriceListTool is erp.get_price_list (T0, sensitive): the prices of the caller's tenant.
const PriceListTool = "erp.get_price_list"

// PriceListInput is the validated input of erp.get_price_list: 1 to 50 SKUs.
type PriceListInput struct {
	SKU []string `json:"sku"`
}

// PriceRow is one price of the answer (explicit field list).
type PriceRow struct {
	SKU       string `json:"sku"`
	UnitPrice string `json:"unit_price"` // decimal string with two places
	Currency  string `json:"currency"`
	Unit      string `json:"unit"`
}

// PriceListOutput is the answer. A SKU that is not in the price list of the tenant is
// not in it either: another tenant's price cannot be seen (XT-06).
type PriceListOutput struct {
	Prices []PriceRow `json:"prices"`
}

// GetPriceList reads the prices of the caller's tenant for the SKUs asked, in the order
// asked and each once. Every call leaves an audit row price_list_read with the number of
// SKUs asked, because the price list is what a leak would take (XT-06).
func GetPriceList(ctx context.Context, st *store.Store, c Caller, in PriceListInput) (PriceListOutput, error) {
	prices, err := st.Prices(ctx, c.TenantID, in.SKU)
	if err != nil {
		return PriceListOutput{}, err
	}
	out := PriceListOutput{Prices: make([]PriceRow, 0, len(in.SKU))}
	seen := map[string]bool{}
	for _, sku := range in.SKU {
		p, found := prices[sku]
		if !found || seen[sku] {
			continue
		}
		seen[sku] = true
		out.Prices = append(out.Prices, PriceRow{SKU: sku, UnitPrice: p.UnitPrice, Currency: p.Currency, Unit: p.Unit})
	}
	detail, err := json.Marshal(map[string]int{"sku_count": len(in.SKU), "rows": len(out.Prices)})
	if err != nil {
		return PriceListOutput{}, err
	}
	err = st.AppendAudit(ctx, store.AuditEvent{
		TenantID: c.TenantID, EventType: "price_list_read", TraceID: c.TraceID, PrincipalID: c.PrincipalID,
		Tool: PriceListTool, Outcome: "ok", Detail: detail,
	})
	return out, err
}
