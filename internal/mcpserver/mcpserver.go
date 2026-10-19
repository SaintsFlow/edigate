// Package mcpserver registers the gateway's tools in the go-sdk server.
//
// Input: tools/call requests that passed the edge (the caller's identity is in
// RequestExtra.TokenInfo). Output: a structured result, or a *jsonrpc.Error in
// the single error format. One wrapper serves every tool: check the input
// schema, call the service, write one audit_log row, answer.
package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/contracts"
	"github.com/SaintsFlow/edigate/internal/edge"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/tools"
)

const (
	listInbox    = "edi.list_inbox"
	readDocument = "edi.read_document"
)

type server struct {
	store *store.Store
	log   *slog.Logger
}

// service runs a tool for a tenant on arguments that already passed the schema.
// It returns a value to encode, or an error (rpcerr errors pass through,
// any other error becomes "internal").
type service func(ctx context.Context, tenantID string, args json.RawMessage) (any, error)

// New builds the MCP server with all tools registered.
func New(st *store.Store, log *slog.Logger) *mcp.Server {
	s := &server{store: st, log: log}
	srv := mcp.NewServer(&mcp.Implementation{Name: "edigate", Version: "0.1.0-dev"}, nil)
	s.add(srv, listInbox, "List the documents in the tenant's inbox.",
		func(ctx context.Context, tenantID string, args json.RawMessage) (any, error) {
			var in tools.ListInboxInput
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
				}
			}
			return tools.ListInbox(ctx, st, tenantID, in)
		})
	s.add(srv, readDocument, "Read one inbox document: typed header and lines, no free text.",
		func(ctx context.Context, tenantID string, args json.RawMessage) (any, error) {
			var in tools.ReadDocumentInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.ReadDocument(ctx, st, tenantID, in)
		})
	return srv
}

// add registers a raw tool handler (not the typed mcp.AddTool) so that our own
// schema check produces the gateway's error format.
func (s *server) add(srv *mcp.Server, name, description string, svc service) {
	in, err := contracts.Input(name)
	if err != nil {
		panic(err)
	}
	out, err := contracts.Output(name)
	if err != nil {
		panic(err)
	}
	srv.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: in, OutputSchema: out},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return s.call(ctx, name, svc, req)
		})
}

// call is the wrapper around every tool: schema check, service, audit row.
// Every call that has an identity leaves one audit row; if the row cannot be
// written the answer is an internal error.
func (s *server) call(ctx context.Context, tool string, svc service, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var id edge.Identity
	var ok bool
	if req.Extra != nil {
		id, ok = edge.IdentityFrom(req.Extra.TokenInfo)
	}
	if !ok {
		return nil, rpcerr.New(rpcerr.Unauthorized, "")
	}
	args := req.Params.Arguments
	sum := sha256.Sum256(args)

	result, callErr := s.run(ctx, tool, svc, id.TenantID, args)
	outcome := "ok"
	var wireErr *jsonrpc.Error
	if callErr != nil {
		var reason rpcerr.Reason
		wireErr, reason = rpcerr.From(callErr)
		outcome = string(reason)
	}

	auditErr := s.store.AppendAudit(ctx, store.AuditEvent{
		TenantID: id.TenantID, EventType: "tool_call", TraceID: id.TraceID, PrincipalID: id.PrincipalID,
		Tool: tool, PayloadSHA256: sum[:], Outcome: outcome,
	})
	if auditErr != nil {
		s.log.Error("audit write failed", "trace_id", id.TraceID, "principal_id", id.PrincipalID, "tool", tool, "error", auditErr.Error())
		return nil, rpcerr.New(rpcerr.Internal, "internal error")
	}
	s.log.Info("tool call", "trace_id", id.TraceID, "principal_id", id.PrincipalID, "tool", tool, "outcome", outcome)
	if wireErr != nil {
		return nil, wireErr
	}
	return result, nil // result is set whenever callErr is nil
}

// run checks the input, calls the service and builds the result.
func (s *server) run(ctx context.Context, tool string, svc service, tenantID string, args json.RawMessage) (*mcp.CallToolResult, error) {
	if err := contracts.Validate(tool, args); err != nil {
		return nil, rpcerr.New(rpcerr.SchemaInvalid, "arguments do not match the tool schema")
	}
	out, err := svc(ctx, tenantID, args)
	if err != nil {
		if _, reason := rpcerr.From(err); reason == rpcerr.Internal {
			s.log.Error("tool failed", "tool", tool, "error", err.Error())
		}
		return nil, err
	}
	body, err := json.Marshal(out)
	if err != nil || contracts.ValidateOutput(tool, body) != nil {
		s.log.Error("tool result does not match its output schema", "tool", tool)
		return nil, rpcerr.New(rpcerr.Internal, "internal error")
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(body)}},
		StructuredContent: json.RawMessage(body),
	}, nil
}
