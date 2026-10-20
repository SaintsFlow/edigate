// Package ops is the HTTP entrance of operators: /ops/. It is the border TB5:
// an operator works through the gateway with a token, so that every action is
// tied to a person in the audit log (and, from wave 2-6, the rule of four eyes
// can tell two operators apart).
//
// Input: requests to /ops/ with a Bearer token of an operator. Output: JSON, and
// errors in the single format of the gateway (internal/rpcerr).
//
// Order: address bucket and token (edge.Gate) -> only kind "operator" (403
// forbidden otherwise) -> method and route -> the action.
//
// Routes:
//
//	POST /ops/txn/{key-hex}/reopen       a failed key of the operator's tenant becomes reopened
//	GET  /ops/approvals?status=pending   the approval requests of the operator's tenant
//	POST /ops/approvals/{id}/approve     one approval of the operator (T3 needs two operators)
//	POST /ops/approvals/{id}/reject      the request is rejected and its key fails
package ops

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/SaintsFlow/edigate/internal/edge"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
)

// Handler serves /ops/.
type Handler struct {
	gate  *edge.Gate
	store *store.Store
	log   *slog.Logger
}

// New builds the handler. The gate is the one /mcp and /edi/inbound use.
func New(gate *edge.Gate, st *store.Store, log *slog.Logger) *Handler {
	return &Handler{gate: gate, store: st, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, ok := h.gate.Authenticate(w, r)
	if !ok {
		return
	}
	p := c.Principal
	if p.Kind != "operator" {
		h.gate.Reject(w, c.TraceID, p.ID, rpcerr.Forbidden, nil)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/ops/"), "/"), "/")
	switch {
	case len(parts) == 3 && parts[0] == "txn" && parts[2] == "reopen":
		if !h.method(w, r, http.MethodPost) {
			return
		}
		h.reopen(w, r, c, parts[1])
	case len(parts) == 1 && parts[0] == "approvals":
		if !h.method(w, r, http.MethodGet) {
			return
		}
		h.listApprovals(w, r, c)
	case len(parts) == 3 && parts[0] == "approvals" && (parts[2] == "approve" || parts[2] == "reject"):
		if !h.method(w, r, http.MethodPost) {
			return
		}
		h.decide(w, r, c, parts[1], parts[2] == "approve")
	default:
		rpcerr.WriteStatus(w, http.StatusNotFound, rpcerr.New(rpcerr.NotFound, ""), nil)
	}
}

// method answers 405 and reports false when the request has another method.
func (h *Handler) method(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method == want {
		return true
	}
	w.Header().Set("Allow", want)
	rpcerr.WriteStatus(w, http.StatusMethodNotAllowed, rpcerr.New(rpcerr.SchemaInvalid, "method not allowed"), nil)
	return false
}

func (h *Handler) internal(w http.ResponseWriter, c edge.Caller, what string, err error) {
	h.log.Error(what, "trace_id", c.TraceID, "principal_id", c.Principal.ID, "error", err.Error())
	rpcerr.WriteStatus(w, http.StatusInternalServerError, rpcerr.New(rpcerr.Internal, "internal error"), nil)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

var (
	reApprovalID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	statuses     = map[string]bool{"pending": true, "approved": true, "rejected": true, "expired": true}
)

// approvalOut is a request as the list shows it. The fields are listed one by one
// on purpose: the payload of the effect is not among them.
type approvalOut struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Tool        string          `json:"tool"`
	Tier        int             `json:"tier"`
	Approvers   int             `json:"approvers"`
	Status      string          `json:"status"`
	RequestedBy string          `json:"requested_by"`
	DecidedBy   []string        `json:"decided_by"`
	RequestedAt time.Time       `json:"requested_at"`
	ExpiresAt   time.Time       `json:"expires_at"`
	Rules       json.RawMessage `json:"rules"`
	View        json.RawMessage `json:"view"`
}

// listApprovals shows the requests of the operator's tenant with a status (default pending).
func (h *Handler) listApprovals(w http.ResponseWriter, r *http.Request, c edge.Caller) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}
	if !statuses[status] {
		rpcerr.WriteStatus(w, http.StatusBadRequest, rpcerr.New(rpcerr.SchemaInvalid, "unknown status"), nil)
		return
	}
	rows, err := h.store.ListApprovals(r.Context(), c.Principal.TenantID, status)
	if err != nil {
		h.internal(w, c, "list approvals failed", err)
		return
	}
	out := make([]approvalOut, 0, len(rows))
	for _, a := range rows {
		out = append(out, approvalOut{
			ID: a.ID, Kind: a.Kind, Tool: a.Tool, Tier: a.Tier, Approvers: a.Approvers, Status: a.Status,
			RequestedBy: a.RequestedBy, DecidedBy: a.DecidedBy, RequestedAt: a.RequestedAt, ExpiresAt: a.ExpiresAt,
			Rules: a.Rules, View: a.View,
		})
	}
	writeJSON(w, map[string]any{"approvals": out})
}

// decide records an approve or a reject of the operator. A request of another
// tenant and an unknown one both give 404 not_found; a request that is not pending
// and an operator who already decided give 409.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request, c edge.Caller, id string, approve bool) {
	if !reApprovalID.MatchString(id) {
		rpcerr.WriteStatus(w, http.StatusBadRequest, rpcerr.New(rpcerr.SchemaInvalid, "bad approval id"), nil)
		return
	}
	p := c.Principal
	d, err := h.store.DecideApproval(r.Context(), p.TenantID, p.ID, c.TraceID, id, approve)
	switch {
	case errors.Is(err, store.ErrNotFound):
		rpcerr.WriteStatus(w, http.StatusNotFound, rpcerr.New(rpcerr.NotFound, ""), nil)
	case errors.Is(err, store.ErrNotPending):
		rpcerr.WriteStatus(w, http.StatusConflict, rpcerr.New(rpcerr.SchemaInvalid, "approval is not pending"), nil)
	case errors.Is(err, store.ErrAlreadyDecided):
		rpcerr.WriteStatus(w, http.StatusConflict, rpcerr.New(rpcerr.SchemaInvalid, "already decided by this operator"), nil)
	case errors.Is(err, store.ErrOwnRequest):
		rpcerr.WriteStatus(w, http.StatusForbidden, rpcerr.New(rpcerr.Forbidden, ""), nil)
	case err != nil:
		h.internal(w, c, "decide approval failed", err)
	default:
		h.log.Info("approval decided", "trace_id", c.TraceID, "principal_id", p.ID, "approval_id", id, "status", d.Status)
		writeJSON(w, map[string]any{"approval_id": d.ID, "status": d.Status, "approvals": d.Approvals, "approvers": d.Approvers})
	}
}

// reopen moves a failed key of the operator's tenant to "reopened". A key of
// another tenant and an unknown key both give 404 not_found.
func (h *Handler) reopen(w http.ResponseWriter, r *http.Request, c edge.Caller, keyHex string) {
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		rpcerr.WriteStatus(w, http.StatusBadRequest, rpcerr.New(rpcerr.SchemaInvalid, "key must be 64 hex characters"), nil)
		return
	}
	p := c.Principal
	err = h.store.ReopenKey(r.Context(), p.TenantID, p.ID, c.TraceID, key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		rpcerr.WriteStatus(w, http.StatusNotFound, rpcerr.New(rpcerr.NotFound, ""), nil)
	case errors.Is(err, store.ErrNotFailed):
		rpcerr.WriteStatus(w, http.StatusConflict, rpcerr.New(rpcerr.SchemaInvalid, "key is not failed"), nil)
	case err != nil:
		h.log.Error("reopen failed", "trace_id", c.TraceID, "principal_id", p.ID, "error", err.Error())
		rpcerr.WriteStatus(w, http.StatusInternalServerError, rpcerr.New(rpcerr.Internal, "internal error"), nil)
	default:
		h.log.Info("txn reopened", "trace_id", c.TraceID, "principal_id", p.ID)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"key": hex.EncodeToString(key), "state": "reopened"})
	}
}
