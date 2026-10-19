package mcpserver

// The up.<server>.<tool> tools of the upstream MCP servers (flow P8). They are not
// registered in the SDK server: the list of the agent is extended after every tools/list,
// from a fresh read of the upstreams and the pins, and a call with the prefix up. is
// taken before the SDK looks for a registered tool. The call itself goes through the same
// wrapper as every other tool (bucket, audit row), with its own run in runUpstream.

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SaintsFlow/edigate/internal/edge"
	"github.com/SaintsFlow/edigate/internal/policy"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/taint"
	"github.com/SaintsFlow/edigate/internal/upstream"
)

// upstreamMiddleware adds the pinned upstream tools to tools/list and takes the calls of
// the tools with the prefix up. out of the SDK.
func (s *server) upstreamMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		switch method {
		case "tools/list":
			res, err := next(ctx, method, req)
			list, ok := res.(*mcp.ListToolsResult)
			if err != nil || !ok || s.up == nil {
				return res, err
			}
			var actor *upstream.Actor
			if extra := req.GetExtra(); extra != nil {
				if id, found := edge.IdentityFrom(extra.TokenInfo); found {
					actor = &upstream.Actor{TenantID: id.TenantID, PrincipalID: id.PrincipalID, TraceID: id.TraceID}
				}
			}
			for _, t := range s.up.Refresh(ctx, actor) {
				if t.Enabled() {
					list.Tools = append(list.Tools, t.Public())
				}
			}
			return list, nil
		case "tools/call":
			if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil && strings.HasPrefix(call.Params.Name, upstream.Prefix) {
				res, err := s.call(ctx, call.Params.Name, nil, call)
				if err != nil {
					return nil, err
				}
				return res, nil
			}
		}
		return next(ctx, method, req)
	}
}

// runUpstream is the run of a call to an upstream tool. The order is fixed: the pin of the
// tool now (forbidden, not_found or manifest_drift), the pinned input schema, the taint
// check (an up. tool is a sink), the policy (upstream.not_allowed), the call, the result.
func (s *server) runUpstream(ctx context.Context, tool string, id edge.Identity, args json.RawMessage) (*mcp.CallToolResult, error) {
	if s.up == nil {
		return nil, rpcerr.New(rpcerr.NotFound, "")
	}
	actor := upstream.Actor{TenantID: id.TenantID, PrincipalID: id.PrincipalID, TraceID: id.TraceID}
	t, err := s.up.Resolve(ctx, tool, actor)
	if err != nil {
		return nil, err
	}
	if err := t.ValidateInput(args); err != nil {
		return nil, err
	}
	if err := taint.CheckSink(tool, args); err != nil {
		return nil, err
	}
	if err := s.allowUpstream(ctx, id, tool, args); err != nil {
		return nil, err
	}
	raw, err := s.up.Call(ctx, t, args)
	if err != nil {
		s.log.Warn("upstream call failed", "tool", tool, "error", err.Error())
		return nil, rpcerr.New(rpcerr.Internal, "internal error")
	}
	res, sum, err := upstream.Shape(ctx, s.store, actor, t, raw)
	if err != nil {
		return nil, err
	}
	detail, err := json.Marshal(map[string]any{"server": t.Server, "tool": t.Live.Name, "quarantined": t.Quarantine, "is_error": raw.IsError})
	if err != nil {
		return nil, err
	}
	err = s.store.AppendAudit(ctx, store.AuditEvent{
		TenantID: id.TenantID, EventType: "upstream_result", TraceID: id.TraceID, PrincipalID: id.PrincipalID,
		Tool: tool, PayloadSHA256: sum[:], Outcome: "ok", Detail: detail,
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// allowUpstream asks the policy whether the tenant may call the tool. The policy sees the
// arguments as the effect; anything but a plain allow is policy_denied, with the rules.
func (s *server) allowUpstream(ctx context.Context, id edge.Identity, tool string, args json.RawMessage) error {
	if s.policy == nil {
		return rpcerr.New(rpcerr.Internal, "internal error")
	}
	effect := map[string]any{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &effect); err != nil {
			return rpcerr.New(rpcerr.SchemaInvalid, "")
		}
	}
	d, err := s.policy.Decide(ctx,
		policy.Principal{ID: id.PrincipalID, Kind: id.Kind, TenantID: id.TenantID},
		policy.ToolCall{Tool: tool, Effect: effect})
	if err != nil {
		return err
	}
	if d.Allow && d.Approval == policy.ApprovalNone {
		return nil
	}
	explain, err := json.Marshal(d)
	if err != nil {
		return err
	}
	err = s.store.AppendAudit(ctx, store.AuditEvent{
		TenantID: id.TenantID, EventType: "policy_denied", TraceID: id.TraceID, PrincipalID: id.PrincipalID,
		Tool: tool, Outcome: string(rpcerr.PolicyDenied), PolicyExplain: explain,
	})
	if err != nil {
		return err
	}
	rules := d.Deny
	if len(rules) == 0 {
		rules = d.Fired
	}
	return rpcerr.Denied(rules)
}
