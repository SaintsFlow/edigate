// Package contracts holds the JSON Schemas (draft 2020-12) of tool inputs and
// outputs, and the closed schemas of the quarantined reader's answers
// (notes/). The schema files are embedded; Validate checks raw tool arguments
// against the input schema before any service is called.
package contracts

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

//go:embed schemas/*.json notes/*.json
var files embed.FS

// ErrInvalid marks arguments or results that do not match their schema.
var ErrInvalid = errors.New("schema validation failed")

var (
	mu       sync.Mutex
	resolved = map[string]*jsonschema.Resolved{}
)

// Input returns the raw input schema of a tool, for the tool listing.
func Input(tool string) (json.RawMessage, error) {
	return files.ReadFile("schemas/" + tool + ".in.json")
}

// Output returns the raw output schema of a tool, for the tool listing.
func Output(tool string) (json.RawMessage, error) {
	return files.ReadFile("schemas/" + tool + ".out.json")
}

// ToolNames lists the tools that have a schema: the tools of the gateway. The linter of
// upstream descriptions uses it to find references to them.
func ToolNames() []string {
	entries, err := files.ReadDir("schemas")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".in.json"); ok {
			names = append(names, name)
		}
	}
	return names
}

// Validate checks raw tool arguments against the input schema of the tool.
// Empty arguments count as an empty object. The error wraps ErrInvalid.
func Validate(tool string, raw []byte) error { return check("schemas/"+tool+".in.json", raw, true) }

// ValidateOutput checks a tool result against the output schema.
func ValidateOutput(tool string, raw []byte) error {
	return check("schemas/"+tool+".out.json", raw, false)
}

// NoteSchema returns the closed schema that edi.extract_note answers with for
// schemaID (notes/<schemaID>.json), as the model's response format.
func NoteSchema(schemaID string) (json.RawMessage, error) {
	if !validID.MatchString(schemaID) {
		return nil, fmt.Errorf("unknown note schema %q", schemaID)
	}
	return files.ReadFile("notes/" + schemaID + ".json")
}

// NoteDescription returns the top-level "description" of a note schema: one
// line per value, for the system prompt of the quarantined reader.
func NoteDescription(schemaID string) (string, error) {
	raw, err := NoteSchema(schemaID)
	if err != nil {
		return "", err
	}
	var s struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s.Description, nil
}

// ValidateNote checks the model's answer against the note schema schemaID.
func ValidateNote(schemaID string, raw []byte) error {
	if !validID.MatchString(schemaID) {
		return fmt.Errorf("%w: unknown note schema", ErrInvalid)
	}
	return check("notes/"+schemaID+".json", raw, false)
}

// validID keeps a schema id from the caller out of file paths.
var validID = regexp.MustCompile(`^[a-z_]{1,40}$`)

func check(path string, raw []byte, emptyIsObject bool) error {
	rs, err := load(path)
	if err != nil {
		return err
	}
	if emptyIsObject && len(raw) == 0 {
		raw = []byte("{}")
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return fmt.Errorf("%w: not JSON", ErrInvalid)
	}
	if err := rs.Validate(instance); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

func load(path string) (*jsonschema.Resolved, error) {
	mu.Lock()
	defer mu.Unlock()
	if rs, ok := resolved[path]; ok {
		return rs, nil
	}
	data, err := files.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("unknown schema %q: %w", path, err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("schema %q: %w", path, err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("schema %q: %w", path, err)
	}
	resolved[path] = rs
	return rs, nil
}
