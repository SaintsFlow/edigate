package txn

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/SaintsFlow/edigate/internal/edi/document"
	"github.com/SaintsFlow/edigate/internal/policy"
)

// InvoiceLine is one line of the supplier invoice as the effect carries it.
// Quantity has three decimal places, the price two; both are strings.
type InvoiceLine struct {
	SKU       string `json:"sku"`
	Qty       string `json:"qty"`
	Unit      string `json:"unit"`
	UnitPrice string `json:"unit_price"`
}

// InvoicePayload is the payload of the effect "onec.post_vendor_invoice" and the
// thing the request hash is taken over. Everything comes from the typed 810:
// supplier, number (the raw BIG02), the order number (BIG04), lines, total (TDS01)
// and currency.
type InvoicePayload struct {
	PartnerID     string        `json:"partner_id"`
	InvoiceNumber string        `json:"invoice_number"`
	OrderRef      string        `json:"order_ref"`
	Lines         []InvoiceLine `json:"lines"`
	Total         string        `json:"total"`
	Currency      string        `json:"currency"`
}

// BuildInvoice makes the payload from the typed 810. It also returns the lines in
// the normal form the policy compares with the order to the supplier. Anything that
// does not fit gives a *DenyError: no lines (no_lines), a line without a usable SKU,
// quantity or price (invalid_line), a missing or malformed total (invalid_total), a
// missing currency (missing_currency).
func BuildInvoice(h document.Header810, lines []document.Line, partnerID, ref string) (InvoicePayload, []policy.NormLine, error) {
	explain := map[string]bool{}
	if len(lines) == 0 {
		explain["no_lines"] = true
	}
	out := InvoicePayload{PartnerID: partnerID, InvoiceNumber: ref, Lines: []InvoiceLine{}}
	if h.OrderRef != nil {
		out.OrderRef = *h.OrderRef
	}
	if h.Currency == nil || !reCurrencyCode(*h.Currency) {
		explain["missing_currency"] = true
	} else {
		out.Currency = *h.Currency
	}
	total, ok := parseScaled(h.TotalAmount.String(), 2)
	if !ok {
		explain["invalid_total"] = true
	} else {
		out.Total = formatScaled(total, 2)
	}
	var norm []policy.NormLine
	for _, l := range lines {
		if l.SKU == nil || isNoteRef(*l.SKU) || l.Qty == nil || l.UnitPrice == nil {
			explain["invalid_line"] = true
			continue
		}
		qty, qok := parseScaled(l.Qty.String(), 3)
		price, pok := parseScaled(l.UnitPrice.String(), 2)
		if !qok || !pok || qty.Sign() <= 0 || !qty.IsInt64() || !price.IsInt64() {
			explain["invalid_line"] = true
			continue
		}
		unit := ""
		if l.Unit != nil {
			unit = *l.Unit
		}
		out.Lines = append(out.Lines, InvoiceLine{SKU: *l.SKU, Qty: formatScaled(qty, 3), Unit: unit, UnitPrice: formatScaled(price, 2)})
		norm = append(norm, policy.NormLine{SKU: *l.SKU, QtyMilli: qty.Int64(), PriceCents: price.Int64()})
	}
	if len(explain) > 0 {
		return InvoicePayload{}, nil, &DenyError{Explain: slices.Sorted(maps.Keys(explain))}
	}
	return out, norm, nil
}

func reCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, c := range s {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}

// NormSupplierOrder reads the lines of a stored order to a supplier
// ([{"sku","qty","unit_price"}], decimal strings) into the normal form.
func NormSupplierOrder(raw []byte) ([]policy.NormLine, error) {
	var lines []struct {
		SKU       string `json:"sku"`
		Qty       string `json:"qty"`
		UnitPrice string `json:"unit_price"`
	}
	if err := json.Unmarshal(raw, &lines); err != nil {
		return nil, fmt.Errorf("supplier order lines: %w", err)
	}
	out := make([]policy.NormLine, 0, len(lines))
	for _, l := range lines {
		qty, qok := parseScaled(l.Qty, 3)
		price, pok := parseScaled(l.UnitPrice, 2)
		if !qok || !pok || !qty.IsInt64() || !price.IsInt64() {
			return nil, fmt.Errorf("supplier order holds a bad line %q", l.SKU)
		}
		out = append(out, policy.NormLine{SKU: l.SKU, QtyMilli: qty.Int64(), PriceCents: price.Int64()})
	}
	return out, nil
}
