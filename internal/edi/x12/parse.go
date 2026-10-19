package x12

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

// maxErrors caps the errors kept for one set. A 997 can list far more, but the
// stored document must not grow with the size of a hostile interchange.
const maxErrors = 99

// isaLen is the fixed length of the ISA segment, terminator included.
const isaLen = 106

// Parse reads one interchange. It returns an *EnvelopeError when the
// interchange or a group is not acceptable as a whole, and otherwise the
// structure with Set.Errors filled for every rejected set.
//
// Rules (each has a test): the ISA is exactly 106 bytes and the separators come
// from it; every byte is printable ASCII or a declared separator; counts and
// control numbers of SE, GE and IEA match; each set follows its grammar and the
// element dictionary; limits are 5 MiB and 100 000 segments.
func Parse(raw []byte) (*Interchange, error) {
	if len(raw) > MaxBytes {
		return nil, envErr(ReasonLimit, "interchange is larger than %d bytes", MaxBytes)
	}
	ic, err := parseISA(raw)
	if err != nil {
		return nil, err
	}
	if bytes.Count(raw, []byte{ic.SegmentTerm}) > MaxSegments {
		return nil, envErr(ReasonLimit, "interchange has more than %d segments", MaxSegments)
	}
	segs, err := splitSegments(raw[isaLen:], ic)
	if err != nil {
		return nil, err
	}
	w := &walker{ic: ic, segs: segs, check: elementChecker{compSep: string(ic.ComponentSep)}, gs06: map[string]bool{}}
	if err := w.interchange(); err != nil {
		return nil, err
	}
	return ic, nil
}

// parseISA checks the fixed 106-byte header and takes the three separators from it.
func parseISA(raw []byte) (*Interchange, error) {
	if len(raw) < isaLen || string(raw[:3]) != "ISA" {
		return nil, envErr(ReasonISA, "ISA is missing or shorter than 106 bytes")
	}
	isa := raw[:isaLen]
	es, cs, term := isa[3], isa[104], isa[105]
	for _, c := range []byte{es, cs, term} {
		if c <= ' ' || c > '~' || isAlnum(c) {
			return nil, envErr(ReasonISA, "a separator is a letter, a digit, a space or a control byte")
		}
	}
	if es == cs || es == term || cs == term {
		return nil, envErr(ReasonISA, "the three separators are not distinct")
	}
	if bytes.ContainsFunc(isa, func(r rune) bool { return r < 0x20 || r > 0x7e }) {
		return nil, envErr(ReasonCharset, "a byte outside printable ASCII in the ISA")
	}
	fields := bytes.Split(isa[:104], []byte{es})
	if len(fields) != 17 || len(fields[16]) != 0 {
		return nil, envErr(ReasonISA, "ISA has no 15 elements of fixed width before ISA16")
	}
	vals := make([]string, 15)
	elements := make([]Element, 16)
	for i := 0; i < 15; i++ {
		f := fields[i+1]
		if len(f) != isaWidths[i] {
			return nil, envErr(ReasonISA, "ISA%02d must be %d bytes long", i+1, isaWidths[i])
		}
		if bytes.IndexByte(f, cs) >= 0 || bytes.IndexByte(f, term) >= 0 {
			return nil, envErr(ReasonISA, "a separator inside ISA%02d", i+1)
		}
		vals[i] = string(f)
		elements[i] = Element{Parts: []string{vals[i]}}
	}
	elements[15] = Element{Parts: []string{string(cs)}}

	switch {
	case !validDate(vals[8]):
		return nil, envErr(ReasonISA, "ISA09 is not a date (YYMMDD)")
	case !validTime(vals[9]) || len(vals[9]) != 4:
		return nil, envErr(ReasonISA, "ISA10 is not a time (HHMM)")
	case vals[10] != "U":
		return nil, envErr(ReasonISA, "ISA11 must be U in 004010")
	case vals[11] != "00401":
		return nil, envErr(ReasonISA, "ISA12 must be 00401")
	case strings.Trim(vals[12], "0123456789") != "":
		return nil, envErr(ReasonISA, "ISA13 must be nine digits")
	case vals[13] != "0" && vals[13] != "1":
		return nil, envErr(ReasonISA, "ISA14 must be 0 or 1")
	case vals[14] != "P" && vals[14] != "T" && vals[14] != "I":
		return nil, envErr(ReasonISA, "ISA15 must be P, T or I")
	}
	return &Interchange{
		ElementSep: es, ComponentSep: cs, SegmentTerm: term,
		ISA: Segment{ID: "ISA", Elements: elements},
	}, nil
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// splitSegments cuts everything after the ISA into segments and elements.
func splitSegments(rest []byte, ic *Interchange) ([]Segment, error) {
	if len(rest) == 0 || rest[len(rest)-1] != ic.SegmentTerm {
		return nil, envErr(ReasonStructure, "the data does not end with a segment terminator")
	}
	tokens := bytes.Split(rest[:len(rest)-1], []byte{ic.SegmentTerm})
	segs := make([]Segment, len(tokens))
	for i, t := range tokens {
		fields := bytes.Split(t, []byte{ic.ElementSep})
		s := Segment{ID: string(fields[0])}
		if len(fields) > 1 {
			s.Elements = make([]Element, len(fields)-1)
			for j, f := range fields[1:] {
				parts := bytes.Split(f, []byte{ic.ComponentSep})
				e := Element{Parts: make([]string, len(parts))}
				for k, p := range parts {
					e.Parts[k] = string(p)
				}
				s.Elements[j] = e
			}
		}
		segs[i] = s
	}
	return segs, nil
}

// walker reads the envelope structure: GS groups, ST sets, GE, IEA.
type walker struct {
	ic    *Interchange
	segs  []Segment
	check elementChecker
	gs06  map[string]bool // group control numbers seen in this interchange
}

func (w *walker) interchange() error {
	i := 0
	for i < len(w.segs) && w.segs[i].ID == "GS" {
		g, next, err := w.group(i)
		if err != nil {
			return err
		}
		w.ic.Groups = append(w.ic.Groups, g)
		i = next
	}
	if len(w.ic.Groups) == 0 {
		return envErr(ReasonStructure, "no functional group after the ISA")
	}
	if i != len(w.segs)-1 || w.segs[i].ID != "IEA" {
		return envErr(ReasonStructure, "expected IEA after the last group")
	}
	iea := w.segs[i]
	if err := w.envelopeSegment(&iea); err != nil {
		return err
	}
	if iea.Value(1) != strconv.Itoa(len(w.ic.Groups)) || iea.Value(2) != w.ic.ControlNumber() {
		return envErr(ReasonCounts, "IEA01 or IEA02 does not match the groups or ISA13")
	}
	w.ic.IEA = iea
	return nil
}

// envelopeSegment applies the dictionary to GS, GE or IEA. Any problem rejects
// the whole interchange.
func (w *walker) envelopeSegment(s *Segment) error {
	reason := ReasonStructure
	if s.ID == "GS" {
		reason = ReasonGS
	}
	if errs := w.check.check(s, dict[s.ID]); len(errs) > 0 {
		e := errs[0]
		if e.AK403 == ElInvalidChar {
			reason = ReasonCharset
		}
		return envErr(reason, "%s%02d: %s", s.ID, e.ElementPos, elementProblem(e.AK403))
	}
	return nil
}

func elementProblem(code string) string {
	switch code {
	case ElMandatoryMissing:
		return "mandatory element missing"
	case ElTooMany:
		return "too many elements"
	case ElTooShort:
		return "element too short"
	case ElTooLong:
		return "element too long"
	case ElInvalidChar:
		return "invalid character"
	case ElInvalidDate:
		return "invalid date"
	case ElInvalidTime:
		return "invalid time"
	}
	return "invalid element"
}

func (w *walker) group(i int) (Group, int, error) {
	gs := w.segs[i]
	if err := w.envelopeSegment(&gs); err != nil {
		return Group{}, 0, err
	}
	if !strings.HasPrefix(gs.Value(8), "004010") {
		return Group{}, 0, envErr(ReasonGS, "GS08 must start with 004010")
	}
	if w.gs06[gs.Value(6)] {
		return Group{}, 0, envErr(ReasonDuplicate, "a group control number is used twice")
	}
	w.gs06[gs.Value(6)] = true

	g := Group{GS: gs}
	seenST02 := map[string]bool{}
	i++
	for i < len(w.segs) && w.segs[i].ID == "ST" {
		set, next, err := w.set(i, &g, seenST02)
		if err != nil {
			return Group{}, 0, err
		}
		g.Sets = append(g.Sets, set)
		i = next
	}
	if i >= len(w.segs) || w.segs[i].ID != "GE" {
		return Group{}, 0, envErr(ReasonStructure, "expected GE after the sets of a group")
	}
	ge := w.segs[i]
	if err := w.envelopeSegment(&ge); err != nil {
		return Group{}, 0, err
	}
	if ge.Value(1) != strconv.Itoa(len(g.Sets)) || ge.Value(2) != gs.Value(6) {
		return Group{}, 0, envErr(ReasonCounts, "GE01 or GE02 does not match the sets or GS06")
	}
	g.GE = ge
	return g, i + 1, nil
}

// set reads one set from ST to SE and validates it. A missing SE is an
// envelope error because the end of the set cannot be found.
func (w *walker) set(i int, g *Group, seenST02 map[string]bool) (Set, int, error) {
	j := i + 1
	for j < len(w.segs) && w.segs[j].ID != "SE" {
		switch w.segs[j].ID {
		case "ST", "GS", "GE", "IEA":
			return Set{}, 0, envErr(ReasonStructure, "a transaction set has no SE")
		}
		j++
	}
	if j >= len(w.segs) {
		return Set{}, 0, envErr(ReasonStructure, "a transaction set has no SE")
	}
	s := Set{ST: w.segs[i], Segments: w.segs[i+1 : j : j], SE: w.segs[j]}
	s.ST.Position = 1
	for k := range s.Segments {
		s.Segments[k].Position = k + 2
	}
	s.SE.Position = len(s.Segments) + 2
	s.Errors = w.validateSet(&s, g.GS.Value(1), seenST02)
	return s, j + 1, nil
}

// validateSet returns the errors of one set, ordered by position and capped.
func (w *walker) validateSet(s *Set, gs01 string, seenST02 map[string]bool) []Error {
	var errs []Error
	st01, st02 := s.ST.Value(1), s.ST.Value(2)
	grammar, supported := grammars[st01]

	if seenST02[st02] && st02 != "" {
		errs = append(errs, Error{AK502: SetControlNotUnq})
	}
	seenST02[st02] = true
	switch {
	case !supported:
		errs = append(errs, Error{AK502: SetNotSupported})
	case groupCode[st01] != gs01:
		errs = append(errs, Error{AK502: SetInvalidID})
	}

	if s.SE.Value(2) != st02 {
		errs = append(errs, Error{AK502: SetControlMismatch})
	}
	if s.SE.Value(1) != strconv.Itoa(len(s.Segments)+2) {
		errs = append(errs, Error{AK502: SetSegmentCount})
	}

	if supported {
		if mErr := match(grammar, s.Segments); mErr != nil {
			errs = append(errs, *mErr)
		}
	}
	// The paths are set now, so element errors can name their loop.
	errs = append(errs, w.check.check(&s.ST, dict["ST"])...)
	errs = append(errs, w.check.check(&s.SE, dict["SE"])...)
	if supported {
		for k := range s.Segments {
			if defs, ok := dict[s.Segments[k].ID]; ok {
				errs = append(errs, w.check.check(&s.Segments[k], defs)...)
			}
		}
	}

	sort.SliceStable(errs, func(a, b int) bool { return errs[a].Position < errs[b].Position })
	if len(errs) > maxErrors {
		errs = errs[:maxErrors]
	}
	return errs
}
