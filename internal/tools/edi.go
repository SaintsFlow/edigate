// Package tools holds the services behind the MCP tools. Handlers in
// internal/mcpserver call these functions with the tenant taken from the
// caller's principal; the services never read identity from the request.
package tools

import "context"

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

// ListInbox returns the documents of a tenant. Document storage does not exist
// yet, so the list is always empty (and never null).
func ListInbox(_ context.Context, _ string, _ ListInboxInput) (ListInboxOutput, error) {
	return ListInboxOutput{Documents: []Document{}}, nil
}
