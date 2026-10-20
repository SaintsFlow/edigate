package store

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SaintsFlow/edigate/internal/audit"
)

// AuditEvent is one row of audit_log. Empty Tool and nil hashes, keys and
// details are stored as NULL.
type AuditEvent struct {
	TenantID      string
	EventType     string
	TraceID       string
	PrincipalID   string
	Tool          string
	IdemKey       []byte
	PayloadSHA256 []byte
	Outcome       string
	Detail        []byte // JSON
	PolicyExplain []byte // JSON: the decision of the policy, its deny and fired rules

	ControlNumbers []byte // JSON: ISA13, GS06 and ST02 of an outgoing document
}

// AppendAudit adds a row to audit_log in a transaction of its own. The table rejects
// UPDATE and DELETE.
func (s *Store) AppendAudit(ctx context.Context, e AuditEvent) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // after a commit this is a no-op
	if err := appendAudit(ctx, tx, e); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// appendAudit writes the row inside a transaction, so that an event that belongs to a
// change commits or rolls back with it. The row joins the hash chain of its tenant: the
// transaction takes an advisory lock for the tenant first, so rows of one tenant are chained
// one after the other (the lock ends with the transaction), and reads the hash of the last
// row. Different tenants do not wait for each other.
func appendAudit(ctx context.Context, tx pgx.Tx, e AuditEvent) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('edigate.audit:' || $1::text, 0))`, e.TenantID); err != nil {
		return err
	}
	prev := audit.ZeroHash()
	err := tx.QueryRow(ctx,
		`SELECT hash FROM audit_log WHERE tenant_id = $1 AND hash IS NOT NULL ORDER BY seq DESC LIMIT 1`, e.TenantID).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	row := audit.Row{
		TenantID: e.TenantID, At: time.Now().UTC().Truncate(time.Microsecond), EventType: e.EventType,
		TraceID: e.TraceID, PrincipalID: e.PrincipalID, Tool: e.Tool, IdemKey: nilIfEmpty(e.IdemKey),
		ControlNumbers: nilIfEmpty(e.ControlNumbers), PolicyExplain: nilIfEmpty(e.PolicyExplain),
		PayloadSHA256: nilIfEmpty(e.PayloadSHA256), Outcome: e.Outcome, Detail: nilIfEmpty(e.Detail), PrevHash: prev,
	}
	if err := tx.QueryRow(ctx, `SELECT nextval(pg_get_serial_sequence('audit_log', 'seq'))`).Scan(&row.Seq); err != nil {
		return err
	}
	hash, err := audit.Hash(row)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO audit_log (seq, tenant_id, at, event_type, trace_id, principal_id, tool, idem_key, payload_sha256, outcome,
		                        detail, policy_explain, control_numbers, prev_hash, hash)
		 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, NULLIF($10, ''), $11, $12, $13, $14, $15)`,
		row.Seq, row.TenantID, row.At, row.EventType, row.TraceID, row.PrincipalID, row.Tool, row.IdemKey, row.PayloadSHA256,
		row.Outcome, row.Detail, row.PolicyExplain, row.ControlNumbers, row.PrevHash, hash)
	return err
}

// RunAuditAnchors signs and writes the anchors of the audit chain every interval until ctx
// ends (see audit.Anchorer).
func (s *Store) RunAuditAnchors(ctx context.Context, a *audit.Anchorer, interval time.Duration, log *slog.Logger) {
	a.Run(ctx, s.pool, interval, log)
}

// AnchorAuditNow makes the anchors that are due and brings the anchor files up to date.
func (s *Store) AnchorAuditNow(ctx context.Context, a *audit.Anchorer) error {
	return a.Once(ctx, s.pool)
}

// VerifyAudit checks the audit chain and the anchors of a tenant (see audit.Verify).
func (s *Store) VerifyAudit(ctx context.Context, o audit.Options) (audit.Result, error) {
	return audit.Verify(ctx, s.pool, o)
}

func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}
