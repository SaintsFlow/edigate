// What passes between the agent and a pinned upstream tool: the check of the arguments
// against the pinned input schema, and the shaping of the result (control M4, TL-04).
//
// Every result reaches the agent with the label {"edigate/taint": "upstream:<server>"}
// in _meta. For a server with quarantine_results the text of the result is kept in the
// table upstream_notes and the agent gets references ($up_N) that no tool opens; a
// structured result passes only if it fits the pinned output schema and holds no free
// string, that is, no string the schema does not fix by enum, const or format.
package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/rpcerr"
)

// TaintKey is the key in _meta of a result that came from an upstream.
const TaintKey = "edigate/taint"

// TaintLabel is the value under TaintKey for a server.
func TaintLabel(server string) string { return "upstream:" + server }

// NoteSaver keeps the text of a quarantined result.
type NoteSaver interface {
	SaveUpstreamNote(ctx context.Context, tenantID, server, tool, traceID, body, label string) (int64, error)
}

// ValidateInput checks the arguments against the input schema that was pinned. Empty
// arguments count as an empty object.
func (t Tool) ValidateInput(args []byte) error {
	rs, err := resolve(t.Live.InputSchema)
	if err != nil {
		return fmt.Errorf("input schema of %s: %w", t.PublicName(), err)
	}
	if len(args) == 0 {
		args = []byte("{}")
	}
	var instance any
	if err := json.Unmarshal(args, &instance); err != nil {
		return rpcerr.New(rpcerr.SchemaInvalid, "arguments do not match the tool schema")
	}
	if err := rs.Validate(instance); err != nil {
		return rpcerr.New(rpcerr.SchemaInvalid, "arguments do not match the tool schema")
	}
	return nil
}

func resolve(schema any) (*jsonschema.Resolved, error) {
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return s.Resolve(nil)
}

// Shape turns the result of the upstream into the result for the agent and returns the
// SHA-256 of the result as the upstream sent it (for the audit row).
func Shape(ctx context.Context, notes NoteSaver, actor Actor, t Tool, raw *mcp.CallToolResult) (*mcp.CallToolResult, [32]byte, error) {
	whole, err := json.Marshal(raw)
	if err != nil {
		return nil, [32]byte{}, err
	}
	sum := sha256.Sum256(whole)
	label := TaintLabel(t.Server)
	out := &mcp.CallToolResult{
		Meta: mcp.Meta{TaintKey: label}, Content: raw.Content, StructuredContent: raw.StructuredContent, IsError: raw.IsError,
	}
	if !t.Quarantine {
		return out, sum, nil
	}
	out.Content = nil
	for _, c := range raw.Content {
		text, err := textOf(c)
		if err != nil {
			return nil, sum, err
		}
		id, err := notes.SaveUpstreamNote(ctx, actor.TenantID, t.Server, t.Live.Name, actor.TraceID, text, label)
		if err != nil {
			return nil, sum, err
		}
		out.Content = append(out.Content, &mcp.TextContent{Text: fmt.Sprintf("$up_%d", id)})
	}
	if raw.StructuredContent != nil && !closedStructure(t.Live.OutputSchema, raw.StructuredContent) {
		out.StructuredContent = nil
	}
	return out, sum, nil
}

// textOf gives the text of a content item: the text itself, or the JSON of any other kind.
func textOf(c mcp.Content) (string, error) {
	if tc, ok := c.(*mcp.TextContent); ok {
		return tc.Text, nil
	}
	b, err := json.Marshal(c)
	return string(b), err
}

// closedStructure reports whether a structured result fits the output schema and holds no
// free string. Without an output schema nothing is known about it, so it does not pass.
func closedStructure(outputSchema, value any) bool {
	if outputSchema == nil {
		return false
	}
	rs, err := resolve(outputSchema)
	if err != nil {
		return false
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return false
	}
	var instance any
	if err := json.Unmarshal(raw, &instance); err != nil || rs.Validate(instance) != nil {
		return false
	}
	schema, ok := outputSchema.(map[string]any)
	if !ok {
		return false
	}
	return !hasFreeString(schema, instance)
}

// hasFreeString walks a value along its schema. A string is free unless its schema has
// enum, const or format. Where the schema says nothing (no properties entry, a
// combinator such as anyOf), a string counts as free, and so does an object key.
func hasFreeString(schema map[string]any, v any) bool {
	switch x := v.(type) {
	case string:
		for _, k := range []string{"enum", "const", "format"} {
			if _, fixed := schema[k]; fixed {
				return false
			}
		}
		return true
	case []any:
		items, _ := schema["items"].(map[string]any)
		for _, e := range x {
			if hasFreeString(items, e) {
				return true
			}
		}
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		for k, e := range x {
			sub, known := props[k].(map[string]any)
			if !known {
				extra, isSchema := schema["additionalProperties"].(map[string]any)
				if !isSchema {
					return true // a key the schema does not name
				}
				sub = extra
			}
			if hasFreeString(sub, e) {
				return true
			}
		}
	}
	return false
}
