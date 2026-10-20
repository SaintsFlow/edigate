package outbound

// The 997 (functional acknowledgment) the gateway sends for every group of an incoming 850
// or 810. Input: an AckSpec the receiver made from the parsed interchange (the errors the
// strict parser found per set are already in it). Output: the bytes of one interchange with
// one group and one 997, parsed back with the strict parser before they are returned.
//
// A 997 reports syntax only. A set the parser accepted is A, a set with errors is R with
// the AK3 and AK4 segments that name the segment and the element. A set the gateway refuses
// for the role of the partner is still syntactically right, so it is A; that refusal is in
// the audit log and in the approval requests, not in the 997.

import (
	"strconv"
	"strings"

	"github.com/SaintsFlow/edigate/internal/edi/x12"
)

// AckSet is the verdict on one set: its ST01 and ST02 as the partner sent them and the
// errors of the strict parser (none means accepted).
type AckSet struct {
	ID      string // ST01
	Control string // ST02
	Errors  []x12.Error
}

// AckSpec describes the 997 for one functional group: GS01 and GS06 of the group and its sets.
type AckSpec struct {
	PartnerID    string // ISA05:ISA06 of the receiver of the 997, the partner that sent the group
	GroupCode    string // GS01 of the group
	GroupControl string // GS06 of the group
	Sets         []AckSet
}

// Build997 writes the 997 for the spec. Values copied from the partner are made to fit the
// field (length, no separators) first, so a strange ST02 or segment id cannot make the
// gateway's own bytes unreadable; if the strict parser still refuses them, Build997 returns
// the error and the caller sends no 997.
func Build997(a AckSpec, env Envelope) ([]byte, error) {
	body := []x12.Segment{trimmed("AK1", clip(a.GroupCode, 2, 2), clip(a.GroupControl, 1, 9))}
	accepted := 0
	for _, set := range a.Sets {
		body = append(body, trimmed("AK2", clip(set.ID, 3, 3), clip(set.Control, 4, 9)))
		body = append(body, ackErrors(set.Errors)...)
		if len(set.Errors) == 0 {
			accepted++
			body = append(body, trimmed("AK5", "A"))
		} else {
			body = append(body, trimmed("AK5", append([]string{"R"}, setCodes(set.Errors)...)...))
		}
	}
	verdict := "P"
	switch n := len(a.Sets); {
	case accepted == n:
		verdict = "A"
	case accepted == 0:
		verdict = "R"
	}
	n := strconv.Itoa(len(a.Sets))
	body = append(body, trimmed("AK9", verdict, n, n, strconv.Itoa(accepted)))
	return wrap("997", "FA", body, env)
}

// ackErrors writes AK3 for every segment that has an error and AK4 for each of its element
// errors. The parser lists the errors of a set in the order of the positions, so the errors of
// one segment follow each other. An error without a segment (the set as a whole) has no AK3.
func ackErrors(errs []x12.Error) []x12.Segment {
	var out []x12.Segment
	lastSeg, lastPos := "", 0
	for _, e := range errs {
		if e.SegmentID == "" {
			continue
		}
		if e.SegmentID != lastSeg || e.Position != lastPos {
			out = append(out, trimmed("AK3", clip(e.SegmentID, 2, 3), strconv.Itoa(max(e.Position, 1)), clip(e.LoopID, 0, 6), clip(e.AK304, 0, 3)))
			lastSeg, lastPos = e.SegmentID, e.Position
		}
		if e.ElementPos > 0 {
			pos := strconv.Itoa(e.ElementPos)
			if e.ComponentPos > 0 {
				pos += ">" + strconv.Itoa(e.ComponentPos)
			}
			out = append(out, x12.Segment{ID: "AK4", Elements: trimElements([]x12.Element{
				{Parts: strings.Split(pos, ">")}, {Parts: []string{clip(e.ElementRef, 0, 4)}},
				{Parts: []string{clip(e.AK403, 1, 3)}}, {Parts: []string{clip(e.BadValue, 0, 99)}},
			})})
		}
	}
	return out
}

// setCodes lists the distinct AK502 codes of a set in order, at most five (AK502 to AK506).
func setCodes(errs []x12.Error) []string {
	var out []string
	for _, e := range errs {
		if e.AK502 == "" || len(out) == 5 {
			continue
		}
		dup := false
		for _, c := range out {
			dup = dup || c == e.AK502
		}
		if !dup {
			out = append(out, clip(e.AK502, 1, 3))
		}
	}
	return out
}

// clip makes a copied value fit an element of lo to hi characters: separators and
// characters outside printable ASCII become "?", a long value is cut, a short one is padded
// with zeros. lo 0 lets an empty value stay empty.
func clip(s string, lo, hi int) string {
	s = x12.Printable(s)
	s = strings.Map(func(r rune) rune {
		if r == '*' || r == '>' || r == '~' {
			return '?'
		}
		return r
	}, s)
	if len(s) > hi {
		s = s[:hi]
	}
	if s != "" || lo > 0 {
		for len(s) < lo {
			s += "0"
		}
	}
	return s
}

// trimmed makes a segment and leaves out the empty elements at its end, which the strict
// parser (as the standard) does not accept.
func trimmed(id string, values ...string) x12.Segment {
	return x12.Segment{ID: id, Elements: trimElements(elements(values...))}
}

func trimElements(els []x12.Element) []x12.Element {
	for len(els) > 0 {
		last := els[len(els)-1]
		if len(last.Parts) == 1 && last.Parts[0] == "" {
			els = els[:len(els)-1]
			continue
		}
		break
	}
	return els
}
