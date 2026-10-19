// Package outbound builds the documents the gateway sends to a partner: the 855
// (order acknowledgment) and the 856 (ship notice).
//
// Input: a Spec, the typed content of the document that the gateway took from its
// own records (the order it created, the facts of the warehouse), and an Envelope with
// the three control numbers and the two interchange ids. Output: the bytes of one
// X12 004010 interchange, or an error. Nothing here comes from the agent.
//
// Build parses its own output with the strict parser and refuses to return bytes the
// parser would reject, so the gateway never sends what it could not read itself.
package outbound

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SaintsFlow/edigate/internal/decimal"
	"github.com/SaintsFlow/edigate/internal/edi/x12"
)

// Line is one line of an outgoing document. Qty is a decimal string with at most
// three places. Status (855 only) is IA (accepted) or IR (rejected). SSCC (856 only)
// is the pallet label of the line.
type Line struct {
	SKU       string `json:"sku"`
	Qty       string `json:"qty"`
	Unit      string `json:"unit"`
	UnitPrice string `json:"unit_price"`
	Status    string `json:"status,omitempty"`
	SSCC      string `json:"sscc,omitempty"`
}

// Spec is the content of an outgoing document. It is stored as JSON in the request
// to the operator and in the outbox, so the field names are fixed.
type Spec struct {
	ST01       string `json:"st01"`                  // 855 or 856
	PartnerID  string `json:"partner_id"`            // ISA05:ISA06 of the receiver
	Ref        string `json:"ref"`                   // the order number of the partner (BEG03)
	OrderDate  string `json:"order_date"`            // CCYYMMDD (BEG05)
	Currency   string `json:"currency"`              // ISO 4217
	AckType    string `json:"ack_type,omitempty"`    // 855: AC, AD or RJ
	ShipTo     string `json:"ship_to,omitempty"`     // code of the ship-to place
	ShipmentID string `json:"shipment_id,omitempty"` // 856: identification of the shipment (BSN02)
	Lines      []Line `json:"lines"`
}

// Envelope holds what the interchange needs besides the content: the ids of the two
// sides ("ZZ:ID"), the control numbers and the moment of writing.
type Envelope struct {
	Sender, Receiver  string
	ISA13, GS06, ST02 string
	Now               time.Time
}

// AmountCents is the sum of the document: quantity times price over the lines that
// are not rejected, in whole cents.
func (s Spec) AmountCents() (int64, error) {
	var items []decimal.Item
	for _, l := range s.Lines {
		if l.Status != "IR" {
			items = append(items, decimal.Item{Qty: l.Qty, Price: l.UnitPrice})
		}
	}
	cents, ok := decimal.SumCents(items)
	if !ok {
		return 0, errors.New("a quantity or a price of the document is not a valid decimal")
	}
	return cents, nil
}

// GroupCode is GS01 of a set: PR for the 855, SH for the 856.
func GroupCode(st01 string) string {
	if st01 == "856" {
		return "SH"
	}
	return "PR"
}

// Build writes the interchange for the spec.
func Build(s Spec, env Envelope) ([]byte, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	var body []x12.Segment
	switch s.ST01 {
	case "855":
		body = s.body855()
	case "856":
		body = s.body856(env.Now)
	default:
		return nil, fmt.Errorf("no builder for transaction set %q", s.ST01)
	}
	sendQual, sendID, _ := strings.Cut(env.Sender, ":")
	recvQual, recvID, _ := strings.Cut(env.Receiver, ":")
	now := env.Now.UTC()
	ic := &x12.Interchange{ElementSep: '*', ComponentSep: '>', SegmentTerm: '~'}
	ic.ISA = x12.Segment{ID: "ISA", Elements: elements("00", "", "00", "", sendQual, sendID, recvQual, recvID,
		now.Format("060102"), now.Format("1504"), "U", "00401", env.ISA13, "0", "T")}
	group := x12.Group{
		GS: segment("GS", GroupCode(s.ST01), sendID, recvID, now.Format("20060102"), now.Format("1504"), env.GS06, "X", "004010"),
		Sets: []x12.Set{{
			ST:       segment("ST", s.ST01, env.ST02),
			Segments: body,
			SE:       segment("SE", strconv.Itoa(len(body)+2), env.ST02),
		}},
		GE: segment("GE", "1", env.GS06),
	}
	ic.Groups = []x12.Group{group}
	ic.IEA = segment("IEA", "1", env.ISA13)

	raw := x12.Write(ic)
	parsed, err := x12.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("the gateway's own %s does not parse: %w", s.ST01, err)
	}
	for _, g := range parsed.Groups {
		for _, set := range g.Sets {
			if len(set.Errors) > 0 {
				return nil, fmt.Errorf("the gateway's own %s has a set error (AK502 %s)", s.ST01, set.Errors[0].AK502)
			}
		}
	}
	return raw, nil
}

// check refuses a spec whose values cannot be written: no lines, or a value with a
// separator or a byte outside printable ASCII.
func (s Spec) check() error {
	if len(s.Lines) == 0 {
		return errors.New("a document needs at least one line")
	}
	values := []string{s.PartnerID, s.Ref, s.OrderDate, s.Currency, s.AckType, s.ShipTo, s.ShipmentID}
	for _, l := range s.Lines {
		values = append(values, l.SKU, l.Qty, l.Unit, l.UnitPrice, l.Status, l.SSCC)
	}
	for _, v := range values {
		if x12.Printable(v) != v || strings.ContainsAny(v, "*>~") {
			return errors.New("a value of the document holds a separator or a character outside printable ASCII")
		}
	}
	return nil
}

func (s Spec) body855() []x12.Segment {
	segs := []x12.Segment{
		segment("BAK", "00", s.AckType, s.Ref, s.OrderDate),
		segment("CUR", "SE", s.Currency),
	}
	if s.ShipTo != "" {
		segs = append(segs, segment("N1", "ST", "", "92", s.ShipTo))
	}
	for i, l := range s.Lines {
		segs = append(segs,
			segment("PO1", strconv.Itoa(i+1), qty(l.Qty), l.Unit, l.UnitPrice, "", "VN", l.SKU),
			segment("ACK", l.Status, qty(l.Qty), l.Unit))
	}
	return append(segs, segment("CTT", strconv.Itoa(len(s.Lines))))
}

// body856 writes the hierarchy: shipment (S), order (O), then a pack (P) and an item (I)
// for every line.
func (s Spec) body856(now time.Time) []x12.Segment {
	now = now.UTC()
	segs := []x12.Segment{
		segment("BSN", "00", s.ShipmentID, now.Format("20060102"), now.Format("1504")),
		segment("HL", "1", "", "S"),
		segment("DTM", "011", now.Format("20060102")),
	}
	if s.ShipTo != "" {
		segs = append(segs, segment("N1", "ST", "", "92", s.ShipTo))
	}
	segs = append(segs, segment("HL", "2", "1", "O"), segment("PRF", s.Ref))
	hl := 2
	for _, l := range s.Lines {
		pack := hl + 1
		segs = append(segs,
			segment("HL", strconv.Itoa(pack), "2", "P"), segment("MAN", "GM", l.SSCC),
			segment("HL", strconv.Itoa(pack+1), strconv.Itoa(pack), "I"),
			segment("LIN", "", "VN", l.SKU), segment("SN1", "", qty(l.Qty), l.Unit))
		hl += 2
	}
	return append(segs, segment("CTT", strconv.Itoa(hl)))
}

// qty writes a quantity as the partner reads it: "10.000" becomes "10".
func qty(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "" {
		return "0"
	}
	return s
}

func segment(id string, values ...string) x12.Segment {
	return x12.Segment{ID: id, Elements: elements(values...)}
}

func elements(values ...string) []x12.Element {
	out := make([]x12.Element, len(values))
	for i, v := range values {
		out[i] = x12.Element{Parts: []string{v}}
	}
	return out
}
