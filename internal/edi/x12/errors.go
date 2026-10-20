package x12

import "fmt"

// Reasons of an EnvelopeError.
const (
	ReasonLimit     = "limit"     // body or segment count over the limit
	ReasonISA       = "isa"       // ISA is malformed
	ReasonCharset   = "charset"   // a byte outside printable ASCII in the envelope
	ReasonStructure = "structure" // GS, GE, IEA, ST or SE are missing or out of place
	ReasonGS        = "gs"        // GS is malformed
	ReasonCounts    = "counts"    // GE or IEA counts or control numbers do not match
	ReasonDuplicate = "duplicate" // a group control number is used twice
)

// EnvelopeError means the interchange or a group is not accepted as a whole.
// Detail is fixed text made by this package; it never copies input bytes.
type EnvelopeError struct {
	Reason string
	Detail string
}

func (e *EnvelopeError) Error() string {
	return fmt.Sprintf("x12 envelope error (%s): %s", e.Reason, e.Detail)
}

func envErr(reason, format string, args ...any) *EnvelopeError {
	return &EnvelopeError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Codes for the 997 (values checked against the Stedi pages of the elements,
// https://www.stedi.com/edi/x12-004010/element/718, /720 and /723).
//
// Element 718, AK502: transaction set syntax error.
const (
	SetNotSupported    = "1"
	SetControlMismatch = "3"
	SetSegmentCount    = "4"
	SetSegmentsInError = "5"
	SetInvalidID       = "6"
	SetControlNotUnq   = "23"
)

// Element 720, AK304: segment syntax error.
const (
	SegUnrecognized   = "1"
	SegMandatoryMiss  = "3"
	SegLoopOverMax    = "4"
	SegOverMax        = "5"
	SegNotInSet       = "6"
	SegNotInSequence  = "7"
	SegElementProblem = "8"
)

// Element 723, AK403: data element syntax error.
const (
	ElMandatoryMissing = "1"
	ElTooMany          = "3"
	ElTooShort         = "4"
	ElTooLong          = "5"
	ElInvalidChar      = "6"
	ElInvalidDate      = "8"
	ElInvalidTime      = "9"
)

// Error is one reason a set is rejected, shaped for a 997: AK502 always, the
// AK3 fields for a segment problem, the AK4 fields for an element problem.
// It is stored as JSON in edi_documents.errors.
type Error struct {
	AK502 string `json:"ak502"`

	SegmentID string `json:"segment_id,omitempty"` // AK301
	Position  int    `json:"position,omitempty"`   // AK302, 1 for ST
	LoopID    string `json:"loop_id,omitempty"`    // AK303
	AK304     string `json:"ak304,omitempty"`

	ElementPos   int    `json:"element_pos,omitempty"`   // AK401-01, counted from 1
	ComponentPos int    `json:"component_pos,omitempty"` // AK401-02, 0 for a simple element
	ElementRef   string `json:"element_ref,omitempty"`   // AK402, number in the data element dictionary
	AK403        string `json:"ak403,omitempty"`
	BadValue     string `json:"bad_value,omitempty"` // AK404, at most 99 printable characters
}

// maxBadValue is the AK404 length limit.
const maxBadValue = 99

// badValue makes the AK404 copy of a value: printable and at most 99 characters.
func badValue(s string) string {
	if len(s) > maxBadValue {
		s = s[:maxBadValue]
	}
	return Printable(s)
}
