package store

import "context"

// AuditEvent is one row of audit_log. Empty Tool and nil hashes are stored as NULL.
type AuditEvent struct {
	TenantID      string
	EventType     string
	TraceID       string
	PrincipalID   string
	Tool          string
	PayloadSHA256 []byte
	Outcome       string
}

// AppendAudit adds a row to audit_log. The table rejects UPDATE and DELETE.
func (s *Store) AppendAudit(ctx context.Context, e AuditEvent) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO audit_log (tenant_id, event_type, trace_id, principal_id, tool, payload_sha256, outcome)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, NULLIF($7, ''))`,
		e.TenantID, e.EventType, e.TraceID, e.PrincipalID, e.Tool, e.PayloadSHA256, e.Outcome)
	return err
}
