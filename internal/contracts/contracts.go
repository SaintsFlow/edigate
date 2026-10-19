// Package contracts holds the JSON Schemas (draft 2020-12) of tool inputs and
// outputs. The schema files are embedded; Validate checks raw tool arguments
// against the input schema before any service is called.
package contracts

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

//go:embed schemas/*.json
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

// Validate checks raw tool arguments against the input schema of the tool.
// Empty arguments count as an empty object. The error wraps ErrInvalid.
func Validate(tool string, raw []byte) error { return check(tool+".in", raw, true) }

// ValidateOutput checks a tool result against the output schema.
func ValidateOutput(tool string, raw []byte) error { return check(tool+".out", raw, false) }

func check(name string, raw []byte, emptyIsObject bool) error {
	rs, err := load(name)
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

func load(name string) (*jsonschema.Resolved, error) {
	mu.Lock()
	defer mu.Unlock()
	if rs, ok := resolved[name]; ok {
		return rs, nil
	}
	data, err := files.ReadFile("schemas/" + name + ".json")
	if err != nil {
		return nil, fmt.Errorf("unknown schema %q: %w", name, err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("schema %q: %w", name, err)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("schema %q: %w", name, err)
	}
	resolved[name] = rs
	return rs, nil
}
