package x12

import "fmt"

// Grammar of the supported transaction sets (X12 004010), the subset named in
// the plan. 855 and 856 are the documents the gateway sends (wave 2-7); it never
// accepts them from a partner, but partner-emulator reads them with this parser. Source of the layout: https://www.stedi.com/edi/x12-004010/850,
// /810 and /997 (checked 19.10.2026). The subset is smaller than the standard:
// a segment that is not listed here is rejected as "not in the defined set".
//
// 850: the many segments the standard allows in the heading and the PO1 loop
// (SAC, ITD, TD5, LIN, ...) are not supported. The ADV loop (ADV, DTM, MTX) of
// the heading is supported because MTX02 and MTX03 carry free text; its
// repeat count is ">1" in the standard and is capped at 1000 here.
// 810: DMG, YNQ (it stands before PER in the heading), ITD and others are not
// supported.
//
// Inside the PO1 loop of the 850 the standard puts REF before MSG, and the
// grammar follows the standard (the wave task listed MSG first).

// node is a segment (kids == nil) or a loop. The first segment of a loop is its
// header: it has the ID of the loop and starts every instance.
type node struct {
	id       string
	min, max int
	kids     []node // the segments and loops after the header
	loop     bool
}

func seg(id string, min, max int) node { return node{id: id, min: min, max: max} }

func loop(id string, min, max int, kids ...node) node {
	return node{id: id, min: min, max: max, kids: kids, loop: true}
}

// n1Loop is the N1 loop of the heading (850 and 810 differ only in PER).
func n1Loop() node {
	return loop("N1", 0, 200, seg("N2", 0, 2), seg("N3", 0, 2), seg("N4", 0, 1), seg("REF", 0, 12), seg("PER", 0, 3))
}

var grammars = map[string][]node{
	"850": {
		seg("BEG", 1, 1), seg("CUR", 0, 1), seg("REF", 0, 12), seg("PER", 0, 3), seg("DTM", 0, 10),
		loop("N9", 0, 1000, seg("MSG", 0, 1000)),
		n1Loop(),
		loop("ADV", 0, 1000, seg("DTM", 0, 1000), seg("MTX", 0, 1000)),
		loop("PO1", 1, 100000,
			seg("CUR", 0, 1),
			loop("PID", 0, 1000),
			seg("REF", 0, 12),
			seg("MSG", 0, 1000)),
		seg("CTT", 1, 1),
	},
	"810": {
		seg("BIG", 1, 1), seg("NTE", 0, 100), seg("CUR", 0, 1), seg("REF", 0, 12), seg("PER", 0, 3),
		n1Loop(),
		seg("DTM", 0, 10),
		loop("IT1", 1, 200000,
			seg("CUR", 0, 1),
			loop("PID", 0, 1000),
			seg("REF", 0, 12)),
		seg("TDS", 1, 1), seg("CTT", 0, 1),
	},
	// 855 (Purchase Order Acknowledgment): BAK, the PO1 loop with the ACK of each line, CTT.
	"855": {
		seg("BAK", 1, 1), seg("CUR", 0, 1), seg("REF", 0, 12), seg("PER", 0, 3), seg("DTM", 0, 10),
		n1Loop(),
		loop("PO1", 1, 100000,
			seg("ACK", 0, 104),
			seg("REF", 0, 12)),
		seg("CTT", 1, 1),
	},
	// 856 (Ship Notice). The HL hierarchy (shipment, order, pack, item) is checked flat:
	// every HL starts an instance that may hold the segments below in this order. The
	// standard nests the loops; a flat grammar cannot follow a tree of unlimited depth, and
	// the gateway writes one fixed shape (S, O, then P and I for each line).
	"856": {
		seg("BSN", 1, 1),
		loop("HL", 1, 200000,
			seg("TD1", 0, 20), seg("PRF", 0, 1), seg("REF", 0, 12), seg("DTM", 0, 10),
			seg("MAN", 0, 10), seg("LIN", 0, 1), seg("SN1", 0, 1), n1Loop()),
		seg("CTT", 1, 1),
	},
	"997": {
		seg("AK1", 1, 1),
		loop("AK2", 0, 999999,
			loop("AK3", 0, 999999, seg("AK4", 0, 99)),
			seg("AK5", 1, 1)),
		seg("AK9", 1, 1),
	},
}

// groupCode maps a set to the GS01 of its group.
var groupCode = map[string]string{"850": "PO", "810": "IN", "855": "PR", "856": "SH", "997": "FA"}

// grammarIDs lists every segment ID a grammar mentions.
func grammarIDs(nodes []node, into map[string]bool) map[string]bool {
	if into == nil {
		into = map[string]bool{}
	}
	for _, n := range nodes {
		into[n.id] = true
		grammarIDs(n.kids, into)
	}
	return into
}

// matcher walks the segments of one set along its grammar, greedily. It sets
// the Path of every segment it places and stops at the first structural error.
type matcher struct {
	segs  []Segment // the segments between ST and SE
	known map[string]bool
}

// match returns nil when every segment found its place, else the first error.
func match(grammar []node, segs []Segment) *Error {
	m := &matcher{segs: segs, known: grammarIDs(grammar, nil)}
	pos, err := m.sequence(grammar, "", 0, nil, "")
	if err != nil {
		return err
	}
	if pos < len(segs) {
		return m.unplaced(pos)
	}
	return nil
}

// frame is one level of the walk: the nodes of a sequence, the one being
// matched, and the level around it. It tells a missing segment from a segment
// that stands in the wrong place.
type frame struct {
	nodes  []node
	i      int
	header string // ID of the loop this sequence belongs to; another instance may follow
	parent *frame
}

// follows reports whether a segment with this ID could start something that
// comes after the current node, here or in an enclosing sequence.
func (f *frame) follows(id string) bool {
	for ; f != nil; f = f.parent {
		if f.header == id {
			return true
		}
		for _, n := range f.nodes[f.i+1:] {
			if n.id == id {
				return true
			}
		}
	}
	return false
}

func (m *matcher) sequence(nodes []node, prefix string, pos int, parent *frame, header string) (int, *Error) {
	f := &frame{nodes: nodes, parent: parent, header: header}
	for i, n := range nodes {
		f.i = i
		var err *Error
		if n.loop {
			pos, err = m.loopNode(n, prefix, pos, f)
		} else {
			pos, err = m.segmentNode(n, prefix, pos, f)
		}
		if err != nil {
			return pos, err
		}
	}
	return pos, nil
}

func (m *matcher) segmentNode(n node, prefix string, pos int, f *frame) (int, *Error) {
	count := 0
	for pos < len(m.segs) && m.segs[pos].ID == n.id {
		if count == n.max {
			return pos, m.segmentError(pos, SegOverMax)
		}
		m.segs[pos].Path = prefix + n.id
		pos++
		count++
	}
	if count < n.min {
		return pos, m.missing(n.id, pos, f)
	}
	return pos, nil
}

func (m *matcher) loopNode(n node, prefix string, pos int, f *frame) (int, *Error) {
	count := 0
	for pos < len(m.segs) && m.segs[pos].ID == n.id {
		if count == n.max {
			return pos, m.segmentError(pos, SegLoopOverMax)
		}
		count++
		inner := fmt.Sprintf("%s%s[%d]/", prefix, n.id, count)
		m.segs[pos].Path = inner + n.id
		var err *Error
		if pos, err = m.sequence(n.kids, inner, pos+1, f, n.id); err != nil {
			return pos, err
		}
	}
	if count < n.min {
		return pos, m.missing(n.id, pos, f)
	}
	return pos, nil
}

// segmentError is an AK3 error about the segment at index i.
func (m *matcher) segmentError(i int, code string) *Error {
	s := &m.segs[i]
	return &Error{AK502: SetSegmentsInError, SegmentID: Printable(truncate(s.ID, 10)), Position: s.Position,
		LoopID: loopOf(s.Path), AK304: code}
}

// missing reports a mandatory segment or loop that did not appear before index
// pos. The segment at pos may explain it better: when it is no part of this set,
// or cannot come after the missing one, it is the segment in error.
func (m *matcher) missing(id string, pos int, f *frame) *Error {
	position := len(m.segs) + 2 // where SE stands
	if pos < len(m.segs) {
		cur := m.segs[pos]
		switch {
		case !m.known[cur.ID]:
			return m.unplaced(pos)
		case !f.follows(cur.ID):
			return m.segmentError(pos, SegNotInSequence)
		}
		position = cur.Position
	}
	return &Error{AK502: SetSegmentsInError, SegmentID: id, Position: position, AK304: SegMandatoryMiss}
}

// unplaced reports the segment at index i that no node accepted: unknown to the
// dictionary, known but not part of this set, or known but out of order.
func (m *matcher) unplaced(i int) *Error {
	code := SegNotInSequence
	switch id := m.segs[i].ID; {
	case m.known[id]:
	case dict[id] != nil:
		code = SegNotInSet
	default:
		code = SegUnrecognized
	}
	return m.segmentError(i, code)
}
