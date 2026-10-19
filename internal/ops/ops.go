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
//	POST /ops/txn/{key-hex}/reopen   a failed key of the operator's tenant becomes reopened
package ops

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

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
	// /ops/txn/{key}/reopen
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/ops/"), "/"), "/")
	if len(parts) != 3 || parts[0] != "txn" || parts[2] != "reopen" {
		rpcerr.WriteStatus(w, http.StatusNotFound, rpcerr.New(rpcerr.NotFound, ""), nil)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		rpcerr.WriteStatus(w, http.StatusMethodNotAllowed, rpcerr.New(rpcerr.SchemaInvalid, "method not allowed"), nil)
		return
	}
	h.reopen(w, r, c, parts[1])
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
