// Package control holds the knobs of the mocks per tenant, for the slots of the benchmark.
//
// Slots share one mock-1c and one mock-wms and tell their rows apart by tenant id. A knob set for a
// tenant (POST /control with a tenant_id) applies only to the requests of that tenant and wins over
// the process-wide value from the environment; DELETE /control/{tenant_id} puts the tenant back.
// Only the stand's own addresses (loopback and private networks) may call the routes.
//
// Input: JSON {"tenant_id": "...", "fail_after_commit_rate": 0..1, "latency_ms": n, "reject_ref_prefix": "...",
// "ship_qty_factor": "1.5"}; every knob is optional, a missing one is left as it was.
// Output: the knobs of the tenant as JSON, with the same names.
package control

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// Knobs are the optional overrides of one tenant. A nil field means "not overridden".
type Knobs struct {
	FailAfterCommitRate *float64 `json:"fail_after_commit_rate,omitempty"`
	LatencyMS           *int     `json:"latency_ms,omitempty"`
	RejectRefPrefix     *string  `json:"reject_ref_prefix,omitempty"`
	ShipQtyFactor       *string  `json:"ship_qty_factor,omitempty"`
}

var reFactor = regexp.MustCompile(`^[0-9]{1,6}(\.[0-9]{1,6})?$`)

// Store keeps the knobs of the tenants. The zero value is not usable; call New.
type Store struct {
	mu          sync.Mutex
	byTenant    map[string]Knobs
	allowFactor bool // mock-wms only: ship_qty_factor
}

// New builds a store. allowShipFactor enables ship_qty_factor (mock-wms).
func New(allowShipFactor bool) *Store {
	return &Store{byTenant: map[string]Knobs{}, allowFactor: allowShipFactor}
}

// Get returns the overrides of a tenant (all nil if none).
func (s *Store) Get(tenant string) Knobs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byTenant[tenant]
}

// Rate returns the failure share for a tenant, or def when not overridden.
func (k Knobs) Rate(def float64) float64 {
	if k.FailAfterCommitRate != nil {
		return *k.FailAfterCommitRate
	}
	return def
}

// Latency returns the delay for a tenant, or def when not overridden.
func (k Knobs) Latency(def time.Duration) time.Duration {
	if k.LatencyMS != nil {
		return time.Duration(*k.LatencyMS) * time.Millisecond
	}
	return def
}

// Prefix returns the refused reference prefix for a tenant, or def when not overridden.
func (k Knobs) Prefix(def string) string {
	if k.RejectRefPrefix != nil {
		return *k.RejectRefPrefix
	}
	return def
}

// Factor returns the shipped-quantity factor for a tenant, or def when not overridden.
func (k Knobs) Factor(def string) string {
	if k.ShipQtyFactor != nil {
		return *k.ShipQtyFactor
	}
	return def
}

func (s *Store) valid(k Knobs) bool {
	if k.FailAfterCommitRate != nil && (*k.FailAfterCommitRate < 0 || *k.FailAfterCommitRate > 1 || math.IsNaN(*k.FailAfterCommitRate)) {
		return false
	}
	if k.LatencyMS != nil && (*k.LatencyMS < 0 || *k.LatencyMS > 60000) {
		return false
	}
	if k.RejectRefPrefix != nil && len(*k.RejectRefPrefix) > 64 {
		return false
	}
	if k.ShipQtyFactor != nil && (!s.allowFactor || !reFactor.MatchString(*k.ShipQtyFactor)) {
		return false
	}
	return true
}

// Register adds the routes /control and /control/{tenant_id} to a mux.
func (s *Store) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /control", s.set)
	mux.HandleFunc("GET /control", s.show)
	mux.HandleFunc("DELETE /control/{tenant_id}", s.reset)
}

func (s *Store) set(w http.ResponseWriter, r *http.Request) {
	if !fromStand(r.RemoteAddr) {
		reply(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	var in struct {
		TenantID string `json:"tenant_id"`
		Knobs
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.TenantID == "" || len(in.TenantID) > 128 || !s.valid(in.Knobs) {
		reply(w, http.StatusBadRequest, map[string]string{"error": "bad knobs"})
		return
	}
	s.mu.Lock()
	cur := s.byTenant[in.TenantID]
	if in.FailAfterCommitRate != nil {
		cur.FailAfterCommitRate = in.FailAfterCommitRate
	}
	if in.LatencyMS != nil {
		cur.LatencyMS = in.LatencyMS
	}
	if in.RejectRefPrefix != nil {
		cur.RejectRefPrefix = in.RejectRefPrefix
	}
	if in.ShipQtyFactor != nil {
		cur.ShipQtyFactor = in.ShipQtyFactor
	}
	s.byTenant[in.TenantID] = cur
	s.mu.Unlock()
	reply(w, http.StatusOK, cur)
}

func (s *Store) show(w http.ResponseWriter, r *http.Request) {
	if !fromStand(r.RemoteAddr) {
		reply(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	reply(w, http.StatusOK, s.Get(r.URL.Query().Get("tenant_id")))
}

func (s *Store) reset(w http.ResponseWriter, r *http.Request) {
	if !fromStand(r.RemoteAddr) {
		reply(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	s.mu.Lock()
	delete(s.byTenant, r.PathValue("tenant_id"))
	s.mu.Unlock()
	reply(w, http.StatusOK, map[string]string{"status": "reset"})
}

// fromStand reports whether the address is loopback or in a private network (the compose network).
func fromStand(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
