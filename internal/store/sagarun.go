package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SaintsFlow/edigate/internal/adapters"
)

// This file is the state machine of the sagas (invariant I5). A saga is judged from the
// facts in the tables, not from events: the state of the key of each step, of the outbox
// rows, of the document that was sent. AdvanceSaga reads those facts, decides (decide is a
// pure function) and writes the new state with the actions it needs in one transaction, so
// it can be run again at any time, by any process, and a crash in the middle loses nothing.
//
// States: running -> done | compensating | incident; compensating -> compensated | incident.
// A failure before the send runs the compensations of the completed steps, last step first,
// each through the outbox with a key of its own. A failure after the send, a negative 997,
// a missing MDN or 997, and a compensation that failed all end in incident and a review
// request for the operators; the document is never sent again with new numbers.

// SagaStep is one step with the facts about it.
type SagaStep struct {
	No        int
	Name      string // the effect type: onec.create_order, edi.send_855, wms.reserve, wms.ship, edi.send_856
	Status    string
	Key       []byte // the key of the effect; nil until the step is called
	KeyState  string // the state of that key; "" when there is none
	KeyReason string // why the key failed: the recorded reason or status
	CompKey   []byte // the key of the compensation; nil until it is queued
	CompState string
	DocID     string // the document the step sent; "" when it did not send one
	DocState  string
	DocReason string
	Overdue   bool // the MDN was counted and no 997 came within the time limit
}

// Saga is a saga with its steps.
type Saga struct {
	ID, TenantID, Kind, State string
	BusinessKey, Payload      []byte
	SourceDocumentID          string
	Steps                     []SagaStep
}

// AdvanceHints are facts the store cannot find out by itself.
type AdvanceHints struct {
	AckTimeout time.Duration  // how long a sent document may wait for its 997
	Denied     map[int]bool   // steps the gateway could not start because the policy refused
	Existence  map[int]string // for a step that failed without an answer: found, not_found or unknown
}

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// OpenSagas lists the sagas that need attention: those still running or compensating, and
// finished ones whose document turned into an incident afterwards (a contradicting 997).
func (s *Store) OpenSagas(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id::text FROM sagas
		 WHERE state IN ('running', 'compensating')
		    OR (state = 'done' AND EXISTS (SELECT 1 FROM outbound_docs d WHERE d.saga_id = sagas.id AND d.state = 'incident'))
		 ORDER BY created_at LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LoadSaga reads a saga with its steps. An unknown id gives ErrNotFound.
func (s *Store) LoadSaga(ctx context.Context, id string, ackTimeout time.Duration) (Saga, error) {
	return loadSaga(ctx, s.pool, id, ackTimeout)
}

func loadSaga(ctx context.Context, q queryer, id string, ackTimeout time.Duration) (Saga, error) {
	var sg Saga
	err := q.QueryRow(ctx,
		`SELECT id::text, tenant_id, kind, state, business_key, payload, source_document_id::text FROM sagas WHERE id = $1::uuid`, id).
		Scan(&sg.ID, &sg.TenantID, &sg.Kind, &sg.State, &sg.BusinessKey, &sg.Payload, &sg.SourceDocumentID)
	if isNoRows(err) {
		return Saga{}, ErrNotFound
	}
	if err != nil {
		return Saga{}, err
	}
	rows, err := q.Query(ctx,
		`SELECT st.step_no, st.name, st.status, st.idem_key, COALESCE(k.state, ''),
		        COALESCE(k.result->>'reason', k.result->>'status', ''),
		        st.compensation_key, COALESCE(ck.state, ''),
		        COALESCE(d.id::text, ''), COALESCE(d.state, ''), COALESCE(d.incident_reason, ''),
		        COALESCE(d.state = 'sent' AND d.ack_status IS NULL
		                 AND d.mdn_at + $2::bigint * interval '1 millisecond' < now(), false)
		 FROM saga_steps st
		 LEFT JOIN idempotency_keys k ON k.key = st.idem_key
		 LEFT JOIN idempotency_keys ck ON ck.key = st.compensation_key
		 LEFT JOIN outbound_docs d ON d.idem_key = st.idem_key
		 WHERE st.saga_id = $1::uuid ORDER BY st.step_no`, id, ackTimeout.Milliseconds())
	if err != nil {
		return Saga{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var st SagaStep
		if err := rows.Scan(&st.No, &st.Name, &st.Status, &st.Key, &st.KeyState, &st.KeyReason, &st.CompKey, &st.CompState,
			&st.DocID, &st.DocState, &st.DocReason, &st.Overdue); err != nil {
			return Saga{}, err
		}
		sg.Steps = append(sg.Steps, st)
	}
	return sg, rows.Err()
}

// compensations maps the effect of a step to its compensation: the effect type, the field of
// the payload that names the effect to undo, and the suffix of the compensation key.
var compensations = map[string]struct{ effect, field, suffix string }{
	adapters.EffectCreateOrder: {adapters.EffectCancelOrder, "order_external_id", "cancel"},
	adapters.EffectWMSReserve:  {adapters.EffectWMSRelease, "reservation_external_id", "release"},
}

// sagaPlan is what decide wants done.
type sagaPlan struct {
	State      string         // the new state; "" keeps it
	Statuses   map[int]string // new step statuses
	Abort      []int          // steps whose key is failed because the saga is abandoned
	Compensate *SagaStep      // the step whose compensation is queued now
	Incident   *sagaIncident  // a review request to open
	Overdue    []string       // documents that get the incident ack_missing
}

type sagaIncident struct{ Rule, Reason, Step string }

// reviewRule names the rule of the review request for the reason of an incident.
func reviewRule(reason string) string {
	switch reason {
	case "no_mdn":
		return "mdn.missing"
	case "ack_rejected":
		return "ack.rejected"
	case "ack_conflict":
		return "ack.conflict"
	case "ack_missing":
		return "ack.missing"
	}
	return "ack.incident"
}

// decide is the whole logic of a saga step by step. It reads the facts and returns the plan;
// it does not touch the database.
func decide(sg Saga, h AdvanceHints) sagaPlan {
	p := sagaPlan{Statuses: map[int]string{}}
	if sg.State == "incident" || sg.State == "compensated" {
		return p
	}
	for _, st := range sg.Steps {
		if st.DocState == "incident" {
			p.State, p.Incident = "incident", &sagaIncident{reviewRule(st.DocReason), st.DocReason, st.Name}
			return p
		}
	}
	if sg.State == "done" {
		return p
	}
	if sg.State == "running" {
		for _, st := range sg.Steps {
			if st.Overdue {
				p.State, p.Overdue = "incident", []string{st.DocID}
				p.Incident = &sagaIncident{reviewRule("ack_missing"), "ack_missing", st.Name}
				return p
			}
		}
		failed := false
		for _, st := range sg.Steps {
			failed = failed || st.KeyState == "failed" || h.Denied[st.No]
		}
		if !failed {
			done := true
			for _, st := range sg.Steps {
				ok := st.KeyState == "completed" && (!adapters.IsSend(st.Name) || st.DocState == "delivered")
				if ok {
					p.Statuses[st.No] = "done"
				}
				done = done && ok
			}
			if done {
				p.State = "done"
			}
			return p
		}
		p.State = "compensating"
		for _, st := range sg.Steps {
			if st.KeyState == "reserved" || st.KeyState == "reopened" {
				p.Abort = append(p.Abort, st.No)
				p.Statuses[st.No] = "failed"
			}
		}
	}
	for _, st := range sg.Steps {
		switch {
		case st.KeyState == "completed":
			p.Statuses[st.No] = "done"
		case st.KeyState == "failed" || h.Denied[st.No]:
			p.Statuses[st.No] = "failed"
		}
	}
	for i := len(sg.Steps) - 1; i >= 0; i-- {
		st := sg.Steps[i]
		if _, has := compensations[st.Name]; !has {
			continue
		}
		if !(st.KeyState == "completed" || (st.KeyState == "failed" && st.KeyReason == "dead")) {
			continue
		}
		switch {
		case st.CompKey == nil:
			p.Compensate = &st
			return p
		case st.CompState == "completed":
			p.Statuses[st.No] = "compensated"
		case st.CompState == "failed":
			p.State, p.Incident = "incident", &sagaIncident{"saga.compensation_failed", "compensation_failed", st.Name}
			return p
		default:
			return p // the compensation is on its way
		}
	}
	p.State = "compensated"
	for _, st := range sg.Steps {
		if st.Key == nil && !h.Denied[st.No] && st.Status != "failed" {
			p.Statuses[st.No] = "skipped"
		}
	}
	return p
}

// AdvanceSaga looks at the facts of a saga and does the next thing, in one transaction. It
// reports whether anything changed. A saga another process is working on is left to it.
func (s *Store) AdvanceSaga(ctx context.Context, id string, h AdvanceHints) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var locked string
	err = tx.QueryRow(ctx, `SELECT id::text FROM sagas WHERE id = $1::uuid FOR UPDATE SKIP LOCKED`, id).Scan(&locked)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sg, err := loadSaga(ctx, tx, id, h.AckTimeout)
	if err != nil {
		return false, err
	}
	p := decide(sg, h)
	changed, err := s.applyPlan(ctx, tx, sg, p, h)
	if err != nil || !changed {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (s *Store) applyPlan(ctx context.Context, tx pgx.Tx, sg Saga, p sagaPlan, h AdvanceHints) (bool, error) {
	changed := false
	for _, st := range sg.Steps {
		if status, ok := p.Statuses[st.No]; ok && status != st.Status {
			if _, err := tx.Exec(ctx, `UPDATE saga_steps SET status = $3 WHERE saga_id = $1::uuid AND step_no = $2`, sg.ID, st.No, status); err != nil {
				return false, err
			}
			changed = true
		}
	}
	for _, no := range p.Abort {
		if err := abortStep(ctx, tx, sg, sg.Steps[no-1]); err != nil {
			return false, err
		}
		changed = true
	}
	if p.Compensate != nil {
		if err := enqueueCompensation(ctx, tx, sg, *p.Compensate, h.Existence[p.Compensate.No]); err != nil {
			return false, err
		}
		changed = true
	}
	for _, doc := range p.Overdue {
		if _, err := tx.Exec(ctx,
			`UPDATE outbound_docs SET state = 'incident', incident_reason = 'ack_missing' WHERE id = $1::uuid`, doc); err != nil {
			return false, err
		}
		if err := appendAudit(ctx, tx, AuditEvent{TenantID: sg.TenantID, EventType: "ack_missing", TraceID: "saga", PrincipalID: "saga", Outcome: "incident"}); err != nil {
			return false, err
		}
		changed = true
	}
	if p.Incident != nil {
		if err := s.openSagaReview(ctx, tx, sg, *p.Incident); err != nil {
			return false, err
		}
		changed = true
	}
	if p.State != "" && p.State != sg.State {
		if _, err := tx.Exec(ctx, `UPDATE sagas SET state = $2, updated_at = now() WHERE id = $1::uuid`, sg.ID, p.State); err != nil {
			return false, err
		}
		detail, err := json.Marshal(map[string]string{"saga_id": sg.ID, "kind": sg.Kind, "from": sg.State, "to": p.State})
		if err != nil {
			return false, err
		}
		if err := appendAudit(ctx, tx, AuditEvent{TenantID: sg.TenantID, EventType: "saga_state", TraceID: "saga", PrincipalID: "saga", Outcome: p.State, Detail: detail}); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

// abortStep abandons a step that has not finished because a later one failed: its queued
// outbox row and its open request are closed and its key fails. A row the relay is
// delivering right now is waited for, and a step that turned out completed stays completed
// (the next pass compensates it).
func abortStep(ctx context.Context, tx pgx.Tx, sg Saga, st SagaStep) error {
	if _, err := tx.Exec(ctx,
		`UPDATE outbox SET status = 'failed', last_error = 'saga_aborted' WHERE idem_key = $1 AND status = 'pending'`, st.Key); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE approvals SET status = 'rejected', decided_at = now()
		 WHERE idem_key = $1 AND kind = 'effect' AND status = 'pending'`, st.Key); err != nil {
		return err
	}
	if err := failKeyIfReserved(ctx, tx, st.Key); err != nil {
		return err
	}
	return appendAudit(ctx, tx, AuditEvent{TenantID: sg.TenantID, EventType: "saga_step_aborted", TraceID: "saga", PrincipalID: "saga", IdemKey: st.Key, Outcome: st.Name})
}

func failKeyIfReserved(ctx context.Context, tx pgx.Tx, key []byte) error {
	_, err := tx.Exec(ctx,
		`UPDATE idempotency_keys
		    SET state = 'failed', completed_at = now(),
		        result = jsonb_build_object('status', 'rejected', 'reason', 'saga_aborted', 'txn_id', txn_id)
		  WHERE key = $1 AND state IN ('reserved', 'reopened')`, key)
	return err
}

// enqueueCompensation queues the compensation of a step through the outbox with a key of its
// own, SHA-256 of the key of the step and "cancel" or "release". existence is what
// Adapter.Exists answered for a step that failed without an answer; it goes to the audit log.
func enqueueCompensation(ctx context.Context, tx pgx.Tx, sg Saga, st SagaStep, existence string) error {
	c := compensations[st.Name]
	h := sha256.New()
	h.Write(st.Key)
	h.Write([]byte(c.suffix))
	compKey := h.Sum(nil)
	payload, err := json.Marshal(map[string]string{c.field: hex.EncodeToString(st.Key)})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO idempotency_keys (key, tenant_id, tool, business_key, req_hash, state, txn_id, result)
		 VALUES ($1, $2, $3, $4, $5, 'reserved', gen_random_uuid(), jsonb_build_object('status', 'accepted'))
		 ON CONFLICT (key) DO NOTHING`,
		compKey, sg.TenantID, st.Name+".compensate", sg.BusinessKey, sha256Sum(payload)); err != nil {
		return fmt.Errorf("reserve the compensation key: %w", err)
	}
	outboxID, err := insertOutboxReturning(ctx, tx, sg.TenantID, compKey, c.effect, payload)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE saga_steps SET compensation_key = $3, compensation_outbox_id = $4 WHERE saga_id = $1::uuid AND step_no = $2`,
		sg.ID, st.No, compKey, outboxID); err != nil {
		return err
	}
	detail, err := json.Marshal(map[string]string{"step": st.Name, "effect_exists": existence})
	if err != nil {
		return err
	}
	return appendAudit(ctx, tx, AuditEvent{TenantID: sg.TenantID, EventType: "saga_compensation_queued", TraceID: "saga", PrincipalID: "saga", IdemKey: compKey, Outcome: c.effect, Detail: detail})
}

// openSagaReview opens the review request for an incident. It is tied to the key of the
// step that went wrong (or of its compensation); one request stays open per key.
func (s *Store) openSagaReview(ctx context.Context, tx pgx.Tx, sg Saga, in sagaIncident) error {
	var key []byte
	for _, st := range sg.Steps {
		if st.Name != in.Step {
			continue
		}
		key = st.Key
		if in.Rule == "saga.compensation_failed" {
			key = st.CompKey
		}
	}
	if key == nil {
		return nil
	}
	view, err := json.Marshal(map[string]any{
		"tool": "saga", "tier": 0, "rules": []string{in.Rule},
		"args": map[string]string{"saga_kind": sg.Kind, "step": in.Step, "reason": in.Reason},
	})
	if err != nil {
		return err
	}
	rules, err := json.Marshal([]string{in.Rule})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO approvals (tenant_id, idem_key, kind, tool, args, view, rules, tier, approvers, status, requested_by, expires_at)
		 VALUES ($1, $2, 'review', 'saga', '{}', $3, $4, 0, 1, 'pending', 'saga', now() + $5::bigint * interval '1 millisecond')
		 ON CONFLICT DO NOTHING`, sg.TenantID, key, view, rules, s.ttl().Milliseconds()); err != nil {
		return fmt.Errorf("open the review request: %w", err)
	}
	return appendAudit(ctx, tx, AuditEvent{TenantID: sg.TenantID, EventType: "saga_incident", TraceID: "saga", PrincipalID: "saga", IdemKey: key, Outcome: in.Rule})
}
