// Package document turns an accepted X12 set into the typed body stored in
// edi_documents.body and reads it back for the agent.
//
// Input: an x12.Set that Parse accepted (Build) or the stored JSON (Decode).
// Output: header and lines made of typed fields only. Free text never enters the
// body: PID05, MSG01, REF03, N1-02, N2, PER and NTE02 are not read here at all
// (wave 2-3 moves them into notes). Money and quantities are decimal strings
// written as JSON numbers (json.Number), never float64.
//
// Strings that a partner wrote and the agent will see (the order number, the
// SKU, the N1 code) are copied as they are for now: their formats and the note
// fallback come with the sanitizer in wave 2-3.
package document

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/SaintsFlow/edigate/internal/edi/x12"
)

// Header850 is the header of a purchase order.
type Header850 struct {
	PartnerID   string  `json:"partner_id"`
	BusinessRef string  `json:"business_ref"` // BEG03
	Purpose     string  `json:"purpose"`      // BEG01
	OrderType   string  `json:"order_type"`   // BEG02
	OrderDate   string  `json:"order_date"`   // BEG05, YYYY-MM-DD
	Currency    *string `json:"currency"`     // CUR02
	ShipTo      *string `json:"ship_to"`      // N1-04 of the N1 loop with N1-01 = ST
	Buyer       *string `json:"buyer"`        // N1-04 of the N1 loop with N1-01 = BY
}

// Header810 is the header of an invoice.
type Header810 struct {
	PartnerID   string      `json:"partner_id"`
	BusinessRef string      `json:"business_ref"` // BIG02
	InvoiceDate string      `json:"invoice_date"` // BIG01, YYYY-MM-DD
	OrderRef    *string     `json:"order_ref"`    // BIG04
	Currency    *string     `json:"currency"`     // CUR02
	TotalAmount json.Number `json:"total_amount"` // TDS01 with two implied decimals
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

type body850 struct {
	Header Header850 `json:"header"`
	Lines  []Line    `json:"lines"`
}

type body810 struct {
	Header Header810 `json:"header"`
	Lines  []Line    `json:"lines"`
}

// Built is what Build returns: the business reference and the JSON body.
type Built struct {
	BusinessRef string // BEG03 for an 850, BIG02 for an 810, "" for a 997
	Body        []byte
}

// Build makes the typed body of an accepted set. partnerID is the sender
// (ISA05:ISA06). It fails for a set type it does not know.
func Build(set *x12.Set, partnerID string) (Built, error) {
	switch st01 := set.ST.Value(1); st01 {
	case "850":
		h, lines := build850(set, partnerID)
		return marshal(h.BusinessRef, body850{Header: h, Lines: lines})
	case "810":
		h, lines := build810(set, partnerID)
		return marshal(h.BusinessRef, body810{Header: h, Lines: lines})
	case "997":
		return marshal("", build997(set))
	default:
		return Built{}, fmt.Errorf("no body for transaction set %q", x12.Printable(st01))
	}
}

func marshal(ref string, v any) (Built, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return Built{}, err
	}
	return Built{BusinessRef: ref, Body: b}, nil
}

func build850(set *x12.Set, partnerID string) (Header850, []Line) {
	h := Header850{PartnerID: partnerID}
	var lines []Line
	for i := range set.Segments {
		s := &set.Segments[i]
		switch {
		case s.Path == "BEG":
			h.Purpose, h.OrderType, h.BusinessRef = s.Value(1), s.Value(2), s.Value(3)
			h.OrderDate = isoDate(s.Value(5))
		case s.Path == "CUR":
			h.Currency = optional(s.Value(2))
		case s.ID == "N1": // only the heading has N1 loops in the supported 850
			switch party := optional(s.Value(4)); s.Value(1) {
			case "ST":
				if h.ShipTo == nil {
					h.ShipTo = party
				}
			case "BY":
				if h.Buyer == nil {
					h.Buyer = party
				}
			}
		case s.ID == "PO1":
			lines = append(lines, line(s))
		}
	}
	return h, noNil(lines)
}

func build810(set *x12.Set, partnerID string) (Header810, []Line) {
	h := Header810{PartnerID: partnerID, TotalAmount: "0.00"}
	var lines []Line
	for i := range set.Segments {
		s := &set.Segments[i]
		switch {
		case s.Path == "BIG":
			h.InvoiceDate, h.BusinessRef, h.OrderRef = isoDate(s.Value(1)), s.Value(2), optional(s.Value(4))
		case s.Path == "CUR":
			h.Currency = optional(s.Value(2))
		case s.Path == "TDS":
			h.TotalAmount = json.Number(impliedDecimal(s.Value(1), 2))
		case s.ID == "IT1":
			lines = append(lines, line(s))
		}
	}
	return h, noNil(lines)
}

// line reads one PO1 or IT1 segment, which share their layout: element 1 line
// number, 2 quantity, 3 unit, 4 unit price, then pairs of product ID qualifier
// and product ID from element 6 on. The first complete pair gives the SKU.
func line(s *x12.Segment) Line {
	l := Line{
		LineNo:    optional(s.Value(1)),
		Qty:       optionalNumber(s.Value(2)),
		Unit:      optional(s.Value(3)),
		UnitPrice: optionalNumber(s.Value(4)),
	}
	for n := 6; n < 25; n += 2 {
		if q, id := s.Value(n), s.Value(n+1); q != "" && id != "" {
			l.SKUQualifier, l.SKU = &q, &id
			break
		}
	}
	return l
}

func build997(set *x12.Set) Ack {
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
	return a
}

// Decode reads a stored body of an 850 or 810 back into typed values. Fields
// that are not part of the types are dropped, so nothing else reaches the agent.
func Decode(st01 string, raw []byte) (header any, lines []Line, err error) {
	switch st01 {
	case "850":
		var b body850
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, nil, err
		}
		return b.Header, noNil(b.Lines), nil
	case "810":
		var b body810
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, nil, err
		}
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

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optionalNumber(s string) *json.Number {
	if s == "" {
		return nil
	}
	n := json.Number(decimal(s))
	return &n
}

// isoDate turns CCYYMMDD into CCYY-MM-DD. The dictionary already guarantees the
// shape; anything else comes back unchanged.
func isoDate(s string) string {
	if len(s) != 8 {
		return s
	}
	return s[:4] + "-" + s[4:6] + "-" + s[6:]
}

// decimal turns an X12 R value (digits, one point, optional minus) into a valid
// JSON number: no leading zeros, no bare point, no negative zero.
func decimal(s string) string {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	return signed(neg, whole, frac)
}

// impliedDecimal turns an N2 value ("5250" with two implied decimals) into
// "52.50".
func impliedDecimal(s string, places int) string {
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
