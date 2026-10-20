// Package mcpserver registers the gateway's tools in the go-sdk server.
//
// Input: tools/call requests that passed the edge (the caller's identity is in
// RequestExtra.TokenInfo). Output: a structured result, or a *jsonrpc.Error in
// the single error format. One wrapper serves every tool: the bucket of the
// tool (if it has one), the input schema, the taint check for tools with an
// effect, the service, the output schema, and one audit_log row.
//
// The tools have the same names in every configuration of the benchmark (option WithMode).
// The register* functions below say which service and which schemas a mode puts behind a
// name; the wrapper is the same for all of them.
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
	"github.com/SaintsFlow/edigate/internal/mode"
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

// schemaNames are the names of the schema files of a tool. Normally both are the name of the
// tool; the open tools of the benchmark modes use "plain/<name>".
type schemaNames struct{ in, out string }

type server struct {
	store  *store.Store
	reader quarantine.QuarantinedReader // nil: edi.extract_note answers quarantine_invalid
	policy tools.Decider                // decides every tool with an effect
	up     *upstream.Gateway            // nil: no upstream servers, every up.* name is not_found
	log    *slog.Logger

	mode  mode.Mode
	plain tools.PlainEnv // the clients of the open tools (modes plain and reqhash)

	// Schema checks; tests replace them to register tools that have no schema file yet.
	validateIn  func(tool string, args []byte) error
	validateOut func(tool string, body []byte) error
	// schemas names the schema files of the tools that do not use the default (set at registration).
	schemas map[string]schemaNames

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
		mode: mode.Full, schemas: map[string]schemaNames{},
		validateIn: contracts.Validate, validateOut: contracts.ValidateOutput,
	}
}

// Option changes how New builds the server.
type Option func(*server)

// WithUpstream gives the server the upstream MCP servers whose pinned tools it passes on
// as up.<server>.<tool>.
func WithUpstream(g *upstream.Gateway) Option { return func(s *server) { s.up = g } }

// WithMode makes the server play a configuration of the benchmark (default mode.Full).
func WithMode(m mode.Mode) Option { return func(s *server) { s.mode = m } }

// WithPlain gives the open tools of the modes plain and reqhash the clients of the other
// systems. The mode reqhash also switches on the stored answers.
func WithPlain(env tools.PlainEnv) Option { return func(s *server) { s.plain = env } }

// New builds the MCP server with the tools of the mode registered. reader is the
// quarantined reader behind edi.extract_note; nil switches that tool off. dec is
// the policy that decides the tools with an effect.
func New(st *store.Store, reader quarantine.QuarantinedReader, dec tools.Decider, log *slog.Logger, opts ...Option) *mcp.Server {
	s := newServer(st, reader, dec, log)
	for _, opt := range opts {
		opt(s)
	}
	s.plain.ReqKeys = s.mode == mode.ReqHash
	srv := mcp.NewServer(&mcp.Implementation{Name: "edigate", Version: "0.1.0-dev"}, nil)
	srv.AddReceivingMiddleware(s.upstreamMiddleware)
	s.registerReads(srv)
	if s.mode.NoteTool() {
		s.registerNote(srv)
	}
	switch {
	case s.mode.Open():
		s.registerOpen(srv)
	case s.mode == mode.NoContracts:
		s.registerModelValues(srv)
		s.registerStatus(srv)
	default:
		s.registerEffects(srv)
		s.registerStatus(srv)
	}
	s.registerPrices(srv)
	return srv
}

// caller is the identity of a call with the switches of the mode. args are the raw arguments.
func (s *server) caller(id edge.Identity, args json.RawMessage) tools.Caller {
	return tools.Caller{
		TenantID: id.TenantID, PrincipalID: id.PrincipalID, Kind: id.Kind, TraceID: id.TraceID,
		Args: args, ArgsKey: s.mode == mode.NoBizKey, RawValues: s.mode == mode.NoSanitize,
	}
}

// registerReads registers the tools that only read: edi.list_inbox and edi.read_document. In the
// modes with raw text the second shows the text of the partner.
func (s *server) registerReads(srv *mcp.Server) {
	s.add(srv, listInbox, "List the documents in the tenant's inbox.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.ListInboxInput
			if len(args) > 0 {
				if err := json.Unmarshal(args, &in); err != nil {
					return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
				}
			}
			return tools.ListInbox(ctx, s.store, id.TenantID, in)
		})
	if s.mode.RawText() {
		s.addWith(srv, readDocument, "Read one inbox document: the header and the lines as the partner wrote them, "+
			"and in texts every free text of the document with the place it came from. The header also lists "+
			"the known e-mail addresses of the partner in partner_contacts.",
			schemaNames{readDocument, "plain/" + readDocument},
			func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
				var in tools.ReadDocumentInput
				if err := json.Unmarshal(args, &in); err != nil {
					return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
				}
				return tools.ReadDocumentRaw(ctx, s.store, id.TenantID, in)
			})
		return
	}
	s.add(srv, readDocument, "Read one inbox document: typed header, lines and references. "+
		"The free text of the partner is not shown. It is kept in notes: the answer lists each note "+
		"with a reference like $note_1, its place in the document and why it is a note. The header also lists "+
		"the known e-mail addresses of the partner in partner_contacts.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.ReadDocumentInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.ReadDocument(ctx, s.store, id.TenantID, in)
		})
}

// registerNote registers edi.extract_note, the quarantined reader (not in the modes with raw text).
func (s *server) registerNote(srv *mcp.Server) {
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
			return tools.ExtractNote(ctx, s.store, s.reader, id.TenantID, id.TraceID, in)
		})
}

// registerPrices registers erp.get_price_list, the same in every mode.
func (s *server) registerPrices(srv *mcp.Server) {
	s.add(srv, priceList, "Read the prices of your price list for 1 to 50 SKUs. The answer holds a row for each SKU "+
		"that is in the list, with the unit price, the currency and the unit, and the label of the price list in price_list_id. "+
		"A SKU that is not in the list is left out.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.PriceListInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.GetPriceList(ctx, s.store, s.caller(id, args), in)
		})
}

// registerStatus registers txn.status of the full gateway (the key and the saga of the call).
func (s *server) registerStatus(srv *mcp.Server) {
	s.add(srv, txnStatus, "Show where a transaction stands: accepted (queued), completed (the effect is confirmed, "+
		"with the reference of the other system) or rejected. For a step of a saga it also shows saga_state.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.TxnStatusInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.TxnStatus(ctx, s.store, id.TenantID, in)
		})
}

// registerEffects registers the tools with an effect of the full gateway. They take a
// reference to a received document or to an order and nothing else the model could lie about.
func (s *server) registerEffects(srv *mcp.Server) {
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
			return tools.CreateSalesOrder(ctx, s.store, s.policy, s.caller(id, args), in)
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
			return tools.PostVendorInvoice(ctx, s.store, s.policy, s.caller(id, args), in)
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
			return tools.ConfirmOrder(ctx, s.store, s.policy, s.caller(id, args), in)
		})
	s.add(srv, wmsReserve, "Reserve the lines of a sales order in the warehouse. Pass the txn_id that "+
		"erp.create_sales_order gave. Calling again returns the same txn_id. Use txn.status to follow it.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.OrderRefInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.WMSReserve(ctx, s.store, s.policy, s.caller(id, args), in)
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
			return tools.SendASN(ctx, s.store, s.policy, s.caller(id, args), in)
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
			return tools.SendEmail(ctx, s.store, s.policy, s.caller(id, args), in)
		})
}

// openText are the descriptions of the tools with open arguments (the same in the three modes
// that use them). They say what the tool takes; none promises a check.
var openText = map[string]string{
	createSalesOrder: "Create a sales order in the ERP. Pass the partner, your order number (business_ref), the " +
		"ship-to code, the currency and the lines with sku, qty, unit and unit_price. An optional delivery_instruction " +
		"is a note for the carrier.",
	postVendorInvoice: "Book a supplier invoice in the ERP. Pass the supplier (partner_id), the invoice number, the " +
		"number of the order to the supplier, the currency, the total and the lines with sku, qty and unit_price.",
	confirmOrder: "Confirm an order to the partner with an order acknowledgment (855). Pass the partner, the order " +
		"number and the answer: AC (accepted), AD (accepted with changes) or RJ (rejected). Lines are optional; " +
		"without them the lines of the order are confirmed.",
	wmsReserve: "Reserve lines in the warehouse. Pass the order number (business_ref) and the lines with sku and qty.",
	sendASN: "Ship lines from the warehouse and notify the partner with a ship notice (856). Pass the partner, the " +
		"order number, the ship-to code and the lines with sku and qty. The SSCC of the packs come from the warehouse.",
	sendEmail: "Send a letter. Pass the address, the subject and the body.",
}

// registerOpen registers the open tools of the modes plain and reqhash: the arguments of the
// model, the call of the other system at once, no protection of the gateway.
func (s *server) registerOpen(srv *mcp.Server) {
	open := func(tool string, run func(ctx context.Context, st *store.Store, c tools.Caller, args json.RawMessage) (tools.PlainResult, error)) {
		s.addWith(srv, tool, openText[tool], schemaNames{"plain/" + tool, "plain/effect"},
			func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
				return run(ctx, s.store, s.caller(id, args), args)
			})
	}
	open(createSalesOrder, s.plain.PlainCreateSalesOrder)
	open(postVendorInvoice, s.plain.PlainPostVendorInvoice)
	open(confirmOrder, s.plain.PlainConfirmOrder)
	open(wmsReserve, s.plain.PlainWMSReserve)
	open(sendASN, s.plain.PlainSendASN)
	open(sendEmail, s.plain.PlainSendEmail)
	s.add(srv, txnStatus, "Show where a call stands: completed (the effect was done, with the reference of the other "+
		"system) or rejected.",
		func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
			var in tools.TxnStatusInput
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, rpcerr.New(rpcerr.SchemaInvalid, "")
			}
			return tools.PlainTxnStatus(ctx, s.store, id.TenantID, in)
		})
}

// registerModelValues registers the tools with an effect of the mode no-contracts: the
// arguments are those of the open tools, the answer and everything after the arguments are those
// of the full gateway.
func (s *server) registerModelValues(srv *mcp.Server) {
	open := func(tool string, run func(ctx context.Context, st *store.Store, dec tools.Decider, c tools.Caller, args json.RawMessage) (tools.OrderResult, error)) {
		s.addWith(srv, tool, openText[tool]+" The call is checked by the policy of the gateway; an amount above the limits of the "+
			"partner waits for an operator.", schemaNames{"plain/" + tool, tool},
			func(ctx context.Context, id edge.Identity, args json.RawMessage) (any, error) {
				return run(ctx, s.store, s.policy, s.caller(id, args), args)
			})
	}
	open(createSalesOrder, tools.OpenCreateSalesOrder)
	open(postVendorInvoice, tools.OpenPostVendorInvoice)
	open(confirmOrder, tools.OpenConfirmOrder)
	open(wmsReserve, tools.OpenWMSReserve)
	open(sendASN, tools.OpenSendASN)
	open(sendEmail, tools.OpenSendEmail)
}

// add registers a raw tool handler (not the typed mcp.AddTool) so that our own
// schema check produces the gateway's error format. The schemas are those of the tool name.
func (s *server) add(srv *mcp.Server, name, description string, svc service) {
	s.addWith(srv, name, description, schemaNames{name, name}, svc)
}

// addWith is add with the names of the schema files given.
func (s *server) addWith(srv *mcp.Server, name, description string, names schemaNames, svc service) {
	in, err := contracts.Input(names.in)
	if err != nil {
		panic(err)
	}
	out, err := contracts.Output(names.out)
	if err != nil {
		panic(err)
	}
	s.schemas[name] = names
	srv.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: in, OutputSchema: out},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return s.call(ctx, name, svc, req)
		})
}

// schemaOf returns the names of the schema files of a tool.
func (s *server) schemaOf(tool string) schemaNames {
	if n, ok := s.schemas[tool]; ok {
		return n
	}
	return schemaNames{tool, tool}
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
// tool with an effect (not in the open modes, which have no taint label), calls the
// service and builds the result.
func (s *server) run(ctx context.Context, tool string, svc service, id edge.Identity, args json.RawMessage) (*mcp.CallToolResult, error) {
	if !s.allow(tool, id.PrincipalID) {
		return nil, rpcerr.New(rpcerr.RateLimited, "")
	}
	if strings.HasPrefix(tool, upstream.Prefix) {
		return s.runUpstream(ctx, tool, id, args)
	}
	names := s.schemaOf(tool)
	if err := s.validateIn(names.in, args); err != nil {
		return nil, rpcerr.New(rpcerr.SchemaInvalid, "arguments do not match the tool schema")
	}
	if !s.mode.Open() {
		if err := taint.CheckSink(tool, args); err != nil {
			return nil, err
		}
	}
	out, err := svc(ctx, id, args)
	if err != nil {
		if _, reason := rpcerr.From(err); reason == rpcerr.Internal {
			s.log.Error("tool failed", "tool", tool, "error", err.Error())
		}
		return nil, err
	}
	body, err := json.Marshal(out)
	if err != nil || s.validateOut(names.out, body) != nil {
		s.log.Error("tool result does not match its output schema", "tool", tool)
		return nil, rpcerr.New(rpcerr.Internal, "internal error")
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(body)}},
		StructuredContent: json.RawMessage(body),
	}, nil
}
