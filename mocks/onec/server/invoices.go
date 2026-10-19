package server

// Supplier invoices of mock-1c: POST /hs/vendor-invoices and
// GET /hs/vendor-invoices?external_id=. They work like the orders: a request with an
// external_id that is already there changes nothing and answers 200 with the stored
// invoice, and the same fault knobs apply (the reject prefix is matched against the
// invoice number).

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// invoiceIn is the body of POST /hs/vendor-invoices. Amounts are decimal strings.
type invoiceIn struct {
	TenantID      string          `json:"tenant_id"`
	ExternalID    *string         `json:"external_id"`
	PartnerID     string          `json:"partner_id"`
	InvoiceNumber string          `json:"invoice_number"`
	OrderRef      string          `json:"order_ref"`
	Lines         json.RawMessage `json:"lines"`
	Total         string          `json:"total"`
	Currency      string          `json:"currency"`
}

func (i invoiceIn) valid() bool {
	var lines []json.RawMessage
	return i.TenantID != "" && i.PartnerID != "" && i.InvoiceNumber != "" &&
		(i.ExternalID == nil || *i.ExternalID != "") &&
		reMoney.MatchString(i.Total) && reCurrency.MatchString(i.Currency) &&
		json.Unmarshal(i.Lines, &lines) == nil && len(lines) > 0
}

func (s *Server) createInvoice(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	if cfg.Latency > 0 {
		select {
		case <-time.After(cfg.Latency):
		case <-r.Context().Done():
			return
		}
	}
	var in invoiceIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil || !in.valid() {
		reply(w, http.StatusBadRequest, map[string]string{"error": "bad invoice"})
		return
	}
	if cfg.RejectRefPrefix != "" && strings.HasPrefix(in.InvoiceNumber, cfg.RejectRefPrefix) {
		reply(w, http.StatusUnprocessableEntity, map[string]string{"error": "invoice refused"})
		return
	}

	var id int64
	err := s.pool.QueryRow(r.Context(),
		`INSERT INTO mock_onec.vendor_invoices (tenant_id, external_id, partner_id, invoice_number, order_ref, lines, total, currency)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8)
		 ON CONFLICT (external_id) DO NOTHING
		 RETURNING id`,
		in.TenantID, in.ExternalID, in.PartnerID, in.InvoiceNumber, in.OrderRef, []byte(in.Lines), in.Total, in.Currency).Scan(&id)
	switch {
	case err == nil:
		if rand.Float64() < cfg.FailAfterCommitRate {
			reply(w, http.StatusInternalServerError, map[string]string{"error": "lost the answer"}) // committed, answer lost
			return
		}
		reply(w, http.StatusCreated, map[string]any{"id": id, "external_id": in.ExternalID})
	case errors.Is(err, pgx.ErrNoRows): // the external id is already there
		inv, err := s.findInvoice(r.Context(), *in.ExternalID)
		if err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
			return
		}
		reply(w, http.StatusOK, inv)
	default:
		reply(w, http.StatusInternalServerError, map[string]string{"error": "write failed"})
	}
}

func (s *Server) lookupInvoice(w http.ResponseWriter, r *http.Request) {
	ext := r.URL.Query().Get("external_id")
	if ext == "" {
		reply(w, http.StatusBadRequest, map[string]string{"error": "external_id is required"})
		return
	}
	inv, err := s.findInvoice(r.Context(), ext)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		reply(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case err != nil:
		reply(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
	default:
		reply(w, http.StatusOK, inv)
	}
}

// invoiceOut is a stored invoice as the mock answers.
type invoiceOut struct {
	ID            int64           `json:"id"`
	TenantID      string          `json:"tenant_id"`
	ExternalID    *string         `json:"external_id"`
	PartnerID     string          `json:"partner_id"`
	InvoiceNumber string          `json:"invoice_number"`
	OrderRef      string          `json:"order_ref"`
	Lines         json.RawMessage `json:"lines"`
	Total         string          `json:"total"`
	Currency      string          `json:"currency"`
}

func (s *Server) findInvoice(ctx context.Context, externalID string) (invoiceOut, error) {
	var o invoiceOut
	var lines []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, external_id, partner_id, invoice_number, order_ref, lines, total::text, currency
		 FROM mock_onec.vendor_invoices WHERE external_id = $1`, externalID).
		Scan(&o.ID, &o.TenantID, &o.ExternalID, &o.PartnerID, &o.InvoiceNumber, &o.OrderRef, &lines, &o.Total, &o.Currency)
	o.Lines = lines
	return o, err
}
