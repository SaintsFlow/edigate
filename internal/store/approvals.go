package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file holds the queries of the policy and of the approval requests: the
// partner and supplier-order data the policy is given, the request opened by
// Reserve, the list and the decisions of operators, and the expiry.

// Errors of a decision on a request. A request of another tenant and an unknown
// one both give ErrNotFound.
var (
	ErrNotPending     = errors.New("approval is not pending")
	ErrAlreadyDecided = errors.New("already decided by this operator")
	ErrOwnRequest     = errors.New("the requester cannot decide the request")
)

// PartnerPolicy is the partner record the policy checks a call against. A limit
// that is nil is not set. Amounts are whole cents.
type PartnerPolicy struct {
	Role          string
	Active        bool
	AutoMaxCents  *int64
	DailyMaxCents *int64
	ForecastCents *int64
	ShipToCodes   []string
	Contacts      []string
}

// PartnerPolicy reads the partner of the tenant with its ship-to codes and
// contacts. An unknown partner gives ErrNotFound.
func (s *Store) PartnerPolicy(ctx context.Context, tenantID, partnerID string) (PartnerPolicy, error) {
	var p PartnerPolicy
	err := s.pool.QueryRow(ctx,
		`SELECT role, active, (auto_max_amount * 100)::bigint, (daily_auto_max * 100)::bigint, (forecast_amount * 100)::bigint,
		        ARRAY(SELECT code FROM partner_ship_to WHERE tenant_id = p.tenant_id AND partner_id = p.partner_id ORDER BY code),
		        ARRAY(SELECT email FROM partner_contacts WHERE tenant_id = p.tenant_id AND partner_id = p.partner_id ORDER BY email)
		 FROM partners p WHERE tenant_id = $1 AND partner_id = $2`, tenantID, partnerID).
		Scan(&p.Role, &p.Active, &p.AutoMaxCents, &p.DailyMaxCents, &p.ForecastCents, &p.ShipToCodes, &p.Contacts)
	if isNoRows(err) {
		return PartnerPolicy{}, ErrNotFound
	}
	return p, err
}

// SupplierOrder is an order to a supplier. Lines is JSON: [{"sku","qty","unit_price"}].
type SupplierOrder struct {
	TotalCents int64
	Currency   string
	Lines      []byte
}

// SupplierOrder reads the order of the tenant to the supplier. An unknown one gives ErrNotFound.
func (s *Store) SupplierOrder(ctx context.Context, tenantID, partnerID, orderNumber string) (SupplierOrder, error) {
	var o SupplierOrder
	err := s.pool.QueryRow(ctx,
		`SELECT (total * 100)::bigint, currency, lines FROM supplier_orders
		 WHERE tenant_id = $1 AND partner_id = $2 AND order_number = $3`, tenantID, partnerID, orderNumber).
		Scan(&o.TotalCents, &o.Currency, &o.Lines)
	if isNoRows(err) {
		return SupplierOrder{}, ErrNotFound
	}
	return o, err
}

// requestApproval opens a request for the new key instead of an outbox row: the key
// stays reserved, its answer becomes pending_approval with the id of the request.
func (s *Store) requestApproval(ctx context.Context, tx pgx.Tx, in ReserveInput, txnID string, v Verdict) (Reserved, error) {
	approvers := 1
	if v.Approval == "two" {
		approvers = 2
	}
	rules, err := json.Marshal(v.Rules)
	if err != nil {
		return Reserved{}, err
	}
	var id string
	err = tx.QueryRow(ctx,
		`INSERT INTO approvals (tenant_id, idem_key, kind, tool, effect_type, args, view, rules, tier, approvers, status, requested_by, expires_at)
		 VALUES ($1, $2, 'effect', $3, $4, $5, $6, $7, $8, $9, 'pending', $10, now() + $11::bigint * interval '1 millisecond')
		 RETURNING id::text`,
		in.TenantID, in.Key, in.Tool, in.EffectType, in.Payload, v.View, rules, v.Tier, approvers, in.PrincipalID, s.ttl().Milliseconds()).Scan(&id)
	if err != nil {
		return Reserved{}, fmt.Errorf("open the approval request: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE idempotency_keys
		    SET result = jsonb_build_object('status', 'pending_approval', 'txn_id', txn_id, 'approval_id', $2::text)
		  WHERE key = $1`, in.Key, id); err != nil {
		return Reserved{}, err
	}
	ev := auditOf(in, "approval_requested", "pending_approval", nil)
	ev.PolicyExplain = v.Explain
	if err := appendAudit(ctx, tx, ev); err != nil {
		return Reserved{}, err
	}
	if err := markProcessed(ctx, tx, in); err != nil {
		return Reserved{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Reserved{}, err
	}
	return Reserved{Outcome: ReservedNew, TxnID: txnID, Status: "pending_approval", ApprovalID: id}, nil
}

// openReview opens a review request for a conflicting duplicate (no effect behind
// it) unless the key already has an open one.
func (s *Store) openReview(ctx context.Context, tx pgx.Tx, in ReserveInput) error {
	if in.ReviewView == nil {
		return nil
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO approvals (tenant_id, idem_key, kind, tool, args, view, rules, tier, approvers, status, requested_by, expires_at)
		 VALUES ($1, $2, 'review', $3, '{}', $4, '["conflicting_duplicate"]', 0, 1, 'pending', $5, now() + $6::bigint * interval '1 millisecond')
		 ON CONFLICT DO NOTHING`,
		in.TenantID, in.Key, in.Tool, in.ReviewView, in.PrincipalID, s.ttl().Milliseconds())
	return err
}

// Approval is a request as the operator sees it. View and Rules hold typed values
// and rule names; the payload of the effect (args) is never read out.
type Approval struct {
	ID          string
	Kind        string
	Tool        string
	Tier        int
	Approvers   int
	Status      string
	RequestedBy string
	DecidedBy   []string
	RequestedAt time.Time
	ExpiresAt   time.Time
	Rules       json.RawMessage
	View        json.RawMessage
}

// ListApprovals returns the requests of the tenant with this status, oldest first
// (at most 100).
func (s *Store) ListApprovals(ctx context.Context, tenantID, status string) ([]Approval, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, kind, tool, tier, approvers, status, requested_by, decided_by, requested_at, expires_at, rules, view
		 FROM approvals WHERE tenant_id = $1 AND status = $2 ORDER BY requested_at, id LIMIT 100`, tenantID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Approval{}
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.Kind, &a.Tool, &a.Tier, &a.Approvers, &a.Status, &a.RequestedBy, &a.DecidedBy,
			&a.RequestedAt, &a.ExpiresAt, &a.Rules, &a.View); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Decision is the result of a decision on a request: its status afterwards and how
// many different operators have approved so far.
type Decision struct {
	ID        string
	Status    string
	Approvals int
	Approvers int
}

// DecideApproval records the decision of an operator on a request of the tenant.
//
// Reject closes the request and fails the key ({"status":"rejected","reason":
// "approval_rejected"}). Approve adds the operator to decided_by; when as many
// different operators as the request needs have approved, the request is approved
// and, in the same transaction, the outbox row appears and the answer of the key
// becomes accepted (I3). The same operator twice, and the requester, are refused.
// A review request is only closed: the key is not touched.
func (s *Store) DecideApproval(ctx context.Context, tenantID, operatorID, traceID, id string, approve bool) (Decision, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Decision{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var a struct {
		kind, tool, effectType, status, requestedBy string
		key, args                                   []byte
		approvers                                   int
		decided                                     []string
		due                                         bool
	}
	var effectType *string
	err = tx.QueryRow(ctx,
		`SELECT kind, tool, effect_type, status, requested_by, idem_key, args, approvers, decided_by, expires_at <= now()
		 FROM approvals WHERE id = $1::uuid AND tenant_id = $2 FOR UPDATE`, id, tenantID).
		Scan(&a.kind, &a.tool, &effectType, &a.status, &a.requestedBy, &a.key, &a.args, &a.approvers, &a.decided, &a.due)
	if isNoRows(err) {
		return Decision{}, ErrNotFound
	}
	if err != nil {
		return Decision{}, err
	}
	if a.status != "pending" {
		return Decision{}, ErrNotPending
	}
	if a.due {
		_ = tx.Rollback(ctx)
		if _, err := s.ExpireApprovals(ctx); err != nil {
			return Decision{}, err
		}
		return Decision{}, ErrNotPending
	}
	if operatorID == a.requestedBy {
		return Decision{}, ErrOwnRequest
	}
	if slices.Contains(a.decided, operatorID) {
		return Decision{}, ErrAlreadyDecided
	}
	if effectType != nil {
		a.effectType = *effectType
	}

	decided := append(a.decided, operatorID)
	status, event := "pending", "approval_decided"
	switch {
	case !approve:
		status, event = "rejected", "approval_rejected"
	case len(decided) >= a.approvers:
		status, event = "approved", "approval_approved"
	}
	if _, err := tx.Exec(ctx,
		`UPDATE approvals SET decided_by = $2, status = $3::text, decided_at = CASE WHEN $3::text = 'pending' THEN NULL ELSE now() END
		 WHERE id = $1::uuid`, id, decided, status); err != nil {
		return Decision{}, err
	}

	if a.kind == "effect" {
		switch status {
		case "approved":
			if err := s.enqueue(ctx, tx, tenantID, a.key, a.effectType, a.args); err != nil {
				return Decision{}, fmt.Errorf("enqueue the approved effect: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE idempotency_keys SET result = jsonb_build_object('status', 'accepted', 'txn_id', txn_id) WHERE key = $1`, a.key); err != nil {
				return Decision{}, err
			}
		case "rejected":
			if err := failKey(ctx, tx, a.key, `{"status":"rejected","reason":"approval_rejected"}`); err != nil {
				return Decision{}, err
			}
		}
	}

	detail, err := json.Marshal(map[string]any{"approval_id": id, "approvals": len(decided), "approvers": a.approvers})
	if err != nil {
		return Decision{}, err
	}
	ev := AuditEvent{
		TenantID: tenantID, EventType: event, TraceID: traceID, PrincipalID: operatorID,
		Tool: a.tool, IdemKey: a.key, Outcome: status, Detail: detail,
	}
	if err := appendAudit(ctx, tx, ev); err != nil {
		return Decision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Decision{}, err
	}
	return Decision{ID: id, Status: status, Approvals: len(decided), Approvers: a.approvers}, nil
}

// failKey ends a key as failed with the recorded answer (a JSON object without txn_id,
// which is added here).
func failKey(ctx context.Context, tx pgx.Tx, key []byte, result string) error {
	_, err := tx.Exec(ctx,
		`UPDATE idempotency_keys
		    SET state = 'failed', completed_at = now(), result = $2::jsonb || jsonb_build_object('txn_id', txn_id)
		  WHERE key = $1`, key, result)
	return err
}

// ExpireApprovals closes the pending requests whose time is over: the request
// becomes expired and the key of an effect request fails with {"status":"expired"}.
// It returns how many requests it closed. Several processes may call it at once.
func (s *Store) ExpireApprovals(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`UPDATE approvals SET status = 'expired', decided_at = now()
		 WHERE id IN (SELECT id FROM approvals WHERE status = 'pending' AND expires_at <= now() FOR UPDATE SKIP LOCKED)
		 RETURNING id::text, tenant_id, idem_key, kind, tool`)
	if err != nil {
		return 0, err
	}
	type expired struct {
		id, tenant, kind, tool string
		key                    []byte
	}
	var done []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.tenant, &e.key, &e.kind, &e.tool); err != nil {
			rows.Close()
			return 0, err
		}
		done = append(done, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, e := range done {
		if e.kind == "effect" {
			if err := failKey(ctx, tx, e.key, `{"status":"expired"}`); err != nil {
				return 0, err
			}
		}
		detail, err := json.Marshal(map[string]string{"approval_id": e.id})
		if err != nil {
			return 0, err
		}
		ev := AuditEvent{TenantID: e.tenant, EventType: "approval_expired", TraceID: "expiry", PrincipalID: "expiry",
			Tool: e.tool, IdemKey: e.key, Outcome: "expired", Detail: detail}
		if err := appendAudit(ctx, tx, ev); err != nil {
			return 0, err
		}
	}
	return len(done), tx.Commit(ctx)
}
