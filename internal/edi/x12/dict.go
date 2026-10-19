package x12

// The element dictionary of the supported segments (X12 004010).
//
// Every segment below was taken from its Stedi page,
// https://www.stedi.com/edi/x12-004010/segment/<ID>, on 19.10.2026: requirement,
// type and minimum and maximum length of each element. Nothing is from memory.
//
// Conditional elements ("C" on the page) are treated as optional: the relational
// conditions between elements are not checked. The composite elements REF04 and
// N907 are not supported, so a REF or N9 with that many elements is rejected as
// "too many data elements". Code lists of ID elements are not checked.

type dataType int

const (
	tAN dataType = iota // printable text
	tID                 // code; the list of values is not checked
	tN0                 // integer
	tN2                 // integer with two implied decimals
	tR                  // decimal number
	tDT                 // date, CCYYMMDD or YYMMDD
	tTM                 // time, HHMM, HHMMSS or up to HHMMSSDD
)

const (
	mand = 'M'
	opt  = 'O'
	cond = 'C'
)

// elemDef describes one element. A composite element lists its components in
// parts and has no type of its own.
type elemDef struct {
	ref   string // data element number (AK402)
	req   byte
	typ   dataType
	min   int
	max   int
	parts []elemDef
}

func el(ref string, req byte, t dataType, min, max int) elemDef {
	return elemDef{ref: ref, req: req, typ: t, min: min, max: max}
}

func composite(ref string, req byte, parts ...elemDef) elemDef {
	return elemDef{ref: ref, req: req, parts: parts}
}

// repeat appends n copies of the group of elements to base.
func repeat(base []elemDef, n int, group ...elemDef) []elemDef {
	for i := 0; i < n; i++ {
		base = append(base, group...)
	}
	return base
}

// dict maps a segment ID to its elements.
var dict = map[string][]elemDef{
	// https://www.stedi.com/edi/x12-004010/segment/ST
	"ST": {el("143", mand, tID, 3, 3), el("329", mand, tAN, 4, 9)},
	// https://www.stedi.com/edi/x12-004010/segment/SE
	"SE": {el("96", mand, tN0, 1, 10), el("329", mand, tAN, 4, 9)},
	// https://www.stedi.com/edi/x12-004010/segment/GS
	"GS": {
		el("479", mand, tID, 2, 2), el("142", mand, tAN, 2, 15), el("124", mand, tAN, 2, 15),
		el("373", mand, tDT, 8, 8), el("337", mand, tTM, 4, 8), el("28", mand, tN0, 1, 9),
		el("455", mand, tID, 1, 2), el("480", mand, tAN, 1, 12),
	},
	// https://www.stedi.com/edi/x12-004010/segment/GE
	"GE": {el("97", mand, tN0, 1, 6), el("28", mand, tN0, 1, 9)},
	// https://www.stedi.com/edi/x12-004010/segment/IEA
	"IEA": {el("I16", mand, tN0, 1, 5), el("I12", mand, tN0, 9, 9)},

	// https://www.stedi.com/edi/x12-004010/segment/BEG
	"BEG": {
		el("353", mand, tID, 2, 2), el("92", mand, tID, 2, 2), el("324", mand, tAN, 1, 22),
		el("328", opt, tAN, 1, 30), el("373", mand, tDT, 8, 8), el("367", opt, tAN, 1, 30),
		el("587", opt, tID, 2, 2), el("1019", opt, tID, 3, 3), el("1166", opt, tID, 2, 2),
		el("1232", opt, tID, 2, 2), el("786", opt, tID, 2, 2), el("640", opt, tID, 2, 2),
	},
	// https://www.stedi.com/edi/x12-004010/segment/CUR
	"CUR": repeat([]elemDef{
		el("98", mand, tID, 2, 3), el("100", mand, tID, 3, 3), el("280", opt, tR, 4, 10),
		el("98", opt, tID, 2, 3), el("100", opt, tID, 3, 3), el("669", opt, tID, 3, 3),
		el("374", cond, tID, 3, 3), el("373", opt, tDT, 8, 8), el("337", opt, tTM, 4, 8),
	}, 4, el("374", cond, tID, 3, 3), el("373", cond, tDT, 8, 8), el("337", cond, tTM, 4, 8)),
	// https://www.stedi.com/edi/x12-004010/segment/DTM
	"DTM": {
		el("374", mand, tID, 3, 3), el("373", cond, tDT, 8, 8), el("337", cond, tTM, 4, 8),
		el("623", opt, tID, 2, 2), el("1250", cond, tID, 2, 3), el("1251", cond, tAN, 1, 35),
	},
	// https://www.stedi.com/edi/x12-004010/segment/REF (REF04 is not supported)
	"REF": {el("128", mand, tID, 2, 3), el("127", cond, tAN, 1, 30), el("352", cond, tAN, 1, 80)},
	// https://www.stedi.com/edi/x12-004010/segment/PER
	"PER": append(repeat([]elemDef{
		el("366", mand, tID, 2, 2), el("93", opt, tAN, 1, 60), el("365", cond, tID, 2, 2), el("364", cond, tAN, 1, 80),
	}, 2, el("365", cond, tID, 2, 2), el("364", cond, tAN, 1, 80)), el("443", opt, tAN, 1, 20)),
	// https://www.stedi.com/edi/x12-004010/segment/N9 (N907 is not supported)
	"N9": {
		el("128", mand, tID, 2, 3), el("127", cond, tAN, 1, 30), el("369", cond, tAN, 1, 45),
		el("373", opt, tDT, 8, 8), el("337", cond, tTM, 4, 8), el("623", opt, tID, 2, 2),
	},
	// https://www.stedi.com/edi/x12-004010/segment/ADV
	"ADV": {
		el("559", mand, tID, 2, 2), el("1000", mand, tAN, 2, 2), el("740", opt, tR, 1, 20),
		el("741", opt, tR, 1, 20), el("729", opt, tAN, 1, 6), el("1000", cond, tAN, 2, 2),
		el("739", cond, tR, 1, 20),
	},
	// https://www.stedi.com/edi/x12-004010/segment/MTX
	"MTX": {el("363", opt, tID, 3, 3), el("1551", cond, tAN, 1, 4096), el("1551", opt, tAN, 1, 4096), el("934", opt, tID, 2, 2)},
	// https://www.stedi.com/edi/x12-004010/segment/MSG
	"MSG": {el("933", mand, tAN, 1, 264), el("934", cond, tID, 2, 2), el("1470", opt, tN0, 1, 9)},
	// https://www.stedi.com/edi/x12-004010/segment/N1
	"N1": {
		el("98", mand, tID, 2, 3), el("93", cond, tAN, 1, 60), el("66", cond, tID, 1, 2),
		el("67", cond, tAN, 2, 80), el("706", opt, tID, 2, 2), el("98", opt, tID, 2, 3),
	},
	// https://www.stedi.com/edi/x12-004010/segment/N2
	"N2": {el("93", mand, tAN, 1, 60), el("93", opt, tAN, 1, 60)},
	// https://www.stedi.com/edi/x12-004010/segment/N3
	"N3": {el("166", mand, tAN, 1, 55), el("166", opt, tAN, 1, 55)},
	// https://www.stedi.com/edi/x12-004010/segment/N4
	"N4": {
		el("19", opt, tAN, 2, 30), el("156", opt, tID, 2, 2), el("116", opt, tID, 3, 15),
		el("26", opt, tID, 2, 3), el("309", cond, tID, 1, 2), el("310", opt, tAN, 1, 30),
	},
	// https://www.stedi.com/edi/x12-004010/segment/PO1
	"PO1": repeat([]elemDef{
		el("350", opt, tAN, 1, 20), el("330", cond, tR, 1, 15), el("355", opt, tID, 2, 2),
		el("212", cond, tR, 1, 17), el("639", opt, tID, 2, 2),
	}, 10, el("235", cond, tID, 2, 2), el("234", cond, tAN, 1, 48)),
	// https://www.stedi.com/edi/x12-004010/segment/PID
	"PID": {
		el("349", mand, tID, 1, 1), el("750", opt, tID, 2, 3), el("559", cond, tID, 2, 2),
		el("751", cond, tAN, 1, 12), el("352", cond, tAN, 1, 80), el("752", opt, tID, 2, 2),
		el("822", opt, tAN, 1, 15), el("1073", opt, tID, 1, 1), el("819", opt, tID, 2, 3),
	},
	// https://www.stedi.com/edi/x12-004010/segment/CTT
	"CTT": {
		el("354", mand, tN0, 1, 6), el("347", opt, tR, 1, 10), el("81", cond, tR, 1, 10),
		el("355", cond, tID, 2, 2), el("183", cond, tR, 1, 8), el("355", cond, tID, 2, 2),
		el("352", opt, tAN, 1, 80),
	},

	// https://www.stedi.com/edi/x12-004010/segment/BIG
	"BIG": {
		el("373", mand, tDT, 8, 8), el("76", mand, tAN, 1, 22), el("373", opt, tDT, 8, 8),
		el("324", opt, tAN, 1, 22), el("328", opt, tAN, 1, 30), el("327", opt, tAN, 1, 8),
		el("640", opt, tID, 2, 2), el("353", opt, tID, 2, 2), el("306", opt, tID, 1, 2),
		el("76", opt, tAN, 1, 22),
	},
	// https://www.stedi.com/edi/x12-004010/segment/NTE
	"NTE": {el("363", opt, tID, 3, 3), el("352", mand, tAN, 1, 80)},
	// https://www.stedi.com/edi/x12-004010/segment/IT1
	"IT1": repeat([]elemDef{
		el("350", opt, tAN, 1, 20), el("358", cond, tR, 1, 10), el("355", cond, tID, 2, 2),
		el("212", cond, tR, 1, 17), el("639", opt, tID, 2, 2),
	}, 10, el("235", cond, tID, 2, 2), el("234", cond, tAN, 1, 48)),
	// https://www.stedi.com/edi/x12-004010/segment/TDS
	"TDS": {el("610", mand, tN2, 1, 15), el("610", opt, tN2, 1, 15), el("610", opt, tN2, 1, 15), el("610", opt, tN2, 1, 15)},

	// https://www.stedi.com/edi/x12-004010/segment/AK1
	"AK1": {el("479", mand, tID, 2, 2), el("28", mand, tN0, 1, 9)},
	// https://www.stedi.com/edi/x12-004010/segment/AK2
	"AK2": {el("143", mand, tID, 3, 3), el("329", mand, tAN, 4, 9)},
	// https://www.stedi.com/edi/x12-004010/segment/AK3
	"AK3": {el("721", mand, tID, 2, 3), el("719", mand, tN0, 1, 6), el("447", opt, tAN, 1, 6), el("720", opt, tID, 1, 3)},
	// https://www.stedi.com/edi/x12-004010/segment/AK4 (AK401 is the composite C030)
	"AK4": {
		composite("C030", mand, el("722", mand, tN0, 1, 2), el("1528", opt, tN0, 1, 2)),
		el("725", opt, tN0, 1, 4), el("723", mand, tID, 1, 3), el("724", opt, tAN, 1, 99),
	},
	// https://www.stedi.com/edi/x12-004010/segment/AK5
	"AK5": repeat([]elemDef{el("717", mand, tID, 1, 1)}, 5, el("718", opt, tID, 1, 3)),
	// https://www.stedi.com/edi/x12-004010/segment/AK9
	"AK9": repeat([]elemDef{
		el("715", mand, tID, 1, 1), el("97", mand, tN0, 1, 6), el("123", mand, tN0, 1, 6), el("2", mand, tN0, 1, 6),
	}, 5, el("716", opt, tID, 1, 3)),
}
