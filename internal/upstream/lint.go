// The linter of tool descriptions: a signal for the administrator who pins a tool, not
// a border (control M2). A description that passes can still be hostile, and a
// description that is flagged can be pinned on purpose (imperative only).
//
// Two findings exist. tool_reference: the description names a tool of the gateway or
// of another upstream server, with or without its up.<server>. prefix; this is how a
// poisoned description steers the model to another tool, and it is never accepted.
// imperative: the description addresses the model in the imperative.
package upstream

import (
	"regexp"
	"strings"
)

// Finding kinds.
const (
	ToolReference = "tool_reference"
	Imperative    = "imperative"
)

// Finding is one result of the linter.
type Finding struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"` // the name or the phrase that was found
}

// imperativePhrases are matched as whole words, ignoring case. They are the ways a
// description speaks to the model instead of describing the tool. The list is short
// on purpose: it flags, and a person decides. The tags are matched separately.
var imperativePhrases = []string{
	"ignore", "disregard", "you must", "you should always", "always", "never", "before using",
	"before any", "before calling", "do not tell", "don't tell", "do not mention", "do not inform",
	"must first", "first call", "instead of",
}

var (
	imperativeRes = compilePhrases(imperativePhrases)
	// <IMPORTANT>, <system>, <instructions>, <secret> and similar tags.
	tagRe = regexp.MustCompile(`(?i)<\s*/?\s*(important|system|instructions?|secret|admin|override)\b[^>]*>`)
)

func compilePhrases(phrases []string) map[string]*regexp.Regexp {
	out := make(map[string]*regexp.Regexp, len(phrases))
	for _, p := range phrases {
		out[p] = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(p) + `\b`)
	}
	return out
}

// Lint checks a description. names are the tool names it must not mention: those of the
// gateway and of the other upstream servers (bare, or with the up.<server>. prefix).
// Findings come in a fixed order: tool references by name, then phrases, then tags.
func Lint(description string, names []string) []Finding {
	var out []Finding
	for _, n := range names {
		if mentions(description, n) {
			out = append(out, Finding{Kind: ToolReference, Detail: n})
		}
	}
	for _, p := range imperativePhrases {
		if imperativeRes[p].MatchString(description) {
			out = append(out, Finding{Kind: Imperative, Detail: p})
		}
	}
	if m := tagRe.FindString(description); m != "" {
		out = append(out, Finding{Kind: Imperative, Detail: strings.TrimSpace(m)})
	}
	return out
}

// mentions reports whether name stands in text as a name: not inside a longer word.
// A full stop that ends a sentence after the name does not count as part of it.
func mentions(text, name string) bool {
	if name == "" {
		return false
	}
	re := regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(name) + `($|[^A-Za-z0-9_-]|\.($|[^A-Za-z0-9_-]))`)
	return re.MatchString(text)
}

// HasKind reports whether the findings hold one of the kind.
func HasKind(findings []Finding, kind string) bool {
	for _, f := range findings {
		if f.Kind == kind {
			return true
		}
	}
	return false
}
