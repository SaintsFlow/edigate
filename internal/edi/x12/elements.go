package x12

import (
	"strings"
	"time"
)

// elementChecker checks the elements of one segment against the dictionary.
type elementChecker struct {
	compSep string
}

// check returns one Error per problem in the elements of seg. The caller has
// set seg.Position and seg.Path. A segment with no problem gives nil.
func (c elementChecker) check(seg *Segment, defs []elemDef) []Error {
	var errs []Error
	add := func(pos, comp int, ref, code, bad string) {
		errs = append(errs, Error{
			AK502: SetSegmentsInError, SegmentID: Printable(truncate(seg.ID, 10)), Position: seg.Position,
			LoopID: loopOf(seg.Path), AK304: SegElementProblem,
			ElementPos: pos, ComponentPos: comp, ElementRef: ref, AK403: code, BadValue: badValue(bad),
		})
	}

	n := len(seg.Elements)
	for i, e := range seg.Elements {
		switch {
		case i >= len(defs):
			add(i+1, 0, "", ElTooMany, strings.Join(e.Parts, c.compSep))
			return errs
		case i == n-1 && isEmpty(e):
			// X12 requires empty trailing elements to be left out.
			add(i+1, 0, defs[i].ref, ElTooMany, "")
		default:
			c.element(defs[i], i+1, e, add)
		}
	}
	for i := n; i < len(defs); i++ {
		if defs[i].req == mand {
			add(i+1, 0, defs[i].ref, ElMandatoryMissing, "")
		}
	}
	return errs
}

type addFunc func(pos, comp int, ref, code, bad string)

func (c elementChecker) element(def elemDef, pos int, e Element, add addFunc) {
	if def.parts != nil {
		c.composite(def, pos, e, add)
		return
	}
	if isEmpty(e) {
		if def.req == mand {
			add(pos, 0, def.ref, ElMandatoryMissing, "")
		}
		return
	}
	if len(e.Parts) > 1 { // a component separator inside a simple element
		add(pos, 0, def.ref, ElInvalidChar, strings.Join(e.Parts, c.compSep))
		return
	}
	if code := checkValue(def, e.Parts[0]); code != "" {
		add(pos, 0, def.ref, code, e.Parts[0])
	}
}

func (c elementChecker) composite(def elemDef, pos int, e Element, add addFunc) {
	if isEmpty(e) {
		if def.req == mand {
			add(pos, 0, def.ref, ElMandatoryMissing, "")
		}
		return
	}
	if len(e.Parts) > len(def.parts) {
		add(pos, len(def.parts)+1, def.ref, ElTooMany, strings.Join(e.Parts, c.compSep))
		return
	}
	for k, pd := range def.parts {
		part := ""
		if k < len(e.Parts) {
			part = e.Parts[k]
		}
		if part == "" {
			if pd.req == mand {
				add(pos, k+1, pd.ref, ElMandatoryMissing, "")
			}
			continue
		}
		if code := checkValue(pd, part); code != "" {
			add(pos, k+1, pd.ref, code, part)
		}
	}
}

// checkValue checks one non-empty simple value and returns the AK403 code of
// the first problem, or "" when the value is fine.
func checkValue(def elemDef, v string) string {
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return ElInvalidChar
		}
	}
	length := len(v)
	switch def.typ {
	case tN0, tN2:
		digits, ok := numberDigits(v, false)
		if !ok {
			return ElInvalidChar
		}
		length = digits
	case tR:
		digits, ok := numberDigits(v, true)
		if !ok {
			return ElInvalidChar
		}
		length = digits
	case tDT, tTM:
		if strings.Trim(v, "0123456789") != "" {
			return ElInvalidChar
		}
	}
	if length < def.min {
		return ElTooShort
	}
	if length > def.max {
		return ElTooLong
	}
	switch def.typ {
	case tDT:
		if !validDate(v) {
			return ElInvalidDate
		}
	case tTM:
		if !validTime(v) {
			return ElInvalidTime
		}
	}
	return ""
}

// numberDigits checks an optional leading minus, digits and, when decimal is
// set, at most one point. It returns the number of digits: sign and point do
// not count toward the length. That rule for N0, N2 and R is the X12 standard's;
// the Stedi pages state the types and lengths but not this rule, so it is not
// checked against the source.
func numberDigits(v string, decimal bool) (digits int, ok bool) {
	if strings.HasPrefix(v, "-") {
		v = v[1:]
	}
	points := 0
	for i := 0; i < len(v); i++ {
		switch {
		case v[i] >= '0' && v[i] <= '9':
			digits++
		case v[i] == '.' && decimal && points == 0:
			points++
		default:
			return 0, false
		}
	}
	return digits, digits > 0
}

// validDate accepts CCYYMMDD and YYMMDD that name a real calendar day.
func validDate(v string) bool {
	switch len(v) {
	case 8:
		_, err := time.Parse("20060102", v)
		return err == nil
	case 6:
		_, err := time.Parse("060102", v)
		return err == nil
	}
	return false
}

// validTime accepts HHMM, HHMMSS, HHMMSSD and HHMMSSDD. The lengths 7 and 8
// (tenths and hundredths of a second) and the range 00..23 for hours are common
// X12 practice, not checked against the source (the Stedi pages give only
// "TM 4/8").
func validTime(v string) bool {
	switch len(v) {
	case 4, 6, 7, 8:
	default:
		return false
	}
	hh, mm := atoi2(v[0:2]), atoi2(v[2:4])
	if hh > 23 || mm > 59 {
		return false
	}
	return len(v) < 6 || atoi2(v[4:6]) <= 59
}

func atoi2(s string) int { return int(s[0]-'0')*10 + int(s[1]-'0') }

func isEmpty(e Element) bool { return len(e.Parts) == 1 && e.Parts[0] == "" }

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// loopOf returns the ID of the innermost loop in a path such as "N1[2]/N3",
// or "" for a segment outside every loop.
func loopOf(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return ""
	}
	loop := parts[len(parts)-2]
	if i := strings.IndexByte(loop, '['); i >= 0 {
		loop = loop[:i]
	}
	return loop
}
