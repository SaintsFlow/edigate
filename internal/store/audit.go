package store

import "context"

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
}

// AppendAudit adds a row to audit_log. The table rejects UPDATE and DELETE.
func (s *Store) AppendAudit(ctx context.Context, e AuditEvent) error {
	return appendAudit(ctx, s.pool, e)
}

// appendAudit writes the row through a pool or through a transaction, so that an
// event that belongs to a change commits or rolls back with it.
func appendAudit(ctx context.Context, db execer, e AuditEvent) error {
	_, err := db.Exec(ctx,
		`INSERT INTO audit_log (tenant_id, event_type, trace_id, principal_id, tool, idem_key, payload_sha256, outcome, detail)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, NULLIF($8, ''), $9)`,
		e.TenantID, e.EventType, e.TraceID, e.PrincipalID, e.Tool, e.IdemKey, e.PayloadSHA256, e.Outcome, e.Detail)
	return err
}
