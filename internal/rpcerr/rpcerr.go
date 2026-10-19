// Package rpcerr is the single error format of the gateway.
//
// Every error the gateway sends, from the edge or from a tool handler, carries
// a code, a short message and data.reason from one closed list. The edge writes
// it into an HTTP response with Write; tool handlers return the *jsonrpc.Error
// built by New and the SDK puts it on the wire unchanged. Internal error text
// never leaves the process: Internal always answers "internal error".
package rpcerr

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Reason is an entry of the closed list of error reasons.
type Reason string

const (
	Unauthorized         Reason = "unauthorized"
	Forbidden            Reason = "forbidden"
	NotFound             Reason = "not_found"
	SchemaInvalid        Reason = "schema_invalid"
	TaintedSink          Reason = "tainted_sink"
	PolicyDenied         Reason = "policy_denied"
	ApprovalRequired     Reason = "approval_required"
	ConflictingDuplicate Reason = "conflicting_duplicate"
	QuarantineInvalid    Reason = "quarantine_invalid"
	ManifestDrift        Reason = "manifest_drift"
	RateLimited          Reason = "rate_limited"
	Internal             Reason = "internal"
)

type entry struct {
	code int64
	http int
}

// table maps each reason to its JSON-RPC code and, for errors the edge can
// produce, its HTTP status (zero when the error only comes from tools).
var table = map[Reason]entry{
	// The go-sdk reserves -32001 (unknown), -32003 (client closing), -32004 (server closing)
	// and -32005 (rejected) and acts on them, so none of the codes below uses one.
	Unauthorized:         {-32030, http.StatusUnauthorized},
	Forbidden:            {-32031, http.StatusForbidden},
	NotFound:             {-32016, 0},
	SchemaInvalid:        {-32602, http.StatusBadRequest},
	TaintedSink:          {-32010, 0},
	PolicyDenied:         {-32011, 0},
	ApprovalRequired:     {-32012, 0},
	ConflictingDuplicate: {-32013, 0},
	QuarantineInvalid:    {-32014, 0},
	ManifestDrift:        {-32015, 0},
	RateLimited:          {-32029, http.StatusTooManyRequests},
	Internal:             {-32603, http.StatusInternalServerError},
}

// New builds the JSON-RPC error for a reason. The message is a short fixed
// text for the reason unless the caller gives a safe one.
func New(r Reason, message string) *jsonrpc.Error {
	return build(table[r].code, r, message)
}

// Denied is the policy_denied error with data.explain: the names of the checks
// that failed, without any data of the document or of a policy.
func Denied(explain []string) *jsonrpc.Error {
	data, _ := json.Marshal(struct {
		Reason  Reason   `json:"reason"`
		Explain []string `json:"explain"`
	}{PolicyDenied, explain})
	return &jsonrpc.Error{Code: table[PolicyDenied].code, Message: string(PolicyDenied), Data: data}
}

// HeaderMismatch is the error for MCP headers that disagree with the body:
// code -32020, reason schema_invalid.
func HeaderMismatch(message string) *jsonrpc.Error {
	return build(mcp.CodeHeaderMismatch, SchemaInvalid, message)
}

func build(code int64, r Reason, message string) *jsonrpc.Error {
	if message == "" {
		message = string(r)
	}
	data, _ := json.Marshal(map[string]string{"reason": string(r)})
	return &jsonrpc.Error{Code: code, Message: message, Data: data}
}

// From turns any error into the gateway format. A *jsonrpc.Error built by this
// package passes through; everything else becomes Internal with a fixed message,
// so internal error text never reaches the client.
func From(err error) (*jsonrpc.Error, Reason) {
	var wire *jsonrpc.Error
	if errors.As(err, &wire) {
		var d struct {
			Reason Reason `json:"reason"`
		}
		if json.Unmarshal(wire.Data, &d) == nil && d.Reason != "" {
			return wire, d.Reason
		}
	}
	return New(Internal, "internal error"), Internal
}

// Write sends a reason as an HTTP response with a JSON-RPC error body.
// id is the raw request id from the body, or nil for null.
func Write(w http.ResponseWriter, r Reason, message string, id json.RawMessage) {
	WriteStatus(w, table[r].http, New(r, message), id)
}

// WriteStatus sends any prepared error with the given HTTP status.
func WriteStatus(w http.ResponseWriter, status int, e *jsonrpc.Error, id json.RawMessage) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	body, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *jsonrpc.Error  `json:"error"`
	}{"2.0", id, e})
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"internal error"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
