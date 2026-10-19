package txn

import (
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"

	"github.com/SaintsFlow/edigate/internal/decimal"
	"github.com/SaintsFlow/edigate/internal/edi/document"
	"github.com/SaintsFlow/edigate/internal/store"
)

// OrderLine is one line of the order as the effect carries it. Quantity has three
// decimal places, prices two; both are strings, never floating point numbers.
type OrderLine struct {
	SKU       string `json:"sku"`
	Qty       string `json:"qty"`
	Unit      string `json:"unit"`
	UnitPrice string `json:"unit_price"`
}

// OrderPayload is the payload of the effect "onec.create_order" and the thing
// the request hash is taken over. Prices, unit and currency come from the price
// list, never from the 850 (flow P4).
type OrderPayload struct {
	PartnerID           string      `json:"partner_id"`
	BusinessRef         string      `json:"business_ref"`
	ShipTo              string      `json:"ship_to"`
	Lines               []OrderLine `json:"lines"`
	Total               string      `json:"total"`
	Currency            string      `json:"currency"`
	DeliveryInstruction string      `json:"delivery_instruction"`
}

// DenyError means the document cannot become an order. Explain holds the names of
// the broken checks; the tool answers policy_denied with them.
type DenyError struct{ Explain []string }

func (e *DenyError) Error() string { return "order denied: " + strings.Join(e.Explain, ",") }

// SKUs lists the product ids of the lines that can be looked up in the price
// list: a line without a SKU and a SKU that is a note reference are left out
// (BuildOrder refuses them as unknown_sku).
func SKUs(lines []document.Line) []string {
	var out []string
	for _, l := range lines {
		if l.SKU != nil && !isNoteRef(*l.SKU) {
			out = append(out, *l.SKU)
		}
	}
	return out
}

// BuildOrder makes the payload of the effect from the typed document: prices of
// the price list for every line, the total and the currency. partnerID and ref are
// the business key (ref is the raw BEG03). Anything that does not fit gives a
// *DenyError: a SKU that is not in the price list or is a note reference
// (unknown_sku), a missing or non-positive quantity (invalid_quantity), no lines
// (no_lines), lines in more than one currency (mixed_currency).
func BuildOrder(h document.Header850, lines []document.Line, partnerID, ref, delivery string, prices map[string]store.Price) (OrderPayload, error) {
	explain := map[string]bool{}
	if len(lines) == 0 {
		explain["no_lines"] = true
	}
	out := OrderPayload{PartnerID: partnerID, BusinessRef: ref, DeliveryInstruction: delivery, Lines: []OrderLine{}}
	if h.ShipTo != nil && !isNoteRef(*h.ShipTo) {
		out.ShipTo = *h.ShipTo
	}
	sum := new(big.Int) // in 1e-5 of the currency: quantity in 1e-3 times price in 1e-2
	for _, l := range lines {
		if l.SKU == nil || isNoteRef(*l.SKU) {
			explain["unknown_sku"] = true
			continue
		}
		price, ok := prices[*l.SKU]
		if !ok {
			explain["unknown_sku"] = true
			continue
		}
		qty, ok := new(big.Int), l.Qty != nil
		if ok {
			qty, ok = parseScaled(l.Qty.String(), 3)
		}
		if !ok || qty.Sign() <= 0 {
			explain["invalid_quantity"] = true
			continue
		}
		cents, ok := parseScaled(price.UnitPrice, 2)
		if !ok {
			return OrderPayload{}, fmt.Errorf("price list holds a bad price for %q", *l.SKU)
		}
		if out.Currency == "" {
			out.Currency = price.Currency
		} else if out.Currency != price.Currency {
			explain["mixed_currency"] = true
		}
		sum.Add(sum, new(big.Int).Mul(qty, cents))
		out.Lines = append(out.Lines, OrderLine{SKU: *l.SKU, Qty: formatScaled(qty, 3), Unit: price.Unit, UnitPrice: formatScaled(cents, 2)})
	}
	if len(explain) > 0 {
		return OrderPayload{}, &DenyError{Explain: slices.Sorted(maps.Keys(explain))}
	}
	// Round half up from 1e-5 to cents.
	total := new(big.Int).Div(new(big.Int).Add(sum, big.NewInt(500)), big.NewInt(1000))
	out.Total = formatScaled(total, 2)
	return out, nil
}

func isNoteRef(s string) bool { return strings.HasPrefix(s, "$note_") }

// The decimal helpers live in internal/decimal; the outgoing documents use them too.
var parseScaled, formatScaled = decimal.ParseScaled, decimal.FormatScaled
