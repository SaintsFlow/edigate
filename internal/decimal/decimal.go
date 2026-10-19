// Package decimal reads and writes non-negative decimal numbers as whole counts of
// a small unit, so that money and quantities never pass through a floating point
// number. Input: a decimal string ("12", "12.5"). Output: a *big.Int count of 10^-scale,
// or a decimal string with a fixed number of places.
package decimal

import (
	"math/big"
	"strings"
)

// ParseScaled reads a non-negative decimal into an integer count of 10^-scale.
// It fails for a sign, an exponent, anything but digits and one point, and for
// more decimal places than scale.
func ParseScaled(s string, scale int) (*big.Int, bool) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(frac) > scale || strings.Trim(whole+frac, "0123456789") != "" {
		return nil, false
	}
	n, ok := new(big.Int).SetString(whole+frac+strings.Repeat("0", scale-len(frac)), 10)
	return n, ok
}

// FormatScaled writes n (a count of 10^-scale) as a decimal string with scale places.
func FormatScaled(n *big.Int, scale int) string {
	digits := n.String()
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	return digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
}

// Item is a quantity (three places) and a unit price (two places).
type Item struct{ Qty, Price string }

// SumCents is the sum of quantity times price over the items, rounded half up to
// whole cents once at the end (the same rule as the total of an order). ok is false
// when a number is not a valid decimal.
func SumCents(items []Item) (int64, bool) {
	sum := new(big.Int) // in 1e-5 of the currency
	for _, it := range items {
		q, qok := ParseScaled(it.Qty, 3)
		p, pok := ParseScaled(it.Price, 2)
		if !qok || !pok {
			return 0, false
		}
		sum.Add(sum, new(big.Int).Mul(q, p))
	}
	sum.Add(sum, big.NewInt(500))
	sum.Div(sum, big.NewInt(1000))
	if !sum.IsInt64() {
		return 0, false
	}
	return sum.Int64(), true
}
