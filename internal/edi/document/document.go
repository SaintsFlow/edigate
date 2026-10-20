// Package document holds the types of the stored document body and reads it back
// for the agent.
//
// Input: the stored JSON (Decode) or an x12.Set of a 997 (BuildAck). Output: the
// typed header and lines of an 850 or 810, or the body of a 997. Money and
// quantities are decimal strings written as JSON numbers (json.Number), never
// float64.
//
// The body of an 850 or 810 is not made here: internal/sanitize builds it
// through the white list of elements and their formats, and puts every free
// text into notes. This package only gives that code the types and the number
// helpers (Decimal, ImpliedDecimal, ISODate).
package document

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SaintsFlow/edigate/internal/edi/x12"
)

// Reference is one REF of the heading: qualifier REF01 and identification
// REF02 (null when the partner left it out). A value that failed its format is
// a note reference ("$note_N") in place of the text.
type Reference struct {
	Qualifier string  `json:"qualifier"`
	ID        *string `json:"id"`
}

// Header850 is the header of a purchase order.
type Header850 struct {
	PartnerID   string      `json:"partner_id"`
	BusinessRef string      `json:"business_ref"` // BEG03
	Purpose     string      `json:"purpose"`      // BEG01
	OrderType   string      `json:"order_type"`   // BEG02
	OrderDate   string      `json:"order_date"`   // BEG05, YYYY-MM-DD
	Currency    *string     `json:"currency"`     // CUR02
	ShipTo      *string     `json:"ship_to"`      // N1-04 of the N1 loop with N1-01 = ST
	Buyer       *string     `json:"buyer"`        // N1-04 of the N1 loop with N1-01 = BY
	References  []Reference `json:"references"`   // REF01 and REF02 of the heading
}

// Header810 is the header of an invoice.
type Header810 struct {
	PartnerID   string      `json:"partner_id"`
	BusinessRef string      `json:"business_ref"` // BIG02
	InvoiceDate string      `json:"invoice_date"` // BIG01, YYYY-MM-DD
	OrderRef    *string     `json:"order_ref"`    // BIG04
	Currency    *string     `json:"currency"`     // CUR02
	TotalAmount json.Number `json:"total_amount"` // TDS01 with two implied decimals
	ShipTo      *string     `json:"ship_to"`      // N1-04 of the N1 loop with N1-01 = ST
	Buyer       *string     `json:"buyer"`        // N1-04 of the N1 loop with N1-01 = BY
	References  []Reference `json:"references"`   // REF01 and REF02 of the heading
}

// Line is one PO1 or IT1 loop. Every value is optional in the standard.
type Line struct {
	LineNo       *string      `json:"line_no"`
	Qty          *json.Number `json:"qty"`
	Unit         *string      `json:"unit"`
	UnitPrice    *json.Number `json:"unit_price"`
	SKU          *string      `json:"sku"`
	SKUQualifier *string      `json:"sku_qualifier"`
}

// Body850 is the stored body of an 850.
type Body850 struct {
	Header Header850 `json:"header"`
	Lines  []Line    `json:"lines"`
}

// Body810 is the stored body of an 810.
type Body810 struct {
	Header Header810 `json:"header"`
	Lines  []Line    `json:"lines"`
}

// Ack is the body of a 997. The gateway reads it (wave 2-7); the agent never sees it.
type Ack struct {
	GroupCode    string   `json:"group_code"`    // AK101
	GroupControl string   `json:"group_control"` // AK102
	Sets         []AckSet `json:"sets"`
	GroupAck     string   `json:"group_ack"` // AK901
	Included     string   `json:"included"`  // AK902
	Received     string   `json:"received"`  // AK903
	Accepted     string   `json:"accepted"`  // AK904
}

// AckSet is one AK2..AK5 pair of a 997.
type AckSet struct {
	SetID   string   `json:"set_id"`  // AK201
	Control string   `json:"control"` // AK202
	Ack     string   `json:"ack"`     // AK501
	Errors  []string `json:"errors"`  // AK502..AK506
}

// BuildAck makes the JSON body of an accepted 997. It holds codes and counters
// only.
func BuildAck(set *x12.Set) ([]byte, error) {
	a := Ack{Sets: []AckSet{}}
	for i := range set.Segments {
		s := &set.Segments[i]
		switch s.ID {
		case "AK1":
			a.GroupCode, a.GroupControl = s.Value(1), s.Value(2)
		case "AK2":
			a.Sets = append(a.Sets, AckSet{SetID: s.Value(1), Control: s.Value(2), Errors: []string{}})
		case "AK5":
			if len(a.Sets) == 0 {
				continue
			}
			cur := &a.Sets[len(a.Sets)-1]
			cur.Ack = s.Value(1)
			for n := 2; n <= 6; n++ {
				if v := s.Value(n); v != "" {
					cur.Errors = append(cur.Errors, v)
				}
			}
		case "AK9":
			a.GroupAck, a.Included, a.Received, a.Accepted = s.Value(1), s.Value(2), s.Value(3), s.Value(4)
		}
	}
	return json.Marshal(a)
}

// Decode reads a stored body of an 850 or 810 back into typed values. Fields
// that are not part of the types are dropped, so nothing else reaches the agent.
func Decode(st01 string, raw []byte) (header any, lines []Line, err error) {
	switch st01 {
	case "850":
		var b Body850
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, nil, err
		}
		b.Header.References = noNilRefs(b.Header.References)
		return b.Header, noNil(b.Lines), nil
	case "810":
		var b Body810
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, nil, err
		}
		b.Header.References = noNilRefs(b.Header.References)
		return b.Header, noNil(b.Lines), nil
	}
	return nil, nil, fmt.Errorf("no readable body for transaction set %q", x12.Printable(st01))
}

func noNil(l []Line) []Line {
	if l == nil {
		return []Line{}
	}
	return l
}

func noNilRefs(r []Reference) []Reference {
	if r == nil {
		return []Reference{}
	}
	return r
}

// ISODate turns CCYYMMDD into CCYY-MM-DD. The dictionary already guarantees the
// shape; anything else comes back unchanged.
func ISODate(s string) string {
	if len(s) != 8 {
		return s
	}
	return s[:4] + "-" + s[4:6] + "-" + s[6:]
}

// Decimal turns an X12 R value (digits, one point, optional minus) into a valid
// JSON number: no leading zeros, no bare point, no negative zero.
func Decimal(s string) string {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	return signed(neg, whole, frac)
}

// ImpliedDecimal turns an N2 value ("5250" with two implied decimals) into
// "52.50".
func ImpliedDecimal(s string, places int) string {
	neg := strings.HasPrefix(s, "-")
	digits := strings.TrimPrefix(s, "-")
	if len(digits) <= places {
		digits = strings.Repeat("0", places-len(digits)+1) + digits
	}
	return signed(neg, digits[:len(digits)-places], digits[len(digits)-places:])
}

func signed(neg bool, whole, frac string) string {
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	out := whole
	if frac != "" {
		out += "." + frac
	}
	if neg && strings.Trim(out, "0.") != "" {
		out = "-" + out
	}
	return out
}
