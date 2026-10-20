// Package mode names the configurations the gateway can play (flag --mode, variable
// EDIGATE_MODE). One binary plays every configuration of the benchmark with the same tool
// names, so a harness changes only the flag.
//
// Input: the text of the flag. Output: a Mode and the answers to "which protection is on".
// Only the protections that the modes switch are asked here; the code of a protection
// that stays on does not look at the mode.
package mode

import (
	"fmt"
	"strings"
)

// Mode is one configuration of the gateway.
type Mode string

const (
	// Full is C3: every protection on.
	Full Mode = "full"
	// Plain is C0 (and C2 behind an agentgateway): tools with open arguments and synchronous
	// calls of the mocks, no protection of the gateway.
	Plain Mode = "plain"
	// ReqHash is C1: plain, plus a key of the tool and the arguments with a stored result.
	ReqHash Mode = "reqhash"
	// NoSanitize is C3-A1: full, but the partner text is shown and the effects take the raw values.
	NoSanitize Mode = "no-sanitize"
	// NoPolicy is C3-A2: full, but the policy allows every call.
	NoPolicy Mode = "no-policy"
	// NoBizKey is C3-A3: full, but the key is made of the raw arguments of the call.
	NoBizKey Mode = "no-bizkey"
	// NoContracts is C3-A4: full, but the effect tools take their values from the model.
	NoContracts Mode = "no-contracts"
)

// All lists the modes in the order of the protocol.
var All = []Mode{Full, Plain, ReqHash, NoSanitize, NoPolicy, NoBizKey, NoContracts}

// Parse reads the text of the flag. An empty text means Full; an unknown one is an error.
func Parse(s string) (Mode, error) {
	if s == "" {
		return Full, nil
	}
	for _, m := range All {
		if string(m) == s {
			return m, nil
		}
	}
	names := make([]string, len(All))
	for i, m := range All {
		names[i] = string(m)
	}
	return "", fmt.Errorf("unknown mode %q (want one of %s)", s, strings.Join(names, ", "))
}

// Open reports whether the tools are the open tools of C0 and C1: no sanitizer, no taint
// label, no policy, no outbox, no sagas.
func (m Mode) Open() bool { return m == Plain || m == ReqHash }

// RawText reports whether edi.read_document shows the text of the partner as it is.
func (m Mode) RawText() bool { return m.Open() || m == NoSanitize }

// NoteTool reports whether edi.extract_note is registered.
func (m Mode) NoteTool() bool { return !m.RawText() }

// ModelValues reports whether the tools with an effect take their values from the
// arguments the model writes (the schemas of the open tools).
func (m Mode) ModelValues() bool { return m.Open() || m == NoContracts }
