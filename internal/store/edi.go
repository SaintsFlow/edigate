package store

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Partner is a trading partner of a tenant. Role is "customer" (sends 850) or
// "supplier" (sends 810). The amount limits are filled and used from wave 2-6.
type Partner struct {
	TenantID  string
	PartnerID string // ISA05:ISA06, for example ZZ:ZZTESTBUYER
	Role      string
	Name      string
}

// AddPartner registers a partner, or updates the role and name of a known one.
func (s *Store) AddPartner(ctx context.Context, p Partner) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO partners (tenant_id, partner_id, role, name) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (tenant_id, partner_id) DO UPDATE SET role = EXCLUDED.role, name = EXCLUDED.name`,
		p.TenantID, p.PartnerID, p.Role, p.Name)
	return err
}

// PartnerAccess is what /edi/inbound needs to know about a partner token.
type PartnerAccess struct {
	Role      string
	Active    bool
	GatewayID string // ISA07:ISA08 of the tenant; empty when the tenant has none
}

// PartnerAccess reads the partner and the tenant's interchange id. ErrNotFound
// when the partner is not registered for the tenant.
func (s *Store) PartnerAccess(ctx context.Context, tenantID, partnerID string) (PartnerAccess, error) {
	var a PartnerAccess
	err := s.pool.QueryRow(ctx,
		`SELECT p.role, p.active, COALESCE(t.interchange_id, '')
		 FROM partners p JOIN tenants t ON t.id = p.tenant_id
		 WHERE p.tenant_id = $1 AND p.partner_id = $2`, tenantID, partnerID).
		Scan(&a.Role, &a.Active, &a.GatewayID)
	if isNoRows(err) {
		return PartnerAccess{}, ErrNotFound
	}
	return a, err
}

// InboundDocument is one transaction set of an interchange to store.
type InboundDocument struct {
	GS06        string
	ST01        string
	ST02        string
	BusinessRef string // "" is stored as NULL
	BKRef       string // raw BEG03 or BIG02, internal only; "" is stored as NULL
	Body        []byte // JSON; nil is stored as NULL
	Status      string // received or rejected
	Errors      []byte // JSON array; nil is stored as NULL
	Notes       []InboundNote
}

// InboundNote is a piece of partner text kept out of the body, stored with the
// document it came from.
type InboundNote struct {
	Ref         string
	SegmentPath string
	Reason      string // free_text or format
	Body        string
	TaintLabel  string
	Canary      string
}

// InboundInterchange is a parsed interchange from a partner, ready to store.
type InboundInterchange struct {
	TenantID  string
	PartnerID string
	ISA13     string
	MessageID string
	Raw       []byte
	SHA256    []byte // of Raw; also stored as the MIC
	MDN       []byte // JSON of the MDN sent for a new interchange
	Documents []InboundDocument
	Acks      []InboundAck // the verdicts of the 997 sets of the interchange, matched in the same transaction
}

// Outcomes of SaveInbound.
const (
	SavedNew       = "ok"
	SavedDuplicate = "duplicate"
	SavedConflict  = "isa13_conflict"
)

// SaveResult tells what SaveInbound did. MDN is the one stored with the first
// copy of a duplicate interchange.
type SaveResult struct {
	Outcome string
	MDN     []byte
}

// SaveInbound stores an interchange and its documents in one transaction. The
// unique index on (tenant, direction, partner, ISA13) decides a race between two
// identical requests: the second one waits for the first and then reads its row.
// Same ISA13 and same SHA-256: duplicate, nothing is written, the stored MDN is
// returned. Same ISA13 and another body: the body goes to edi_rejected and
// nothing else is written.
func (s *Store) SaveInbound(ctx context.Context, in InboundInterchange) (SaveResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SaveResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	err = tx.QueryRow(ctx,
		`INSERT INTO edi_interchanges
		   (tenant_id, direction, counterparty_id, isa13, message_id, raw, raw_sha256, mic_sha256, mdn)
		 VALUES ($1, 'in', $2, $3, $4, $5, $6, $6, $7)
		 ON CONFLICT (tenant_id, direction, counterparty_id, isa13) DO NOTHING
		 RETURNING id::text`,
		in.TenantID, in.PartnerID, in.ISA13, in.MessageID, in.Raw, in.SHA256, in.MDN).Scan(&id)

	if isNoRows(err) {
		var sum, mdn []byte
		err = tx.QueryRow(ctx,
			`SELECT raw_sha256, mdn FROM edi_interchanges
			 WHERE tenant_id = $1 AND direction = 'in' AND counterparty_id = $2 AND isa13 = $3`,
			in.TenantID, in.PartnerID, in.ISA13).Scan(&sum, &mdn)
		if err != nil {
			return SaveResult{}, fmt.Errorf("read the first copy of the interchange: %w", err)
		}
		if bytes.Equal(sum, in.SHA256) {
			return SaveResult{Outcome: SavedDuplicate, MDN: mdn}, nil
		}
		if err := insertRejected(ctx, tx, in.TenantID, in.PartnerID, in.ISA13, in.Raw, in.SHA256, "isa13_conflict", ""); err != nil {
			return SaveResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return SaveResult{}, err
		}
		return SaveResult{Outcome: SavedConflict}, nil
	}
	if err != nil {
		return SaveResult{}, err
	}

	for _, d := range in.Documents {
		var docID string
		err = tx.QueryRow(ctx,
			`INSERT INTO edi_documents
			   (interchange_id, tenant_id, partner_id, gs06, st01, st02, business_ref, bk_ref, body, status, errors)
			 VALUES ($1::uuid, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''), $9, $10, $11)
			 RETURNING id::text`,
			id, in.TenantID, in.PartnerID, d.GS06, d.ST01, d.ST02, d.BusinessRef, d.BKRef, d.Body, d.Status, d.Errors).Scan(&docID)
		if err != nil {
			return SaveResult{}, fmt.Errorf("store document: %w", err)
		}
		for _, n := range d.Notes {
			_, err = tx.Exec(ctx,
				`INSERT INTO edi_notes (document_id, tenant_id, ref, segment_path, reason, body, taint_label, canary)
				 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)`,
				docID, in.TenantID, n.Ref, n.SegmentPath, n.Reason, n.Body, n.TaintLabel, n.Canary)
			if err != nil {
				return SaveResult{}, fmt.Errorf("store note: %w", err)
			}
		}
	}
	if err := matchAcks(ctx, tx, in.TenantID, in.PartnerID, in.Acks); err != nil {
		return SaveResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SaveResult{}, err
	}
	return SaveResult{Outcome: SavedNew}, nil
}

// SaveRejected stores an interchange that was refused as a whole (envelope
// error). isa13 is "" when it could not be read.
func (s *Store) SaveRejected(ctx context.Context, tenantID, partnerID, isa13 string, raw, sha []byte, detail string) error {
	return insertRejected(ctx, s.pool, tenantID, partnerID, isa13, raw, sha, "envelope_error", detail)
}

// execer is the part of a pool and of a transaction that insertRejected needs.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertRejected(ctx context.Context, db execer, tenantID, partnerID, isa13 string, raw, sha []byte, reason, detail string) error {
	_, err := db.Exec(ctx,
		`INSERT INTO edi_rejected (tenant_id, partner_id, isa13, raw, raw_sha256, reason, detail)
		 VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, NULLIF($7, ''))`,
		tenantID, partnerID, isa13, raw, sha, reason, detail)
	return err
}

// Document is a stored document as the read tools see it.
type Document struct {
	ID          string
	ST01        string
	PartnerID   string
	BusinessRef string
	Status      string
	Body        []byte // JSON
	CreatedAt   time.Time
}

// agentVisible limits every agent query to the sets the agent may see: 850 and
// 810 that were accepted. 997 and rejected sets are never shown.
const agentVisible = `st01 IN ('850', '810') AND status IN ('received', 'processed')`

// ListInbox returns the visible documents of one tenant, newest first. status
// "" means both visible statuses.
func (s *Store) ListInbox(ctx context.Context, tenantID, status string, limit int) ([]Document, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, st01, partner_id, COALESCE(business_ref, ''), status, created_at
		 FROM edi_documents
		 WHERE tenant_id = $1 AND `+agentVisible+` AND ($2::text = '' OR status = $2::text)
		 ORDER BY created_at DESC, id
		 LIMIT $3`, tenantID, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	docs := []Document{}
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.ST01, &d.PartnerID, &d.BusinessRef, &d.Status, &d.CreatedAt); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

// ReadDocument returns one visible document of the tenant. A document of
// another tenant, a 997, a rejected set and an unknown id all give ErrNotFound.
func (s *Store) ReadDocument(ctx context.Context, tenantID, id string) (Document, error) {
	var d Document
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, st01, partner_id, COALESCE(business_ref, ''), status, body, created_at
		 FROM edi_documents
		 WHERE tenant_id = $1 AND id = $2::uuid AND `+agentVisible+` AND body IS NOT NULL`, tenantID, id).
		Scan(&d.ID, &d.ST01, &d.PartnerID, &d.BusinessRef, &d.Status, &d.Body, &d.CreatedAt)
	if isNoRows(err) {
		return Document{}, ErrNotFound
	}
	return d, err
}

// NoteRef is what the agent may know about a note: where it is and why it is a
// note. The text stays in the table.
type NoteRef struct {
	Ref         string
	SegmentPath string
	Reason      string
}

// ListNotes returns the notes of a document of the tenant in document order.
// The caller has already read the document, so an unknown id gives an empty list.
func (s *Store) ListNotes(ctx context.Context, tenantID, documentID string) ([]NoteRef, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT n.ref, n.segment_path, n.reason
		 FROM edi_notes n
		 WHERE n.tenant_id = $1 AND n.document_id = $2::uuid
		 ORDER BY substring(n.ref from 7)::int`, tenantID, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notes := []NoteRef{}
	for rows.Next() {
		var n NoteRef
		if err := rows.Scan(&n.Ref, &n.SegmentPath, &n.Reason); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}

// StoredNote is a note with its text, for the quarantined reader only.
type StoredNote struct {
	Body       string
	TaintLabel string
	Canary     string
}

// ReadNote returns a note of a visible document (850 or 810, accepted) of the
// tenant. Another tenant, a 997, a rejected set, an unknown document and an
// unknown ref all give ErrNotFound.
func (s *Store) ReadNote(ctx context.Context, tenantID, documentID, ref string) (StoredNote, error) {
	var n StoredNote
	err := s.pool.QueryRow(ctx,
		`SELECT n.body, n.taint_label, n.canary
		 FROM edi_notes n JOIN edi_documents d ON d.id = n.document_id
		 WHERE n.tenant_id = $1 AND d.tenant_id = $1 AND n.document_id = $2::uuid AND n.ref = $3
		   AND d.`+agentVisible, tenantID, documentID, ref).
		Scan(&n.Body, &n.TaintLabel, &n.Canary)
	if isNoRows(err) {
		return StoredNote{}, ErrNotFound
	}
	return n, err
}

// QuarantineCall is the cost record of one call to the model (protocol item O1).
// The note text and the answer are not part of it.
type QuarantineCall struct {
	TenantID         string
	TraceID          string
	DocumentID       string
	Ref              string
	SchemaID         string
	Model            string
	PromptTokens     *int // nil when the model reported none
	CompletionTokens *int
	LatencyMS        int
	Outcome          string
}

// RecordQuarantineCall stores the cost record of a call.
func (s *Store) RecordQuarantineCall(ctx context.Context, c QuarantineCall) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO quarantine_calls
		   (tenant_id, trace_id, document_id, ref, schema_id, model, prompt_tokens, completion_tokens, latency_ms, outcome)
		 VALUES ($1, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10)`,
		c.TenantID, c.TraceID, c.DocumentID, c.Ref, c.SchemaID, c.Model, c.PromptTokens, c.CompletionTokens, c.LatencyMS, c.Outcome)
	return err
}
