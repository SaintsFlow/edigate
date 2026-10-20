package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
)

// This file holds the queries of the open tools of the modes plain and reqhash (wave 2-9a)
// and the two reads that the modes with raw text need. The open tools keep a light row for
// every call that did its effect (table plain_txns) and take control numbers from the same
// counters as the sagas.

// NewTxnID makes a random version 4 UUID for the answer of an open tool.
func NewTxnID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// PlainTxn is a row of plain_txns.
type PlainTxn struct {
	TxnID  string
	Tool   string
	Args   []byte // JSON
	Result []byte // JSON: the answer the tool gave
}

// PlainByKey finds the stored answer of a request key (mode reqhash).
func (s *Store) PlainByKey(ctx context.Context, tenantID string, key []byte) (PlainTxn, bool, error) {
	var t PlainTxn
	err := s.pool.QueryRow(ctx,
		`SELECT txn_id::text, tool, args, result FROM plain_txns WHERE tenant_id = $1 AND req_key = $2`, tenantID, key).
		Scan(&t.TxnID, &t.Tool, &t.Args, &t.Result)
	if isNoRows(err) {
		return PlainTxn{}, false, nil
	}
	return t, err == nil, err
}

// PlainSave writes the row of a call that did its effect. key is nil in the mode plain. When two
// calls with one key finish together, the first row stays and is returned with saved false.
func (s *Store) PlainSave(ctx context.Context, tenantID string, t PlainTxn, key []byte) (saved bool, stored PlainTxn, err error) {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO plain_txns (txn_id, tenant_id, tool, args, result, req_key)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
		t.TxnID, tenantID, t.Tool, t.Args, t.Result, key)
	if err != nil {
		return false, PlainTxn{}, err
	}
	if tag.RowsAffected() == 1 {
		return true, t, nil
	}
	stored, found, err := s.PlainByKey(ctx, tenantID, key)
	if err == nil && !found {
		err = errors.New("the row of an open call is neither saved nor found")
	}
	return false, stored, err
}

// PlainState reads the answer an open tool gave for a txn_id of the tenant. Another tenant's
// txn_id and an unknown one both give ErrNotFound.
func (s *Store) PlainState(ctx context.Context, tenantID, txnID string) (PlainTxn, error) {
	var t PlainTxn
	err := s.pool.QueryRow(ctx,
		`SELECT txn_id::text, tool, args, result FROM plain_txns WHERE tenant_id = $1 AND txn_id = $2::uuid`, tenantID, txnID).
		Scan(&t.TxnID, &t.Tool, &t.Args, &t.Result)
	if isNoRows(err) {
		return PlainTxn{}, ErrNotFound
	}
	return t, err
}

// PlainLast returns the arguments of the last call of a tool for a partner and a business
// reference, or ErrNotFound. Confirming and shipping an order find its lines this way.
func (s *Store) PlainLast(ctx context.Context, tenantID, tool, partnerID, ref string) ([]byte, error) {
	var args []byte
	err := s.pool.QueryRow(ctx,
		`SELECT args FROM plain_txns
		 WHERE tenant_id = $1 AND tool = $2 AND args->>'partner_id' = $3 AND args->>'business_ref' = $4
		 ORDER BY created_at DESC LIMIT 1`, tenantID, tool, partnerID, ref).Scan(&args)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	return args, err
}

// ControlNumbers is what a document to a partner needs besides its content.
type ControlNumbers struct {
	Gateway     string // the interchange id of the tenant (ISA07:ISA08)
	ISA, GS, ST int64
}

// NextControlNumbers allocates the three control numbers of one outgoing interchange from the
// counters the sagas use. The partner must be registered; the tenant must have an interchange id.
func (s *Store) NextControlNumbers(ctx context.Context, tenantID, partnerID string) (ControlNumbers, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ControlNumbers{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var gateway *string
	if err := tx.QueryRow(ctx, `SELECT interchange_id FROM tenants WHERE id = $1`, tenantID).Scan(&gateway); err != nil {
		return ControlNumbers{}, err
	}
	if gateway == nil {
		return ControlNumbers{}, errors.New("the tenant has no interchange id: it cannot send documents")
	}
	n := ControlNumbers{Gateway: *gateway}
	// A fixed order, so that two callers cannot lock the counters crosswise.
	if n.ISA, err = nextNumber(ctx, tx, tenantID, partnerID, "isa"); err != nil {
		return ControlNumbers{}, err
	}
	if n.GS, err = nextNumber(ctx, tx, tenantID, partnerID, "gs"); err != nil {
		return ControlNumbers{}, err
	}
	if n.ST, err = nextNumber(ctx, tx, tenantID, partnerID, "st"); err != nil {
		return ControlNumbers{}, err
	}
	return n, tx.Commit(ctx)
}

// PartnerOfContact returns the partner that has this address among its contacts, or ErrNotFound.
// A letter written by the model names no partner, so the mode no-contracts finds it this way.
func (s *Store) PartnerOfContact(ctx context.Context, tenantID, email string) (string, error) {
	var partner string
	err := s.pool.QueryRow(ctx,
		`SELECT partner_id FROM partner_contacts WHERE tenant_id = $1 AND email = $2 ORDER BY partner_id LIMIT 1`,
		tenantID, email).Scan(&partner)
	if isNoRows(err) {
		return "", ErrNotFound
	}
	return partner, err
}

// RawNote is a note with its text, for the modes that show the text of the partner.
type RawNote struct {
	Ref         string
	SegmentPath string
	Reason      string
	Text        string
}

// DocumentNotes returns the notes of a document of the tenant with their text, in document
// order. The caller has already read the document, so an unknown id gives an empty list.
func (s *Store) DocumentNotes(ctx context.Context, tenantID, documentID string) ([]RawNote, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT ref, segment_path, reason, body FROM edi_notes
		 WHERE tenant_id = $1 AND document_id = $2::uuid ORDER BY substring(ref from 7)::int`, tenantID, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notes := []RawNote{}
	for rows.Next() {
		var n RawNote
		if err := rows.Scan(&n.Ref, &n.SegmentPath, &n.Reason, &n.Text); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}
