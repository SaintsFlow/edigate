// Package tools holds the services behind the MCP tools. Handlers in
// internal/mcpserver call these functions with the tenant taken from the
// caller's principal; the services never read identity from the request.
package tools

import (
	"context"
	"errors"
	"time"

	"github.com/SaintsFlow/edigate/internal/edi/document"
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

// Note points at a piece of free text of the document. The text itself is not
// part of the answer; notes arrive with wave 2-3, so the list is empty for now.
type Note struct {
	Ref         string `json:"ref"`
	SegmentPath string `json:"segment_path"`
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
	return ReadDocumentOutput{ID: d.ID, ST01: d.ST01, Status: d.Status, Header: header, Lines: lines, Notes: []Note{}}, nil
}
