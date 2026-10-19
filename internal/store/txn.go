package store

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file holds the queries of the guarantee "at most one effect per business
// key": the order source, the price list, the reservation of a key together with
// its outbox row (Reserve), txn.status, reopening of a failed key, and the claim
// of an outbox row for the relay.

// ErrNotFailed means a key is not in the state failed, so it cannot be reopened.
var ErrNotFailed = errors.New("key is not failed")

// OrderSource is the part of a stored 850 (an order) or 810 (an invoice) that an
// effect is made from.
type OrderSource struct {
	ID        string
	PartnerID string
	BKRef     string // raw BEG03 or BIG02 without spaces at the edges; "" when the document has none
	Status    string
	Body      []byte // JSON
}

// OrderSource reads a document of the set st01 ("850" or "810") of the tenant in
// the status received or processed. Any other document, another tenant's document
// and an unknown id give ErrNotFound.
func (s *Store) OrderSource(ctx context.Context, tenantID, documentID, st01 string) (OrderSource, error) {
	var d OrderSource
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, partner_id, COALESCE(bk_ref, ''), status, body
		 FROM edi_documents
		 WHERE tenant_id = $1 AND id = $2::uuid AND st01 = $3
		   AND status IN ('received', 'processed') AND body IS NOT NULL`, tenantID, documentID, st01).
		Scan(&d.ID, &d.PartnerID, &d.BKRef, &d.Status, &d.Body)
	if isNoRows(err) {
		return OrderSource{}, ErrNotFound
	}
	return d, err
}

// Price is a row of the price list; UnitPrice is a decimal string with two places.
type Price struct {
	UnitPrice string
	Currency  string
	Unit      string
}

// Prices returns the price list rows of the tenant for the given SKUs, by SKU.
// A SKU that is not in the list is missing from the map.
func (s *Store) Prices(ctx context.Context, tenantID string, skus []string) (map[string]Price, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT sku, unit_price::text, currency, unit FROM price_list
		 WHERE tenant_id = $1 AND sku = ANY($2)`, tenantID, skus)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Price{}
	for rows.Next() {
		var sku string
		var p Price
		if err := rows.Scan(&sku, &p.UnitPrice, &p.Currency, &p.Unit); err != nil {
			return nil, err
		}
		out[sku] = p
	}
	return out, rows.Err()
}

// ReserveInput is one call of an effect tool, ready to reserve.
type ReserveInput struct {
	TenantID    string
	Tool        string
	PrincipalID string
	TraceID     string
	DocumentID  string // the source document; it becomes "processed"
	Key         []byte // 32 bytes, from txn.Key
	BusinessKey []byte // JSON array
	ReqHash     []byte
	EffectType  string
	Payload     []byte // JSON, the body of the effect

	// PartnerID and AmountCents are set for a tool whose amount counts against
	// daily_auto_max (T1): the sum accepted automatically today is read under lock
	// and handed to Decide, and AmountCents is added to it when the call goes through
	// without approval. Empty PartnerID: no counter.
	PartnerID   string
	AmountCents int64

	// Decide runs inside the transaction for a new or reopened key, after the daily
	// counter is locked. A nil Decide means no policy (tests of the key itself).
	Decide func(dailyAutoCents int64) (Verdict, error)

	// ReviewView is the typed view of the call, for the review request that a
	// conflicting duplicate opens (nil: no review).
	ReviewView []byte

	// BeforeOutbox is for tests: it runs after the key is written and before the
	// outbox row; an error from it rolls the whole reservation back.
	BeforeOutbox func() error
}

// Verdict is the decision of the policy as Reserve needs it. Deny names the rules
// that forbid the call (nothing is reserved). Approval is "none" (the effect goes
// to the outbox), "one" or "two" (an approval request is opened instead). Rules
// are the names of the rules that caused the request, View what the operator sees,
// Explain the JSON for audit_log.policy_explain.
type Verdict struct {
	Deny     []string
	Approval string
	Tier     int
	Rules    []string
	View     []byte
	Explain  []byte
}

// Outcomes of Reserve.
const (
	ReservedNew      = "new"      // a new key (or a reopened one) with a new outbox row or an approval request
	ReservedSame     = "same"     // the key exists with the same request hash
	ReservedConflict = "conflict" // the key exists with another request hash
	ReservedDenied   = "denied"   // the policy forbids the call; nothing was reserved
)

// Reserved is the answer of Reserve. For ReservedNew and ReservedSame, Status is
// the recorded status of the key: "accepted", "pending_approval" (with ApprovalID),
// or for a failed key "rejected" or "expired" with the recorded Reason. For
// ReservedDenied, Verdict holds the rules; for ReservedConflict only Outcome is set.
type Reserved struct {
	Outcome    string
	TxnID      string
	Status     string
	Reason     string
	ApprovalID string
	Verdict    Verdict
}

// Reserve writes the key, its answer, the outbox row and the audit row
// "effect_reserved" in one transaction (invariants I1 and I3).
//
// INSERT ... ON CONFLICT DO NOTHING decides a race: a second call waits for the
// first on the unique index and then reads the same row (SELECT ... FOR UPDATE).
// Same request hash: the same txn_id and the current state. Another hash:
// conflicting duplicate, an audit row with both hashes and no effect. A reopened
// key is reused with a new txn_id, a new hash and a new outbox row.
func (s *Store) Reserve(ctx context.Context, in ReserveInput) (Reserved, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Reserved{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var txnID string
	err = tx.QueryRow(ctx,
		`WITH n AS (SELECT gen_random_uuid() AS id)
		 INSERT INTO idempotency_keys (key, tenant_id, tool, business_key, req_hash, state, txn_id, result)
		 SELECT $1::bytea, $2::text, $3::text, $4::jsonb, $5::bytea, 'reserved', n.id, jsonb_build_object('status', 'accepted', 'txn_id', n.id) FROM n
		 ON CONFLICT (key) DO NOTHING
		 RETURNING txn_id::text`,
		in.Key, in.TenantID, in.Tool, in.BusinessKey, in.ReqHash).Scan(&txnID)
	switch {
	case err == nil:
		return s.finishReservation(ctx, tx, in, txnID, "reserved")
	case !isNoRows(err):
		return Reserved{}, err
	}

	var stored []byte
	var state, storedTxn string
	var result []byte
	err = tx.QueryRow(ctx,
		`SELECT req_hash, state, txn_id::text, result FROM idempotency_keys WHERE key = $1 FOR UPDATE`, in.Key).
		Scan(&stored, &state, &storedTxn, &result)
	if err != nil {
		return Reserved{}, fmt.Errorf("read the key: %w", err)
	}

	if state == "reopened" {
		err = tx.QueryRow(ctx,
			`WITH n AS (SELECT gen_random_uuid() AS id)
			 UPDATE idempotency_keys k
			    SET txn_id = n.id, req_hash = $2, state = 'reserved', completed_at = NULL,
			        result = jsonb_build_object('status', 'accepted', 'txn_id', n.id)
			   FROM n WHERE k.key = $1
			 RETURNING k.txn_id::text`, in.Key, in.ReqHash).Scan(&txnID)
		if err != nil {
			return Reserved{}, fmt.Errorf("reuse the reopened key: %w", err)
		}
		return s.finishReservation(ctx, tx, in, txnID, "reopened")
	}

	if !bytes.Equal(stored, in.ReqHash) {
		detail, err := json.Marshal(map[string]string{"stored_req_hash": hex.EncodeToString(stored), "new_req_hash": hex.EncodeToString(in.ReqHash)})
		if err != nil {
			return Reserved{}, err
		}
		if err := appendAudit(ctx, tx, auditOf(in, "conflicting_duplicate", "conflicting_duplicate", detail)); err != nil {
			return Reserved{}, err
		}
		if err := s.openReview(ctx, tx, in); err != nil {
			return Reserved{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Reserved{}, err
		}
		return Reserved{Outcome: ReservedConflict}, nil
	}

	if err := markProcessed(ctx, tx, in); err != nil {
		return Reserved{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Reserved{}, err
	}
	var r struct {
		Status     string `json:"status"`
		Reason     string `json:"reason"`
		ApprovalID string `json:"approval_id"`
	}
	if err := json.Unmarshal(result, &r); err != nil {
		return Reserved{}, fmt.Errorf("read the recorded result: %w", err)
	}
	return Reserved{Outcome: ReservedSame, TxnID: storedTxn, Status: r.Status, Reason: r.Reason, ApprovalID: r.ApprovalID}, nil
}

// finishReservation decides what a new (or reopened) key becomes, inside the
// transaction that holds the key: refused by the policy (nothing is kept), an
// outbox row (the effect goes on), or an approval request (it waits for operators).
// The audit row of the decision carries policy_explain. The counter of the day is
// locked first, so that parallel calls of one partner are decided one after another.
func (s *Store) finishReservation(ctx context.Context, tx pgx.Tx, in ReserveInput, txnID, outcome string) (Reserved, error) {
	v := Verdict{Approval: "none"}
	if in.Decide != nil {
		var daily int64
		if in.PartnerID != "" {
			var err error
			if daily, err = lockDailyTotal(ctx, tx, in.TenantID, in.PartnerID); err != nil {
				return Reserved{}, err
			}
		}
		var err error
		if v, err = in.Decide(daily); err != nil {
			return Reserved{}, err
		}
		if len(v.Deny) > 0 {
			return Reserved{Outcome: ReservedDenied, Verdict: v}, nil // the deferred rollback drops the key
		}
	}
	if v.Approval != "none" {
		return s.requestApproval(ctx, tx, in, txnID, v)
	}

	if in.BeforeOutbox != nil {
		if err := in.BeforeOutbox(); err != nil {
			return Reserved{}, err
		}
	}
	if in.PartnerID != "" && in.AmountCents > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE daily_auto_totals SET total = total + $3::bigint::numeric / 100
			 WHERE tenant_id = $1 AND partner_id = $2 AND day = (now() AT TIME ZONE 'UTC')::date`,
			in.TenantID, in.PartnerID, in.AmountCents); err != nil {
			return Reserved{}, fmt.Errorf("count the order in the daily total: %w", err)
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox (tenant_id, idem_key, effect_type, payload, status) VALUES ($1, $2, $3, $4, 'pending')`,
		in.TenantID, in.Key, in.EffectType, in.Payload); err != nil {
		return Reserved{}, fmt.Errorf("enqueue the effect: %w", err)
	}
	ev := auditOf(in, "effect_reserved", outcome, nil)
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
	return Reserved{Outcome: ReservedNew, TxnID: txnID, Status: "accepted"}, nil
}

// lockDailyTotal makes sure the row of today (UTC) exists, locks it and returns the
// sum accepted automatically so far, in cents.
func lockDailyTotal(ctx context.Context, tx pgx.Tx, tenantID, partnerID string) (int64, error) {
	if _, err := tx.Exec(ctx,
		`INSERT INTO daily_auto_totals (tenant_id, partner_id, day) VALUES ($1, $2, (now() AT TIME ZONE 'UTC')::date)
		 ON CONFLICT DO NOTHING`, tenantID, partnerID); err != nil {
		return 0, fmt.Errorf("daily total: %w", err)
	}
	var cents int64
	err := tx.QueryRow(ctx,
		`SELECT (total * 100)::bigint FROM daily_auto_totals
		 WHERE tenant_id = $1 AND partner_id = $2 AND day = (now() AT TIME ZONE 'UTC')::date FOR UPDATE`,
		tenantID, partnerID).Scan(&cents)
	return cents, err
}

func auditOf(in ReserveInput, event, outcome string, detail []byte) AuditEvent {
	return AuditEvent{
		TenantID: in.TenantID, EventType: event, TraceID: in.TraceID, PrincipalID: in.PrincipalID,
		Tool: in.Tool, IdemKey: in.Key, PayloadSHA256: in.ReqHash, Outcome: outcome, Detail: detail,
	}
}

func markProcessed(ctx context.Context, tx pgx.Tx, in ReserveInput) error {
	_, err := tx.Exec(ctx,
		`UPDATE edi_documents SET status = 'processed'
		 WHERE id = $1::uuid AND tenant_id = $2 AND status = 'received'`, in.DocumentID, in.TenantID)
	return err
}

// TxnState is what txn.status needs: the state of the key and the external
// reference of the latest delivered effect ("" when there is none).
type TxnState struct {
	State        string // reserved, completed, failed or reopened
	ResultStatus string // the recorded status: accepted, pending_approval, rejected or expired
	ExternalRef  string
}

// TxnState reads the key with this txn_id. A key of another tenant and an
// unknown txn_id both give ErrNotFound.
func (s *Store) TxnState(ctx context.Context, tenantID, txnID string) (TxnState, error) {
	var t TxnState
	err := s.pool.QueryRow(ctx,
		`SELECT k.state, COALESCE(k.result->>'status', ''),
		        COALESCE((SELECT o.external_ref FROM outbox o
		                  WHERE o.idem_key = k.key AND o.external_ref IS NOT NULL ORDER BY o.id DESC LIMIT 1), '')
		 FROM idempotency_keys k WHERE k.tenant_id = $1 AND k.txn_id = $2::uuid`, tenantID, txnID).
		Scan(&t.State, &t.ResultStatus, &t.ExternalRef)
	if isNoRows(err) {
		return TxnState{}, ErrNotFound
	}
	return t, err
}

// ReopenKey moves a failed key of the tenant to "reopened" and writes the audit
// row "txn_reopen" with the operator. A key of another tenant and an unknown key
// give ErrNotFound; a key in any other state than failed gives ErrNotFailed.
func (s *Store) ReopenKey(ctx context.Context, tenantID, principalID, traceID string, key []byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var state string
	err = tx.QueryRow(ctx,
		`SELECT state FROM idempotency_keys WHERE key = $1 AND tenant_id = $2 FOR UPDATE`, key, tenantID).Scan(&state)
	if isNoRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != "failed" {
		return ErrNotFailed
	}
	if _, err := tx.Exec(ctx, `UPDATE idempotency_keys SET state = 'reopened' WHERE key = $1`, key); err != nil {
		return err
	}
	ev := AuditEvent{TenantID: tenantID, EventType: "txn_reopen", TraceID: traceID, PrincipalID: principalID, IdemKey: key, Outcome: "ok"}
	if err := appendAudit(ctx, tx, ev); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// OutboxRow is a row the relay has locked.
type OutboxRow struct {
	ID         int64
	TenantID   string
	Key        []byte
	EffectType string
	Payload    []byte // JSON
	Attempts   int    // failed attempts so far
}

// Claim holds one locked outbox row inside an open transaction. The relay ends it
// with exactly one of Ack, Retry, Fail or Release, so that the change of the row,
// of the key and the audit row commit together.
type Claim struct {
	Row OutboxRow
	tx  pgx.Tx
}

// ClaimOutbox locks the oldest pending row that is due (FOR UPDATE SKIP LOCKED,
// so workers and processes never take the same row). It returns nil when there is
// none. A process that dies while holding a claim loses the lock with its
// connection and the row is taken again.
func (s *Store) ClaimOutbox(ctx context.Context) (*Claim, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	c := &Claim{tx: tx}
	err = tx.QueryRow(ctx,
		`SELECT id, tenant_id, idem_key, effect_type, payload, attempts FROM outbox
		 WHERE status = 'pending' AND next_attempt_at <= now()
		 ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`).
		Scan(&c.Row.ID, &c.Row.TenantID, &c.Row.Key, &c.Row.EffectType, &c.Row.Payload, &c.Row.Attempts)
	if isNoRows(err) {
		_ = tx.Rollback(ctx)
		return nil, nil
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return c, nil
}

// Release gives the row back untouched (rollback). Safe to call after a commit.
func (c *Claim) Release(ctx context.Context) { _ = c.tx.Rollback(ctx) }

// Ack records a delivered effect: the row is acked with its external reference,
// the key is completed, and "effect_applied" goes to the audit log.
func (c *Claim) Ack(ctx context.Context, externalRef string) error {
	if _, err := c.tx.Exec(ctx,
		`UPDATE outbox SET status = 'acked', external_ref = $2, last_error = NULL WHERE id = $1`, c.Row.ID, externalRef); err != nil {
		return err
	}
	if _, err := c.tx.Exec(ctx,
		`UPDATE idempotency_keys SET state = 'completed', completed_at = now() WHERE key = $1`, c.Row.Key); err != nil {
		return err
	}
	return c.finish(ctx, "effect_applied", "ok", nil)
}

// Retry keeps the row pending for another attempt after delay.
func (c *Claim) Retry(ctx context.Context, lastError string, delay time.Duration) error {
	if _, err := c.tx.Exec(ctx,
		`UPDATE outbox SET attempts = attempts + 1, last_error = $2,
		        next_attempt_at = now() + $3::bigint * interval '1 millisecond'
		 WHERE id = $1`, c.Row.ID, truncate(lastError), delay.Milliseconds()); err != nil {
		return err
	}
	return c.tx.Commit(ctx)
}

// Fail ends the row for good with status "failed" (the adapter refused the effect)
// or "dead" (attempts are used up). The key becomes failed with the recorded
// answer {"status":"rejected","reason":reason}; the audit row is "effect_failed".
func (c *Claim) Fail(ctx context.Context, status, reason, lastError string) error {
	if _, err := c.tx.Exec(ctx,
		`UPDATE outbox SET status = $2, attempts = attempts + 1, last_error = $3 WHERE id = $1`,
		c.Row.ID, status, truncate(lastError)); err != nil {
		return err
	}
	if _, err := c.tx.Exec(ctx,
		`UPDATE idempotency_keys
		    SET state = 'failed', completed_at = now(),
		        result = jsonb_build_object('status', 'rejected', 'reason', $2::text, 'txn_id', txn_id)
		  WHERE key = $1`, c.Row.Key, reason); err != nil {
		return err
	}
	detail, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return err
	}
	return c.finish(ctx, "effect_failed", reason, detail)
}

func (c *Claim) finish(ctx context.Context, event, outcome string, detail []byte) error {
	ev := AuditEvent{
		TenantID: c.Row.TenantID, EventType: event, TraceID: "relay", PrincipalID: "relay",
		IdemKey: c.Row.Key, Outcome: outcome, Detail: detail,
	}
	if err := appendAudit(ctx, c.tx, ev); err != nil {
		return err
	}
	return c.tx.Commit(ctx)
}

func truncate(s string) string {
	if len(s) > 500 {
		return s[:500]
	}
	return s
}
