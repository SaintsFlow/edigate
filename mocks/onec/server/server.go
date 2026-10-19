// Package server is mock-1c: a small HTTP service that behaves like the HTTP
// service of a 1C installation for sales orders, for the test stand.
//
// Input: POST /hs/orders (a new order) and GET /hs/orders?external_id= (a lookup).
// Output: JSON. It writes only to its own schema mock_onec in the stand's database,
// so that a benchmark can count effects with a query.
//
// A request with an external_id that is already there changes nothing and
// answers 200 with the stored order (an idempotent receiver). A request without an
// external_id always makes a new order: this is how a client without keys, like the
// baseline configuration C0 of the benchmark, behaves.
//
// Fault knobs (set from the environment by cmd, or by tests): the share of
// requests that commit the order and then answer 500; a fixed delay; a prefix of
// the business reference that gets a permanent refusal (422).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schemaSQL = `
CREATE SCHEMA IF NOT EXISTS mock_onec;
CREATE TABLE IF NOT EXISTS mock_onec.sales_orders (
    id           bigserial PRIMARY KEY,
    tenant_id    text NOT NULL,
    external_id  text NULL UNIQUE,
    partner_id   text NOT NULL,
    business_ref text NOT NULL,
    ship_to      text NOT NULL DEFAULT '',
    lines        jsonb NOT NULL,
    total        numeric(18, 2) NOT NULL,
    currency     char(3) NOT NULL,
    status       text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'cancelled')),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS mock_onec.vendor_invoices (
    id             bigserial PRIMARY KEY,
    tenant_id      text NOT NULL,
    external_id    text NULL UNIQUE,
    partner_id     text NOT NULL,
    invoice_number text NOT NULL,
    order_ref      text NOT NULL DEFAULT '',
    lines          jsonb NOT NULL,
    total          numeric(18, 2) NOT NULL,
    currency       char(3) NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);`

// Migrate creates the schema and the table of the mock if they are missing.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schemaSQL)
	return err
}

// Config holds the fault knobs.
type Config struct {
	FailAfterCommitRate float64       // MOCK_FAIL_AFTER_COMMIT_RATE: 0 to 1
	Latency             time.Duration // MOCK_LATENCY_MS
	RejectRefPrefix     string        // MOCK_REJECT_REF_PREFIX; "" rejects nothing
	FailCancel          bool          // MOCK_FAIL_CANCEL: every cancel answers 500 (the test of a failed compensation)
}

// Server is the HTTP handler of the mock.
type Server struct {
	pool *pgxpool.Pool
	mu   sync.Mutex // guards cfg: tests change the knobs while the server runs
	cfg  Config
}

// New builds the server on a pool whose database has the mock schema (see Migrate).
func New(pool *pgxpool.Pool, cfg Config) *Server { return &Server{pool: pool, cfg: cfg} }

// SetFailAfterCommitRate changes the share of requests that commit and then fail.
func (s *Server) SetFailAfterCommitRate(rate float64) {
	s.mu.Lock()
	s.cfg.FailAfterCommitRate = rate
	s.mu.Unlock()
}

// SetRejectRefPrefix changes the prefix of business references that get a 422.
func (s *Server) SetRejectRefPrefix(prefix string) {
	s.mu.Lock()
	s.cfg.RejectRefPrefix = prefix
	s.mu.Unlock()
}

// SetFailCancel makes every cancel answer 500 (or stops doing so).
func (s *Server) SetFailCancel(fail bool) {
	s.mu.Lock()
	s.cfg.FailCancel = fail
	s.mu.Unlock()
}

func (s *Server) config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Handler returns the routes of the mock.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hs/orders", s.create)
	mux.HandleFunc("GET /hs/orders", s.lookup)
	mux.HandleFunc("POST /hs/orders/{external_id}/cancel", s.cancel)
	mux.HandleFunc("POST /hs/vendor-invoices", s.createInvoice)
	mux.HandleFunc("GET /hs/vendor-invoices", s.lookupInvoice)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.pool.Ping(r.Context()); err != nil {
			reply(w, http.StatusServiceUnavailable, map[string]string{"error": "database unavailable"})
			return
		}
		reply(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// orderIn is the body of POST /hs/orders. Amounts are decimal strings.
type orderIn struct {
	TenantID    string          `json:"tenant_id"`
	ExternalID  *string         `json:"external_id"`
	PartnerID   string          `json:"partner_id"`
	BusinessRef string          `json:"business_ref"`
	ShipTo      string          `json:"ship_to"`
	Lines       json.RawMessage `json:"lines"`
	Total       string          `json:"total"`
	Currency    string          `json:"currency"`
}

var (
	reMoney    = regexp.MustCompile(`^[0-9]{1,16}(\.[0-9]{1,2})?$`)
	reCurrency = regexp.MustCompile(`^[A-Z]{3}$`)
)

func (o orderIn) valid() bool {
	var lines []json.RawMessage
	return o.TenantID != "" && o.PartnerID != "" && o.BusinessRef != "" &&
		(o.ExternalID == nil || *o.ExternalID != "") &&
		reMoney.MatchString(o.Total) && reCurrency.MatchString(o.Currency) &&
		json.Unmarshal(o.Lines, &lines) == nil && len(lines) > 0
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	if cfg.Latency > 0 {
		select {
		case <-time.After(cfg.Latency):
		case <-r.Context().Done():
			return
		}
	}
	var in orderIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil || !in.valid() {
		reply(w, http.StatusBadRequest, map[string]string{"error": "bad order"})
		return
	}
	if cfg.RejectRefPrefix != "" && strings.HasPrefix(in.BusinessRef, cfg.RejectRefPrefix) {
		reply(w, http.StatusUnprocessableEntity, map[string]string{"error": "order refused"})
		return
	}

	var id int64
	err := s.pool.QueryRow(r.Context(),
		`INSERT INTO mock_onec.sales_orders (tenant_id, external_id, partner_id, business_ref, ship_to, lines, total, currency)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::numeric, $8)
		 ON CONFLICT (external_id) DO NOTHING
		 RETURNING id`,
		in.TenantID, in.ExternalID, in.PartnerID, in.BusinessRef, in.ShipTo, []byte(in.Lines), in.Total, in.Currency).Scan(&id)
	switch {
	case err == nil:
		if rand.Float64() < cfg.FailAfterCommitRate {
			reply(w, http.StatusInternalServerError, map[string]string{"error": "lost the answer"}) // committed, answer lost
			return
		}
		reply(w, http.StatusCreated, map[string]any{"id": id, "external_id": in.ExternalID})
	case errors.Is(err, pgx.ErrNoRows): // the external id is already there
		o, err := s.find(r.Context(), *in.ExternalID)
		if err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
			return
		}
		reply(w, http.StatusOK, o)
	default:
		reply(w, http.StatusInternalServerError, map[string]string{"error": "write failed"})
	}
}

// cancel is the compensation of an order. It is idempotent: an order that is there becomes
// cancelled (again), an order that is not there is left alone and the answer is the same.
func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	if cfg.Latency > 0 {
		select {
		case <-time.After(cfg.Latency):
		case <-r.Context().Done():
			return
		}
	}
	if cfg.FailCancel {
		reply(w, http.StatusInternalServerError, map[string]string{"error": "cancel failed"})
		return
	}
	ext := r.PathValue("external_id")
	var n int
	err := s.pool.QueryRow(r.Context(),
		`WITH c AS (UPDATE mock_onec.sales_orders SET status = 'cancelled' WHERE external_id = $1 RETURNING 1)
		 SELECT count(*) FROM c`, ext).Scan(&n)
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": "write failed"})
		return
	}
	reply(w, http.StatusOK, map[string]any{"external_id": ext, "status": "cancelled", "found": n > 0})
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) {
	ext := r.URL.Query().Get("external_id")
	if ext == "" {
		reply(w, http.StatusBadRequest, map[string]string{"error": "external_id is required"})
		return
	}
	o, err := s.find(r.Context(), ext)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		reply(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case err != nil:
		reply(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
	default:
		reply(w, http.StatusOK, o)
	}
}

// orderOut is a stored order as the mock answers.
type orderOut struct {
	ID          int64           `json:"id"`
	TenantID    string          `json:"tenant_id"`
	ExternalID  *string         `json:"external_id"`
	PartnerID   string          `json:"partner_id"`
	BusinessRef string          `json:"business_ref"`
	ShipTo      string          `json:"ship_to"`
	Lines       json.RawMessage `json:"lines"`
	Total       string          `json:"total"`
	Currency    string          `json:"currency"`
	Status      string          `json:"status"`
}

func (s *Server) find(ctx context.Context, externalID string) (orderOut, error) {
	var o orderOut
	var lines []byte
	err := s.pool.QueryRow(ctx,
		`SELECT id, tenant_id, external_id, partner_id, business_ref, ship_to, lines, total::text, currency, status
		 FROM mock_onec.sales_orders WHERE external_id = $1`, externalID).
		Scan(&o.ID, &o.TenantID, &o.ExternalID, &o.PartnerID, &o.BusinessRef, &o.ShipTo, &lines, &o.Total, &o.Currency, &o.Status)
	o.Lines = lines
	return o, err
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
