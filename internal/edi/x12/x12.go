// Package x12 is a strict reader and writer of ANSI X12 004010 interchanges.
//
// Input: the raw bytes of one interchange (Parse). Output: an Interchange, or an
// *EnvelopeError when the interchange or one of its groups cannot be accepted as
// a whole. A fault inside one transaction set does not stop the parse: the set
// is kept with its Errors filled and the other sets are accepted.
//
// Write turns an Interchange back into bytes. For an interchange that Parse
// accepted without set errors, Write(Parse(raw)) equals raw byte for byte.
// Waves 2-7 and later write 855, 856 and 997 with the same code.
//
// Supported sets: 850, 810 and 997 (see grammar.go). The text is never repaired
// or escaped: 004010 has no release character, so a value that holds a separator
// or a byte outside printable ASCII rejects the set.
package x12

import (
	"bytes"
	"strings"
)

// Limits on one interchange. The HTTP layer enforces MaxBytes before Parse;
// Parse checks both again.
const (
	MaxBytes    = 5 << 20
	MaxSegments = 100000
)

// Element is one data element. Parts has one entry for a simple element and
// one entry per component for a composite one. An empty element is [""].
type Element struct {
	Parts []string
}

// Segment is one segment. Elements[0] is the first element after the ID.
type Segment struct {
	ID       string
	Elements []Element
	// Position counts segments in the set, 1 for ST. It is 0 outside a set.
	Position int
	// Path is the loop path of a segment the grammar placed, for example
	// "BEG", "N1[2]/N3" or "PO1[1]/PID[2]/PID". Envelope segments and segments
	// the grammar did not place have an empty path.
	Path string
}

// Value returns the first part of element n (counted from 1), or "" when the
// element is absent.
func (s *Segment) Value(n int) string {
	if n < 1 || n > len(s.Elements) || len(s.Elements[n-1].Parts) == 0 {
		return ""
	}
	return s.Elements[n-1].Parts[0]
}

// Set is one transaction set from ST to SE. Segments holds what lies between
// them. A set with a non-empty Errors list is rejected.
type Set struct {
	ST       Segment
	Segments []Segment
	SE       Segment
	Errors   []Error
}

// Group is one functional group from GS to GE.
type Group struct {
	GS   Segment
	Sets []Set
	GE   Segment
}

// Interchange is a whole ISA..IEA envelope with the separators it declares.
type Interchange struct {
	ElementSep   byte
	ComponentSep byte
	SegmentTerm  byte
	ISA          Segment
	Groups       []Group
	IEA          Segment
}

// SenderID returns ISA05:ISA06 with the trailing spaces of ISA06 removed.
func (ic *Interchange) SenderID() string {
	return ic.ISA.Value(5) + ":" + strings.TrimRight(ic.ISA.Value(6), " ")
}

// ReceiverID returns ISA07:ISA08 with the trailing spaces of ISA08 removed.
func (ic *Interchange) ReceiverID() string {
	return ic.ISA.Value(7) + ":" + strings.TrimRight(ic.ISA.Value(8), " ")
}

// ControlNumber returns ISA13.
func (ic *Interchange) ControlNumber() string { return ic.ISA.Value(13) }

// isaWidths are the fixed widths of ISA01..ISA15 in 004010 (ISA16 is one byte).
var isaWidths = [15]int{2, 10, 2, 10, 2, 15, 2, 15, 6, 4, 1, 5, 9, 1, 1}

// Write builds the bytes of an interchange from its structure. Separators and
// terminator come from the Interchange. ISA elements are padded to their fixed
// width (text to the right with spaces, ISA13 to the left with zeros); an
// element that is too long is left as it is, and Parse would reject it.
func Write(ic *Interchange) []byte {
	var b bytes.Buffer
	w := func(s *Segment) {
		b.WriteString(s.ID)
		for _, e := range s.Elements {
			b.WriteByte(ic.ElementSep)
			b.WriteString(strings.Join(e.Parts, string(ic.ComponentSep)))
		}
		b.WriteByte(ic.SegmentTerm)
	}

	b.WriteString("ISA")
	for i := 0; i < 15; i++ {
		v := ""
		if i < len(ic.ISA.Elements) && len(ic.ISA.Elements[i].Parts) > 0 {
			v = ic.ISA.Elements[i].Parts[0]
		}
		if pad := isaWidths[i] - len(v); pad > 0 {
			if i == 12 {
				v = strings.Repeat("0", pad) + v
			} else {
				v += strings.Repeat(" ", pad)
			}
		}
		b.WriteByte(ic.ElementSep)
		b.WriteString(v)
	}
	b.WriteByte(ic.ElementSep)
	b.WriteByte(ic.ComponentSep)
	b.WriteByte(ic.SegmentTerm)

	for gi := range ic.Groups {
		g := &ic.Groups[gi]
		w(&g.GS)
		for si := range g.Sets {
			s := &g.Sets[si]
			w(&s.ST)
			for i := range s.Segments {
				w(&s.Segments[i])
			}
			w(&s.SE)
		}
		w(&g.GE)
	}
	w(&ic.IEA)
	return b.Bytes()
}

// Printable returns s with every byte outside 0x20..0x7E replaced by '?'. It
// makes a piece of untrusted input safe to store, to log and to copy into a 997.
func Printable(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	out := []byte(s)
	for i, c := range out {
		if c < 0x20 || c > 0x7e {
			out[i] = '?'
		}
	}
	return string(out)
}
