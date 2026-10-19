// Package mcpserver registers the gateway's tools in the go-sdk server.
//
// Input: tools/call requests that passed the edge (the caller's identity is in
// RequestExtra.TokenInfo). Output: a structured result, or a *jsonrpc.Error in
// the single error format. One wrapper serves every tool: the bucket of the
// tool (if it has one), the input schema, the taint check for tools with an
// effect, the service, the output schema, and one audit_log row.
package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/time/rate"

	"github.com/SaintsFlow/edigate/internal/contracts"
	"github.com/SaintsFlow/edigate/internal/edge"
	"github.com/SaintsFlow/edigate/internal/quarantine"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
	"github.com/SaintsFlow/edigate/internal/taint"
	"github.com/SaintsFlow/edigate/internal/tools"
	"github.com/SaintsFlow/edigate/internal/upstream"
)

const (
	listInbox    = "edi.list_inbox"
	readDocument = "edi.read_document"
	extractNote  = "edi.extract_note"

	createSalesOrder  = tools.CreateOrderTool
	postVendorInvoice = tools.PostInvoiceTool
	confirmOrder      = tools.ConfirmOrderTool
	wmsReserve        = tools.WMSReserveTool
	sendASN           = tools.SendASNTool
	txnStatus         = "txn.status"
	priceList         = tools.PriceListTool
	sendEmail         = tools.SendEmailTool
)

// toolRates are the own buckets of tools, per principal, on top of the general
// bucket of the edge. A model call is slow and costs money, so edi.extract_note
// gets 2 calls per second with a burst of 2.
var toolRates = map[string]struct {
	rate  rate.Limit
	burst int
}{
	extractNote: {2, 2},
}

type server struct {
	store  *store.Store
	reader quarantine.QuarantinedReader // nil: edi.extract_note answers quarantine_invalid
	policy tools.Decider                // decides every tool with an effect
	up     *upstream.Gateway            // nil: no upstream servers, every up.* name is not_found
	log    *slog.Logger

	// Schema checks; tests replace them to register tools that have no schema file yet.
	validateIn  func(tool string, args []byte) error
	validateOut func(tool string, body []byte) error

	// One limiter per tool and principal. Principals are few, so keys are not swept.
	limitMu  sync.Mutex
	limiters map[string]*rate.Limiter
}

// service runs a tool for the caller on arguments that already passed the
// schema. It returns a value to encode, or an error (rpcerr errors pass
// through, any other error becomes "internal"). The identity comes from the
// edge, never from the arguments.
type service func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error)

func newServer(st *store.Store, reader quarantine.QuarantinedReader, dec tools.Decider, log *slog.Logger) *server {
	return &server{
		store: st, reader: reader, policy: dec, log: log, limiters: map[string]*rate.Limiter{},
		validateIn: contracts.Validate, validateOut: contracts.ValidateOutput,
	}
}

// Option changes how New builds the server.
type Option func(*server)

// WithUpstream gives the server the upstream MCP servers whose pinned tools it passes on
// as up.<server>.<tool>.
func WithUpstream(g *upstream.Gateway) Option { return func(s *server) { s.up = g } }

// New builds the MCP server with all tools registered. reader is the
// quarantined reader behind edi.extract_note; nil switches that tool off. dec is
// the policy that decides the tools with an effect.
func New(st *store.Store, reader quarantine.QuarantinedReader, dec tools.Decider, log *slog.Logger, opts ...Option) *mcp.Server {
	s := newServer(st, reader, dec, log)
	for _, opt := range opts {
		opt(s)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "edigate", Version: "0.1.0-dev"}, nil)
	srv.AddReceivingMiddleware(s.upstreamMiddleware)
	s.add(srv, listInbox, "List the documents in the tenant's inbox.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.ListInboxInput
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
				}
			}
			return tools.ListInbox(ctx, st, id.TenantID, in)
		})
	s.add(srv, readDocument, "Read one inbox document: typed header, lines and references. "+
		"The free text of the partner is not shown. It is kept in notes: the answer lists each note "+
		"with a reference like $note_1, its place in the document and why it is a note.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.ReadDocumentInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.ReadDocument(ctx, st, id.TenantID, in)
		})
	s.add(srv, extractNote, "Read one note of a document as a fixed set of values. Pass the document id, "+
		"the note reference and a schema: delivery_instruction (a delivery instruction from a fixed list), "+
		"packaging (a packaging kind from a fixed list and a pallet height in cm) or contact_request "+
		"(whether the partner asks for contact and by which channel). The answer holds only the values of "+
		"that schema, never the text of the note.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.ExtractNoteInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.ExtractNote(ctx, st, s.reader, id.TenantID, id.TraceID, in)
		})
	s.add(srv, createSalesOrder, "Create a sales order in the ERP from a received purchase order (850). Pass the "+
		"document id and, if the partner asked for one, a delivery instruction from the fixed list. Prices, partner "+
		"and quantities are taken by the gateway from the document and the price list. Calling again for the same "+
		"order returns the same txn_id and creates nothing new. An order above the limits of the partner is not "+
		"refused: the answer is pending_approval with an approval_id, and an operator decides. Use txn.status "+
		"with the txn_id to follow the order.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.CreateSalesOrderInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.CreateSalesOrder(ctx, st, dec, callerOf(id), in)
		})
	s.add(srv, postVendorInvoice, "Book a supplier invoice in the ERP from a received invoice (810). Pass the "+
		"document id. The amount and the lines are taken by the gateway from the document and checked against "+
		"the order to the supplier. An invoice always waits for two different operators: the answer is "+
		"pending_approval with an approval_id. Calling again for the same invoice returns the same txn_id. "+
		"Use txn.status with the txn_id to follow it.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.PostVendorInvoiceInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.PostVendorInvoice(ctx, st, dec, callerOf(id), in)
		})
	s.add(srv, confirmOrder, "Confirm a sales order to the partner with an order acknowledgment (855). Pass the txn_id "+
		"that erp.create_sales_order gave and the kind of answer: AC (accepted), AD (accepted with changes) or RJ "+
		"(rejected). The gateway builds the document from the order. It cannot be taken back, so a rejection or "+
		"an amount far from the order waits for an operator: the answer is pending_approval. The document leaves "+
		"after the order exists in the ERP. Use txn.status to follow it: saga_state turns done when the partner "+
		"has acknowledged it.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.ConfirmOrderInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.ConfirmOrder(ctx, st, dec, callerOf(id), in)
		})
	s.add(srv, wmsReserve, "Reserve the lines of a sales order in the warehouse. Pass the txn_id that "+
		"erp.create_sales_order gave. Calling again returns the same txn_id. Use txn.status to follow it.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.OrderRefInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.WMSReserve(ctx, st, dec, callerOf(id), in)
		})
	s.add(srv, sendASN, "Ship a reserved order and notify the partner with a ship notice (856). Pass the txn_id "+
		"that erp.create_sales_order gave; the reservation (wms.reserve) must be done first. The gateway builds the "+
		"notice from what the warehouse shipped, with the SSCC of every pack. It cannot be taken back, so a "+
		"shipment that differs much from the order waits for an operator. One notice per order.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.OrderRefInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.SendASN(ctx, st, dec, callerOf(id), in)
		})
	s.add(srv, priceList, "Read the prices of your price list for 1 to 50 SKUs. The answer holds a row for each SKU "+
		"that is in the list, with the unit price, the currency and the unit. A SKU that is not in the list is left out.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.PriceListInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.GetPriceList(ctx, st, callerOf(id), in)
		})
	s.add(srv, sendEmail, "Send a standard letter to a contact of the partner of a received order (850). Pass the "+
		"document id, the address (it must be a contact of that partner) and a template: tpl_order_received, "+
		"tpl_delivery_date or tpl_contact_back. The letter has no free text: it is made from the template and the "+
		"order number. One letter per order and template. Use txn.status with the txn_id to follow it.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.SendEmailInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.SendEmail(ctx, st, dec, callerOf(id), in)
		})
	s.add(srv, txnStatus, "Show where a transaction stands: accepted (queued), completed (the effect is confirmed, "+
		"with the reference of the other system) or rejected. For a step of a saga it also shows saga_state.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.TxnStatusInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.TxnStatus(ctx, st, id.TenantID, in)
		})
	return srv
}

func callerOf(id edge.Identity) tools.Caller {
	return tools.Caller{TenantID: id.TenantID, PrincipalID: id.PrincipalID, Kind: id.Kind, TraceID: id.TraceID}
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

	result, callErr := s.run(ctx, tool, svc, id, args)
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

// allow takes a token from the tool's own bucket for this principal. Tools
// without an own bucket always pass.
func (s *server) allow(tool, principal string) bool {
	cfg, ok := toolRates[tool]
	if !ok {
		return true
	}
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	key := tool + "|" + principal
	lim, found := s.limiters[key]
	if !found {
		lim = rate.NewLimiter(cfg.rate, cfg.burst)
		s.limiters[key] = lim
	}
	return lim.Allow()
}

// run checks the bucket and the input, refuses a note reference on its way to a
// tool with an effect, calls the service and builds the result.
func (s *server) run(ctx context.Context, tool string, svc service, id edge.Identity, args json.RawMessage) (*mcp.CallToolResult, error) {
	if !s.allow(tool, id.PrincipalID) {
		return nil, rpcerr.New(rpcerr.RateLimited, "")
	}
	if strings.HasPrefix(tool, upstream.Prefix) {
		return s.runUpstream(ctx, tool, id, args)
	}
	if err := s.validateIn(tool, args); err != nil {
		return nil, rpcerr.New(rpcerr.SchemaInvalid, "arguments do not match the tool schema")
	}
	if err := taint.CheckSink(tool, args); err != nil {
		return nil, err
	}
	out, err := svc(ctx, id, args)
	if err != nil {
		if _, reason := rpcerr.From(err); reason == rpcerr.Internal {
			s.log.Error("tool failed", "tool", tool, "error", err.Error())
		}
		return nil, err
	}
	body, err := json.Marshal(out)
	if err != nil || s.validateOut(tool, body) != nil {
		s.log.Error("tool result does not match its output schema", "tool", tool)
		return nil, rpcerr.New(rpcerr.Internal, "internal error")
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(body)}},
		StructuredContent: json.RawMessage(body),
	}, nil
}
