// Package inbound is the HTTP entrance for partners: POST /edi/inbound.
//
// Input: a raw X12 004010 interchange in the body, a partner bearer token and the
// headers Message-Id, AS2-From and AS2-To (an emulation of AS2 without MIME and
// signatures). Output: HTTP 200 with an MDN (a JSON receipt) when the
// interchange is stored or was already stored; HTTP 422 or 409 with an error MDN
// when it is refused; and, for faults before the receipt, the gateway's single
// error format (internal/rpcerr).
//
// Order, all on the server: address bucket and token (edge.Gate) -> only kind
// "partner" of an active partner of a tenant that has an interchange id ->
// bucket per partner -> body size and headers -> AS2-From and AS2-To against the
// token and the tenant -> strict parse -> ISA05:ISA06 and ISA07:ISA08 against
// the same -> one transaction that stores the interchange and its documents
// (a repeat of the same interchange returns the first MDN, another body under the
// same ISA13 is refused) -> one audit row.
//
// The MDN confirms transport, not content: a set rejected by the parser or by the
// role of the partner is stored as rejected and the MDN still says "processed".
package inbound

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/SaintsFlow/edigate/internal/edge"
	"github.com/SaintsFlow/edigate/internal/edi/x12"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/sanitize"
	"github.com/SaintsFlow/edigate/internal/store"
)

const (
	dispositionOK    = "processed"
	dispositionError = "processed/Error: unexpected-processing-error"
	maxMessageID     = 998
)

// MDN is the receipt for an interchange. Field order is fixed by the struct, so
// the same MDN always gives the same bytes.
type MDN struct {
	MessageID         string `json:"message_id"`
	OriginalMessageID string `json:"original_message_id"`
	MICSHA256         string `json:"mic_sha256"`
	Disposition       string `json:"disposition"`
}

func (m MDN) bytes() []byte {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // Message-Id is usually <id@host>
	_ = enc.Encode(m)
	return []byte(strings.TrimSuffix(b.String(), "\n"))
}

// Handler serves /edi/inbound.
type Handler struct {
	gate  *edge.Gate
	store *store.Store
	log   *slog.Logger
}

// New builds the handler. The gate is the same one /mcp uses, so both entrances
// share the bucket per client address.
func New(gate *edge.Gate, st *store.Store, log *slog.Logger) *Handler {
	return &Handler{gate: gate, store: st, log: log}
}

// reply is the outcome of one request: either an MDN (mdn set) or an error in
// the single format (reason set). outcome goes to the audit row; empty means no
// audit row for this reply.
type reply struct {
	status  int
	mdn     []byte
	reason  rpcerr.Reason
	message string
	outcome string
	sha     []byte // SHA-256 of the body, nil when the body was not read
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, ok := h.gate.Authenticate(w, r)
	if !ok {
		return
	}
	p := c.Principal
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		rpcerr.WriteStatus(w, http.StatusMethodNotAllowed, rpcerr.New(rpcerr.SchemaInvalid, "method not allowed"), nil)
		return
	}
	access, ok := h.partnerAccess(r.Context(), w, c)
	if !ok {
		return
	}
	if !h.gate.AllowPartner(p.TenantID, p.PartnerID) {
		h.gate.Reject(w, c.TraceID, p.ID, rpcerr.RateLimited, nil)
		return
	}

	rep := h.receive(r.Context(), w, r, p, access)
	if rep.outcome != "" {
		err := h.store.AppendAudit(r.Context(), store.AuditEvent{
			TenantID: p.TenantID, EventType: "edi_inbound", TraceID: c.TraceID, PrincipalID: p.ID,
			PayloadSHA256: rep.sha, Outcome: rep.outcome,
		})
		if err != nil {
			h.log.Error("audit write failed", "trace_id", c.TraceID, "principal_id", p.ID, "error", err.Error())
			rep = reply{status: http.StatusInternalServerError, reason: rpcerr.Internal}
		}
	}
	h.log.Info("edi inbound", "trace_id", c.TraceID, "principal_id", p.ID, "status", rep.status, "outcome", rep.outcome)
	if rep.reason != "" {
		rpcerr.WriteStatus(w, rep.status, rpcerr.New(rep.reason, rep.message), nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rep.status)
	_, _ = w.Write(rep.mdn)
}

// partnerAccess applies the checks that need the database: the token must be a
// partner token, the partner active and the tenant must have an interchange id.
// These refusals are logged but not audited (they cost nothing to repeat).
func (h *Handler) partnerAccess(ctx context.Context, w http.ResponseWriter, c edge.Caller) (store.PartnerAccess, bool) {
	p := c.Principal
	if p.Kind != "partner" {
		h.gate.Reject(w, c.TraceID, p.ID, rpcerr.Forbidden, nil)
		return store.PartnerAccess{}, false
	}
	access, err := h.store.PartnerAccess(ctx, p.TenantID, p.PartnerID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		h.gate.Reject(w, c.TraceID, p.ID, rpcerr.Forbidden, nil)
		return store.PartnerAccess{}, false
	case err != nil:
		h.log.Error("partner lookup failed", "trace_id", c.TraceID, "error", err.Error())
		h.gate.Reject(w, c.TraceID, p.ID, rpcerr.Internal, nil)
		return store.PartnerAccess{}, false
	case !access.Active || access.GatewayID == "":
		h.gate.Reject(w, c.TraceID, p.ID, rpcerr.Forbidden, nil)
		return store.PartnerAccess{}, false
	}
	return access, true
}

// receive reads and checks the request, parses and stores the interchange and
// builds the reply. It writes nothing itself.
func (h *Handler) receive(ctx context.Context, w http.ResponseWriter, r *http.Request, p store.Principal, access store.PartnerAccess) reply {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, x12.MaxBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return reply{status: http.StatusRequestEntityTooLarge, reason: rpcerr.SchemaInvalid, message: "request body too large", outcome: "schema_invalid"}
		}
		return reply{status: http.StatusBadRequest, reason: rpcerr.SchemaInvalid, outcome: "schema_invalid"}
	}
	sum := sha256.Sum256(body)
	refuse := func(status int, reason rpcerr.Reason) reply {
		return reply{status: status, reason: reason, outcome: string(reason), sha: sum[:]}
	}

	messageID, from, to := r.Header.Get("Message-Id"), r.Header.Get("AS2-From"), r.Header.Get("AS2-To")
	if !validMessageID(messageID) || from == "" || to == "" {
		return refuse(http.StatusBadRequest, rpcerr.SchemaInvalid)
	}
	if from != p.PartnerID || to != access.GatewayID {
		return refuse(http.StatusForbidden, rpcerr.Forbidden)
	}

	ic, err := x12.Parse(body)
	if err != nil {
		return h.envelopeRefused(ctx, p, err, body, sum[:], messageID)
	}
	if ic.SenderID() != p.PartnerID || ic.ReceiverID() != access.GatewayID {
		return refuse(http.StatusForbidden, rpcerr.Forbidden)
	}

	docs, err := buildDocuments(ic, p.PartnerID, access.Role)
	if err != nil {
		h.log.Error("build documents failed", "error", err.Error())
		return reply{status: http.StatusInternalServerError, reason: rpcerr.Internal, outcome: "internal", sha: sum[:]}
	}
	mdn := MDN{MessageID: newUUID() + "@edigate", OriginalMessageID: messageID, MICSHA256: hex.EncodeToString(sum[:]), Disposition: dispositionOK}
	saved, err := h.store.SaveInbound(ctx, store.InboundInterchange{
		TenantID: p.TenantID, PartnerID: p.PartnerID, ISA13: ic.ControlNumber(), MessageID: messageID,
		Raw: body, SHA256: sum[:], MDN: mdn.bytes(), Documents: docs,
	})
	if err != nil {
		h.log.Error("store interchange failed", "error", err.Error())
		return reply{status: http.StatusInternalServerError, reason: rpcerr.Internal, outcome: "internal", sha: sum[:]}
	}

	switch saved.Outcome {
	case store.SavedDuplicate:
		var first MDN
		if err := json.Unmarshal(saved.MDN, &first); err != nil {
			h.log.Error("stored MDN is unreadable", "error", err.Error())
			return reply{status: http.StatusInternalServerError, reason: rpcerr.Internal, outcome: "internal", sha: sum[:]}
		}
		return reply{status: http.StatusOK, mdn: first.bytes(), outcome: "duplicate", sha: sum[:]}
	case store.SavedConflict:
		mdn.Disposition = dispositionError
		mdn.MessageID = newUUID() + "@edigate"
		return reply{status: http.StatusConflict, mdn: mdn.bytes(), outcome: "isa13_conflict", sha: sum[:]}
	}
	return reply{status: http.StatusOK, mdn: mdn.bytes(), outcome: "ok", sha: sum[:]}
}

// envelopeRefused stores a refused interchange for the operator and builds the
// 422 reply with an error MDN.
func (h *Handler) envelopeRefused(ctx context.Context, p store.Principal, parseErr error, body, sum []byte, messageID string) reply {
	var env *x12.EnvelopeError
	if !errors.As(parseErr, &env) {
		return reply{status: http.StatusInternalServerError, reason: rpcerr.Internal, outcome: "internal", sha: sum}
	}
	isa13 := ""
	if len(body) >= 106 && body[89] == body[3] && body[99] == body[3] && strings.Trim(string(body[90:99]), "0123456789") == "" {
		isa13 = string(body[90:99]) // nine digits between two separators where ISA13 stands
	}
	if err := h.store.SaveRejected(ctx, p.TenantID, p.PartnerID, isa13, body, sum, env.Reason+": "+env.Detail); err != nil {
		h.log.Error("store refused interchange failed", "error", err.Error())
		return reply{status: http.StatusInternalServerError, reason: rpcerr.Internal, outcome: "internal", sha: sum}
	}
	mdn := MDN{MessageID: newUUID() + "@edigate", OriginalMessageID: messageID, MICSHA256: hex.EncodeToString(sum), Disposition: dispositionError}
	return reply{status: http.StatusUnprocessableEntity, mdn: mdn.bytes(), outcome: "envelope_error", sha: sum}
}

// validMessageID: 1 to 998 printable ASCII characters.
func validMessageID(s string) bool {
	return s != "" && len(s) <= maxMessageID && x12.Printable(s) == s
}

// allowed lists the sets each partner role may send.
var allowed = map[string][]string{
	"customer": {"850", "997"},
	"supplier": {"810", "997"},
}

func roleAllows(role, st01 string) bool {
	for _, s := range allowed[role] {
		if s == st01 {
			return true
		}
	}
	return false
}

// buildDocuments makes one row per set: rejected with the parser's errors,
// rejected with role_not_allowed, or received with the sanitized body and its
// notes (internal/sanitize).
//
// The key (interchange, GS06, ST02) must be unique in the table. ST02 of a set
// that repeats an earlier one, or that is not a valid ST02, is stored as "#n";
// a real ST02 has at least four characters, so the substitute cannot collide.
func buildDocuments(ic *x12.Interchange, partnerID, role string) ([]store.InboundDocument, error) {
	var docs []store.InboundDocument
	seen := map[string]bool{}
	substitutes := 0
	for gi := range ic.Groups {
		g := &ic.Groups[gi]
		gs06 := g.GS.Value(6)
		for si := range g.Sets {
			s := &g.Sets[si]
			st01, st02 := s.ST.Value(1), s.ST.Value(2)
			key := gs06 + "|" + st02
			if seen[key] || len(st02) < 4 || len(st02) > 9 || x12.Printable(st02) != st02 {
				substitutes++
				st02 = fmt.Sprintf("#%d", substitutes)
			}
			seen[key] = true
			if len(st01) > 10 {
				st01 = st01[:10]
			}
			d := store.InboundDocument{GS06: gs06, ST01: x12.Printable(st01), ST02: st02, Status: "rejected"}

			switch {
			case len(s.Errors) > 0:
				errs, err := json.Marshal(s.Errors)
				if err != nil {
					return nil, err
				}
				d.Errors = errs
			case !roleAllows(role, st01):
				d.Errors = []byte(`[{"reason":"role_not_allowed"}]`)
			default:
				built, err := sanitize.Build(s, partnerID)
				if err != nil {
					return nil, err
				}
				d.Status, d.BusinessRef, d.Body = "received", built.BusinessRef, built.Body
				for _, n := range built.Notes {
					d.Notes = append(d.Notes, store.InboundNote{
						Ref: n.Ref, SegmentPath: n.SegmentPath, Reason: n.Reason, Body: n.Text,
						TaintLabel: n.TaintLabel, Canary: n.Canary,
					})
				}
			}
			docs = append(docs, d)
		}
	}
	return docs, nil
}

// newUUID returns a random version 4 UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
