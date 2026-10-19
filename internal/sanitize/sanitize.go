// Package sanitize turns an accepted X12 set into what the agent may see: the
// typed body and the notes (control M1, the white list).
//
// Input: an x12.Set that Parse accepted and the partner id from the envelope.
// Output: a Result with the JSON body, the business reference and the notes.
//
// One rule: an element reaches the body only if it is on the list below and
// passes the format of its place. A listed element that fails its format is
// replaced by its note reference ("$note_N", reason "format"). The free text
// elements (PID05, MSG01, REF03, N1-02, N2, PER, NTE02, MTX) never enter the
// body: each one becomes a note (reason "free_text"). Elements that are on
// neither list (N3, N4, DTM, PER01, PO105, ...) are dropped without a note.
//
// Notes are numbered in document order. Each note carries the taint label of
// the partner and a random canary (16 hex characters) that the quarantined
// reader uses to notice a model that repeats its input.
package sanitize

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/SaintsFlow/edigate/internal/edi/document"
	"github.com/SaintsFlow/edigate/internal/edi/x12"
)

// Reasons a piece of text is a note.
const (
	ReasonFreeText = "free_text"
	ReasonFormat   = "format"
)

// Note is a piece of partner text kept out of the body.
type Note struct {
	Ref         string // $note_1, $note_2, ... in document order
	SegmentPath string // for example PO1[2]/PID[1]/PID05
	Reason      string // free_text or format
	Text        string
	TaintLabel  string // partner:<partner_id>
	Canary      string
}

// Result is what Build returns for one set. BusinessRef is the value the agent
// sees as the business reference (a note reference if BEG03 or BIG02 failed
// its format); Notes is empty for a 997.
type Result struct {
	BusinessRef string
	Body        []byte
	Notes       []Note
}

// The formats of the listed elements.
var (
	reBusinessRef = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,22}$`)
	reCode2       = regexp.MustCompile(`^[A-Z0-9]{2}$`)
	reCurrency    = regexp.MustCompile(`^[A-Z]{3}$`)
	reParty       = regexp.MustCompile(`^[A-Za-z0-9._/-]{2,35}$`)
	reRefQual     = regexp.MustCompile(`^[A-Z0-9]{2,3}$`)
	reRefID       = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,30}$`)
	reLineNo      = regexp.MustCompile(`^[A-Za-z0-9]{1,20}$`)
	reSKU         = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,48}$`)
)

// Build makes the body and the notes of an accepted set. partnerID is the
// sender (ISA05:ISA06). It fails for a set type it does not know.
func Build(set *x12.Set, partnerID string) (Result, error) {
	b := &builder{label: "partner:" + partnerID}
	var body any
	var ref string
	switch st01 := set.ST.Value(1); st01 {
	case "850":
		v := b.build850(set, partnerID)
		body, ref = v, v.Header.BusinessRef
	case "810":
		v := b.build810(set, partnerID)
		body, ref = v, v.Header.BusinessRef
	case "997":
		raw, err := document.BuildAck(set)
		return Result{Body: raw}, err
	default:
		return Result{}, fmt.Errorf("no body for transaction set %q", x12.Printable(st01))
	}
	if b.err != nil {
		return Result{}, b.err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Result{}, err
	}
	return Result{BusinessRef: ref, Body: raw, Notes: b.notes}, nil
}

// builder walks the segments of one set in order and collects the notes.
type builder struct {
	label string
	notes []Note
	err   error // first failure to draw a canary
}

func (b *builder) build850(set *x12.Set, partnerID string) document.Body850 {
	h := document.Header850{PartnerID: partnerID, References: []document.Reference{}}
	lines := []document.Line{}
	for i := range set.Segments {
		s := &set.Segments[i]
		switch s.ID {
		case "BEG":
			h.Purpose = str(b.value(s, 1, reCode2))
			h.OrderType = str(b.value(s, 2, reCode2))
			h.BusinessRef = str(b.value(s, 3, reBusinessRef))
			h.OrderDate = document.ISODate(s.Value(5))
		case "CUR":
			if s.Path == "CUR" {
				h.Currency = b.value(s, 2, reCurrency)
			}
		case "N1":
			b.party(s, &h.ShipTo, &h.Buyer)
		case "PO1":
			lines = append(lines, b.line(s))
		default:
			b.common(s, &h.References)
		}
	}
	return document.Body850{Header: h, Lines: lines}
}

func (b *builder) build810(set *x12.Set, partnerID string) document.Body810 {
	h := document.Header810{PartnerID: partnerID, TotalAmount: "0.00", References: []document.Reference{}}
	lines := []document.Line{}
	for i := range set.Segments {
		s := &set.Segments[i]
		switch s.ID {
		case "BIG":
			h.InvoiceDate = document.ISODate(s.Value(1))
			h.BusinessRef = str(b.value(s, 2, reBusinessRef))
			h.OrderRef = b.value(s, 4, reBusinessRef)
		case "CUR":
			if s.Path == "CUR" {
				h.Currency = b.value(s, 2, reCurrency)
			}
		case "TDS":
			h.TotalAmount = json.Number(document.ImpliedDecimal(s.Value(1), 2))
		case "N1":
			b.party(s, &h.ShipTo, &h.Buyer)
		case "IT1":
			lines = append(lines, b.line(s))
		default:
			b.common(s, &h.References)
		}
	}
	return document.Body810{Header: h, Lines: lines}
}

// common handles the segments that mean the same in both sets: the heading
// REF (a reference for the agent), REF03 anywhere, and the free text segments.
func (b *builder) common(s *x12.Segment, refs *[]document.Reference) {
	switch s.ID {
	case "REF":
		if s.Path == "REF" {
			qualifier := str(b.value(s, 1, reRefQual))
			*refs = append(*refs, document.Reference{Qualifier: qualifier, ID: b.value(s, 2, reRefID)})
		}
		b.text(s, 3)
	case "PER":
		b.text(s, 2, 4, 6, 8)
	case "MSG":
		b.text(s, 1)
	case "PID":
		b.text(s, 5)
	case "N2":
		b.text(s, 1, 2)
	case "NTE":
		b.text(s, 2)
	case "MTX":
		b.text(s, 2, 3)
	}
}

// party handles an N1: the name is free text; the code of the first ST and the
// first BY with a code becomes ship_to and buyer.
func (b *builder) party(s *x12.Segment, shipTo, buyer **string) {
	b.text(s, 2)
	target := map[string]**string{"ST": shipTo, "BY": buyer}[s.Value(1)]
	if target != nil && *target == nil {
		*target = b.value(s, 4, reParty)
	}
}

// line reads one PO1 or IT1 segment, which share their layout: 1 line number,
// 2 quantity, 3 unit, 4 unit price, 6 and 7 product ID qualifier and product ID.
func (b *builder) line(s *x12.Segment) document.Line {
	l := document.Line{LineNo: b.value(s, 1, reLineNo)}
	if v := s.Value(2); v != "" {
		n := json.Number(document.Decimal(v))
		l.Qty = &n
	}
	l.Unit = b.value(s, 3, reCode2)
	if v := s.Value(4); v != "" {
		n := json.Number(document.Decimal(v))
		l.UnitPrice = &n
	}
	if s.Value(6) != "" && s.Value(7) != "" {
		l.SKUQualifier = b.value(s, 6, reCode2)
		l.SKU = b.value(s, 7, reSKU)
	}
	return l
}

// value returns element n of s for the agent: nil when absent, the text when it
// matches re, else the reference of a new "format" note holding the text.
func (b *builder) value(s *x12.Segment, n int, re *regexp.Regexp) *string {
	v := s.Value(n)
	if v == "" {
		return nil
	}
	if re.MatchString(v) {
		return &v
	}
	ref := b.note(s, n, ReasonFormat)
	return &ref
}

// text makes a "free_text" note of every non-empty element in ns.
func (b *builder) text(s *x12.Segment, ns ...int) {
	for _, n := range ns {
		if s.Value(n) != "" {
			b.note(s, n, ReasonFreeText)
		}
	}
}

// note stores element n of s as the next note and returns its reference.
func (b *builder) note(s *x12.Segment, n int, reason string) string {
	canary, err := newCanary()
	if err != nil && b.err == nil {
		b.err = err
	}
	ref := fmt.Sprintf("$note_%d", len(b.notes)+1)
	b.notes = append(b.notes, Note{
		Ref: ref, SegmentPath: fmt.Sprintf("%s%02d", s.Path, n), Reason: reason,
		Text: s.Value(n), TaintLabel: b.label, Canary: canary,
	})
	return ref
}

// newCanary draws 16 hex characters from the system random source.
func newCanary() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
