// Package server is mock-wms: a small HTTP service that behaves like the warehouse system
// for the test stand.
//
// Input: POST /reservations (reserve the lines of an order), POST
// /reservations/{external_id}/release (undo it), POST /shipments (ship an order), and
// GET /reservations?external_id= and GET /shipments?external_id= (lookups). Output: JSON.
// It writes only to its own schema mock_wms, so that a test can count effects with a query.
//
// A request with an external_id that is already there changes nothing and answers 200
// with the stored row (an idempotent receiver). A release is idempotent too: it releases
// what is there and does nothing for what is not.
//
// A shipment is the fact of the warehouse: it records the quantity that left (the ordered
// quantity times MOCK_SHIP_QTY_FACTOR) and one SSCC per line, derived from the external id so
// that the same shipment always has the same labels.
//
// Fault knobs (from the environment, or set by tests): the share of requests that commit and
// then answer 500, a fixed delay, a business reference prefix that gets a permanent refusal
// (422; for shipments only, so that a reservation can succeed and be compensated), and the
// quantity factor of a shipment.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SaintsFlow/edigate/internal/decimal"
	"github.com/SaintsFlow/edigate/mocks/control"
)

const schemaSQL = `
CREATE SCHEMA IF NOT EXISTS mock_wms;
CREATE TABLE IF NOT EXISTS mock_wms.reservations (
    id          bigserial PRIMARY KEY,
    tenant_id   text NOT NULL,
    external_id text NULL UNIQUE,
    order_ref   text NOT NULL,
    lines       jsonb NOT NULL,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'released')),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS mock_wms.shipments (
    id          bigserial PRIMARY KEY,
    tenant_id   text NOT NULL,
    external_id text NULL UNIQUE,
    order_ref   text NOT NULL,
    lines       jsonb NOT NULL,
    sscc        text[] NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE mock_wms.reservations ALTER COLUMN external_id DROP NOT NULL;
ALTER TABLE mock_wms.shipments ALTER COLUMN external_id DROP NOT NULL;`

// Migrate creates the schema and the tables of the mock if they are missing.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schemaSQL)
	return err
}

// Config holds the fault knobs.
type Config struct {
	FailAfterCommitRate float64       // MOCK_FAIL_AFTER_COMMIT_RATE: 0 to 1
	Latency             time.Duration // MOCK_LATENCY_MS
	RejectRefPrefix     string        // MOCK_REJECT_REF_PREFIX: shipments of an order with this prefix get 422
	ShipQtyFactor       string        // MOCK_SHIP_QTY_FACTOR: decimal multiplier of the shipped quantity; "" means 1
}

// Server is the HTTP handler of the mock.
type Server struct {
	pool *pgxpool.Pool
	mu   sync.Mutex // guards cfg: tests change the knobs while the server runs
	cfg  Config
	ctl  *control.Store // knobs per tenant (POST /control), over cfg
}

// New builds the server on a pool whose database has the mock schema (see Migrate).
func New(pool *pgxpool.Pool, cfg Config) *Server {
	return &Server{pool: pool, cfg: cfg, ctl: control.New(true)}
}

// SetRejectRefPrefix changes the prefix of business references whose shipment gets a 422.
func (s *Server) SetRejectRefPrefix(prefix string) {
	s.mu.Lock()
	s.cfg.RejectRefPrefix = prefix
	s.mu.Unlock()
}

// SetShipQtyFactor changes the multiplier of the shipped quantity.
func (s *Server) SetShipQtyFactor(factor string) {
	s.mu.Lock()
	s.cfg.ShipQtyFactor = factor
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
	mux.HandleFunc("POST /reservations", s.reserve)
	mux.HandleFunc("GET /reservations", s.lookupReservation)
	mux.HandleFunc("POST /reservations/{external_id}/release", s.release)
	mux.HandleFunc("POST /shipments", s.ship)
	mux.HandleFunc("GET /shipments", s.lookupShipment)
	s.ctl.Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.pool.Ping(r.Context()); err != nil {
			reply(w, http.StatusServiceUnavailable, map[string]string{"error": "database unavailable"})
			return
		}
		reply(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// line is a line of an order. Qty has up to three decimal places.
type line struct {
	SKU  string `json:"sku"`
	Qty  string `json:"qty"`
	Unit string `json:"unit"`
}

// request is the body of POST /reservations and POST /shipments. An empty external_id is a
// request without a key (a client like the baseline C0 sends none): it always makes a new row.
type request struct {
	TenantID   string `json:"tenant_id"`
	ExternalID string `json:"external_id"`
	OrderRef   string `json:"order_ref"`
	Lines      []line `json:"lines"`
}

var reQty = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,3})?$`)

func (q request) valid() bool {
	if q.TenantID == "" || q.OrderRef == "" || len(q.Lines) == 0 {
		return false
	}
	for _, l := range q.Lines {
		if l.SKU == "" || l.Unit == "" || !reQty.MatchString(l.Qty) {
			return false
		}
	}
	return true
}

// begin reads the body, then applies the delay of the tenant. It returns the config in force for
// that tenant (the knobs of POST /control over the process-wide ones). It writes the answer and
// returns false when the request is not usable.
func (s *Server) begin(w http.ResponseWriter, r *http.Request) (request, Config, bool) {
	var in request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil || !in.valid() {
		reply(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return request{}, Config{}, false
	}
	cfg := s.forTenant(s.config(), in.TenantID)
	if cfg.Latency > 0 {
		select {
		case <-time.After(cfg.Latency):
		case <-r.Context().Done():
			return request{}, Config{}, false
		}
	}
	return in, cfg, true
}

// forTenant puts the knobs set for one tenant over the process-wide config. Release has no tenant
// in its request and uses the process-wide config only.
func (s *Server) forTenant(cfg Config, tenant string) Config {
	k := s.ctl.Get(tenant)
	cfg.FailAfterCommitRate = k.Rate(cfg.FailAfterCommitRate)
	cfg.Latency = k.Latency(cfg.Latency)
	cfg.RejectRefPrefix = k.Prefix(cfg.RejectRefPrefix)
	cfg.ShipQtyFactor = k.Factor(cfg.ShipQtyFactor)
	return cfg
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	in, cfg, ok := s.begin(w, r)
	if !ok {
		return
	}
	lines, _ := json.Marshal(in.Lines)
	var id int64
	err := s.pool.QueryRow(r.Context(),
		`INSERT INTO mock_wms.reservations (tenant_id, external_id, order_ref, lines)
		 VALUES ($1, NULLIF($2, ''), $3, $4) ON CONFLICT (external_id) DO NOTHING RETURNING id`,
		in.TenantID, in.ExternalID, in.OrderRef, lines).Scan(&id)
	s.answer(w, r, cfg, err, id, in.ExternalID, "mock_wms.reservations", nil)
}

func (s *Server) ship(w http.ResponseWriter, r *http.Request) {
	in, cfg, ok := s.begin(w, r)
	if !ok {
		return
	}
	if cfg.RejectRefPrefix != "" && strings.HasPrefix(in.OrderRef, cfg.RejectRefPrefix) {
		reply(w, http.StatusUnprocessableEntity, map[string]string{"error": "shipment refused"})
		return
	}
	// The labels come from the external id, so that the same shipment has the same labels. A
	// shipment without an external id takes a random seed.
	seed := in.ExternalID
	if seed == "" {
		seed = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(time.Now().UnixNano(), rand.Uint64()))))
	}
	shipped := make([]line, len(in.Lines))
	labels := make([]string, len(in.Lines))
	for i, l := range in.Lines {
		qty, err := scaled(l.Qty, cfg.ShipQtyFactor)
		if err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": "bad quantity factor"})
			return
		}
		shipped[i] = line{SKU: l.SKU, Qty: qty, Unit: l.Unit}
		labels[i] = SSCC(seed, i)
	}
	lines, _ := json.Marshal(shipped)
	var id int64
	err := s.pool.QueryRow(r.Context(),
		`INSERT INTO mock_wms.shipments (tenant_id, external_id, order_ref, lines, sscc)
		 VALUES ($1, NULLIF($2, ''), $3, $4, $5) ON CONFLICT (external_id) DO NOTHING RETURNING id`,
		in.TenantID, in.ExternalID, in.OrderRef, lines, labels).Scan(&id)
	s.answer(w, r, cfg, err, id, in.ExternalID, "mock_wms.shipments", map[string]any{"lines": shipped, "sscc": labels})
}

// answer finishes a create: a new row is 201 (or a lost answer, by the knob), a row that was
// there is 200 with the stored one. facts are added to the answer of a new row (what a
// shipment recorded), so that a client without a key can read the labels it got.
func (s *Server) answer(w http.ResponseWriter, r *http.Request, cfg Config, err error, id int64, ext, table string, facts map[string]any) {
	switch {
	case err == nil:
		if rand.Float64() < cfg.FailAfterCommitRate {
			reply(w, http.StatusInternalServerError, map[string]string{"error": "lost the answer"}) // committed, answer lost
			return
		}
		body := map[string]any{"id": id, "external_id": nil}
		if ext != "" {
			body["external_id"] = ext
		}
		maps.Copy(body, facts)
		reply(w, http.StatusCreated, body)
	case errors.Is(err, pgx.ErrNoRows):
		var stored int64
		if err := s.pool.QueryRow(r.Context(), `SELECT id FROM `+table+` WHERE external_id = $1`, ext).Scan(&stored); err != nil {
			reply(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
			return
		}
		reply(w, http.StatusOK, map[string]any{"id": stored, "external_id": ext})
	default:
		reply(w, http.StatusInternalServerError, map[string]string{"error": "write failed"})
	}
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	if cfg.Latency > 0 {
		select {
		case <-time.After(cfg.Latency):
		case <-r.Context().Done():
			return
		}
	}
	ext := r.PathValue("external_id")
	var n int
	err := s.pool.QueryRow(r.Context(),
		`WITH c AS (UPDATE mock_wms.reservations SET status = 'released' WHERE external_id = $1 RETURNING 1)
		 SELECT count(*) FROM c`, ext).Scan(&n)
	if err != nil {
		reply(w, http.StatusInternalServerError, map[string]string{"error": "write failed"})
		return
	}
	reply(w, http.StatusOK, map[string]any{"external_id": ext, "status": "released", "found": n > 0})
}

func (s *Server) lookupReservation(w http.ResponseWriter, r *http.Request) {
	s.lookup(w, r, `SELECT jsonb_build_object('id', id, 'external_id', external_id, 'order_ref', order_ref, 'lines', lines, 'status', status)
	                FROM mock_wms.reservations WHERE external_id = $1`)
}

func (s *Server) lookupShipment(w http.ResponseWriter, r *http.Request) {
	s.lookup(w, r, `SELECT jsonb_build_object('id', id, 'external_id', external_id, 'order_ref', order_ref, 'lines', lines, 'sscc', to_jsonb(sscc))
	                FROM mock_wms.shipments WHERE external_id = $1`)
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request, query string) {
	ext := r.URL.Query().Get("external_id")
	if ext == "" {
		reply(w, http.StatusBadRequest, map[string]string{"error": "external_id is required"})
		return
	}
	var body []byte
	err := s.pool.QueryRow(r.Context(), query, ext).Scan(&body)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		reply(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case err != nil:
		reply(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

// scaled multiplies a quantity by the factor and rounds half up to three places. An empty
// factor means 1.
func scaled(qty, factor string) (string, error) {
	q, ok := decimal.ParseScaled(qty, 3)
	if !ok {
		return "", errors.New("bad quantity")
	}
	f := new(big.Rat).SetInt64(1)
	if factor != "" {
		if _, ok := f.SetString(factor); !ok || f.Sign() < 0 {
			return "", errors.New("bad factor")
		}
	}
	r := new(big.Rat).Mul(new(big.Rat).SetInt(q), f) // in thousandths
	num, den := r.Num(), r.Denom()
	out := new(big.Int).Div(new(big.Int).Add(new(big.Int).Mul(num, big.NewInt(2)), den), new(big.Int).Mul(den, big.NewInt(2)))
	return decimal.FormatScaled(out, 3), nil
}

// SSCC is the 18 digit label of line i of a shipment: 17 digits taken from SHA-256 of the
// external id and the line number, and the GS1 check digit (mod 10, weights 3 and 1 from the
// right). The same shipment always has the same labels.
func SSCC(externalID string, i int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", externalID, i)))
	digits := new(big.Int).SetBytes(sum[:]).Text(10)
	body := fmt.Sprintf("%017s", digits[len(digits)-min(17, len(digits)):])
	total := 0
	for j := 0; j < 17; j++ {
		d := int(body[16-j] - '0')
		if j%2 == 0 {
			d *= 3
		}
		total += d
	}
	return body + string(rune('0'+(10-total%10)%10))
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
