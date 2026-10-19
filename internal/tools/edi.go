// Package tools holds the services behind the MCP tools. Handlers in
// internal/mcpserver call these functions with the tenant taken from the
// caller's principal; the services never read identity from the request.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/SaintsFlow/edigate/internal/edi/document"
	"github.com/SaintsFlow/edigate/internal/quarantine"
	"github.com/SaintsFlow/edigate/internal/rpcerr"
	"github.com/SaintsFlow/edigate/internal/store"
)

const defaultInboxLimit = 50

// ListInboxInput is the validated input of edi.list_inbox.
type ListInboxInput struct {
	Status string `json:"status,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// Document is one inbox entry as returned to the agent (explicit field list).
type Document struct {
	ID          string `json:"id"`
	ST01        string `json:"st01"`
	PartnerID   string `json:"partner_id"`
	BusinessRef string `json:"business_ref"`
	ReceivedAt  string `json:"received_at"`
	Status      string `json:"status"`
}

// ListInboxOutput is the result of edi.list_inbox.
type ListInboxOutput struct {
	Documents []Document `json:"documents"`
}

// ListInbox returns the 850 and 810 documents of a tenant that were accepted,
// newest first. 997 and rejected sets are never listed.
func ListInbox(ctx context.Context, st *store.Store, tenantID string, in ListInboxInput) (ListInboxOutput, error) {
	limit := in.Limit
	if limit == 0 {
		limit = defaultInboxLimit
	}
	rows, err := st.ListInbox(ctx, tenantID, in.Status, limit)
	if err != nil {
		return ListInboxOutput{}, err
	}
	out := ListInboxOutput{Documents: make([]Document, 0, len(rows))}
	for _, r := range rows {
		out.Documents = append(out.Documents, Document{
			ID: r.ID, ST01: r.ST01, PartnerID: r.PartnerID, BusinessRef: r.BusinessRef,
			ReceivedAt: r.CreatedAt.UTC().Format(time.RFC3339), Status: r.Status,
		})
	}
	return out, nil
}

// ReadDocumentInput is the validated input of edi.read_document.
type ReadDocumentInput struct {
	DocumentID string `json:"document_id"`
}

// Note points at a piece of partner text that is kept out of the document: free
// text, or an element that failed its format. The text itself is not part of the
// answer.
type Note struct {
	Ref         string `json:"ref"`
	SegmentPath string `json:"segment_path"`
	Reason      string `json:"reason"`
}

// ReadDocumentOutput is the result of edi.read_document: typed fields only.
type ReadDocumentOutput struct {
	ID     string          `json:"id"`
	ST01   string          `json:"st01"`
	Status string          `json:"status"`
	Header any             `json:"header"`
	Lines  []document.Line `json:"lines"`
	Notes  []Note          `json:"notes"`
}

// ReadDocument returns one document of the tenant. A document of another
// tenant, a 997, a rejected set and an unknown id all give the same not_found.
func ReadDocument(ctx context.Context, st *store.Store, tenantID string, in ReadDocumentInput) (ReadDocumentOutput, error) {
	d, err := st.ReadDocument(ctx, tenantID, in.DocumentID)
	if errors.Is(err, store.ErrNotFound) {
		return ReadDocumentOutput{}, rpcerr.New(rpcerr.NotFound, "")
	}
	if err != nil {
		return ReadDocumentOutput{}, err
	}
	header, lines, err := document.Decode(d.ST01, d.Body)
	if err != nil {
		return ReadDocumentOutput{}, err
	}
	refs, err := st.ListNotes(ctx, tenantID, d.ID)
	if err != nil {
		return ReadDocumentOutput{}, err
	}
	notes := make([]Note, 0, len(refs))
	for _, r := range refs {
		notes = append(notes, Note{Ref: r.Ref, SegmentPath: r.SegmentPath, Reason: r.Reason})
	}
	contacts, err := st.PartnerContacts(ctx, tenantID, d.PartnerID)
	if err != nil {
		return ReadDocumentOutput{}, err
	}
	return ReadDocumentOutput{ID: d.ID, ST01: d.ST01, Status: d.Status, Header: headerWithContacts{header, contacts}, Lines: lines, Notes: notes}, nil
}

// headerWithContacts (used by ReadDocument and ReadDocumentRaw) is the typed header of the document plus the addresses of the partner
// from the starting data of the gateway (table partner_contacts), so that the agent can pick
// the "to" of notify.send_email. The addresses are trusted data and never come from the document.
type headerWithContacts struct {
	header   any
	contacts []string
}

// MarshalJSON writes the fields of the header and then "partner_contacts".
func (h headerWithContacts) MarshalJSON() ([]byte, error) {
	base, err := json.Marshal(h.header)
	if err != nil {
		return nil, err
	}
	contacts, err := json.Marshal(h.contacts)
	if err != nil {
		return nil, err
	}
	if len(base) < 2 || base[len(base)-1] != '}' {
		return nil, errors.New("the header of a document is not an object")
	}
	out := append([]byte{}, base[:len(base)-1]...)
	out = append(out, `,"partner_contacts":`...)
	out = append(out, contacts...)
	return append(out, '}'), nil
}

// ExtractNoteInput is the validated input of edi.extract_note.
type ExtractNoteInput struct {
	DocumentID string `json:"document_id"`
	Ref        string `json:"ref"`
	SchemaID   string `json:"schema_id"`
}

// ExtractNoteOutput is the result of edi.extract_note: only the values of the
// closed schema, always declassified.
type ExtractNoteOutput struct {
	SchemaID     string          `json:"schema_id"`
	Values       json.RawMessage `json:"values"`
	Declassified bool            `json:"declassified"`
}

// ExtractNote reads one note through the quarantined reader. A note that is not
// of this tenant, of a hidden document or unknown gives not_found; a missing or
// refused answer of the model gives quarantine_invalid. The text of the note is
// never part of the answer.
func ExtractNote(ctx context.Context, st *store.Store, reader quarantine.QuarantinedReader, tenantID, traceID string, in ExtractNoteInput) (ExtractNoteOutput, error) {
	n, err := st.ReadNote(ctx, tenantID, in.DocumentID, in.Ref)
	if errors.Is(err, store.ErrNotFound) {
		return ExtractNoteOutput{}, rpcerr.New(rpcerr.NotFound, "")
	}
	if err != nil {
		return ExtractNoteOutput{}, err
	}
	if reader == nil {
		return ExtractNoteOutput{}, rpcerr.New(rpcerr.QuarantineInvalid, "")
	}
	values, err := reader.Extract(ctx, quarantine.Note{
		TenantID: tenantID, TraceID: traceID, DocumentID: in.DocumentID, Ref: in.Ref, Text: n.Body, Canary: n.Canary,
	}, in.SchemaID)
	if errors.Is(err, quarantine.ErrInvalid) {
		return ExtractNoteOutput{}, rpcerr.New(rpcerr.QuarantineInvalid, "")
	}
	if err != nil {
		return ExtractNoteOutput{}, err
	}
	return ExtractNoteOutput{SchemaID: in.SchemaID, Values: values, Declassified: true}, nil
}
