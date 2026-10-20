package store

import (
	"context"
	"errors"
)

// This file holds the queries of the upstream servers (wave 2-8): pinned manifests,
// references to quarantined results, mail templates.

// Manifest is the pin of one tool of an upstream server.
type Manifest struct {
	Server    string
	Tool      string
	SHA256    []byte // 32 bytes
	Signature []byte
	Lint      []byte // JSON array of findings
	PinnedBy  string
	PinnedAt  string // RFC 3339, set when read
}

// PinManifest writes or replaces the pin of a tool.
func (s *Store) PinManifest(ctx context.Context, m Manifest) error {
	lint := m.Lint
	if len(lint) == 0 {
		lint = []byte("[]")
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO manifests (server, tool, sha256, signature, lint, pinned_by) VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (server, tool) DO UPDATE
		   SET sha256 = EXCLUDED.sha256, signature = EXCLUDED.signature, lint = EXCLUDED.lint,
		       pinned_by = EXCLUDED.pinned_by, pinned_at = now()`,
		m.Server, m.Tool, m.SHA256, m.Signature, lint, m.PinnedBy)
	return err
}

// Manifests returns the pins of one server by tool name.
func (s *Store) Manifests(ctx context.Context, server string) (map[string]Manifest, error) {
	all, err := s.ListManifests(ctx, server)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Manifest, len(all))
	for _, m := range all {
		out[m.Tool] = m
	}
	return out, nil
}

// ListManifests returns the pins of a server (all servers when server is empty), ordered.
func (s *Store) ListManifests(ctx context.Context, server string) ([]Manifest, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT server, tool, sha256, signature, lint, pinned_by, to_char(pinned_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		 FROM manifests WHERE $1 = '' OR server = $1 ORDER BY server, tool`, server)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Manifest
	for rows.Next() {
		var m Manifest
		if err := rows.Scan(&m.Server, &m.Tool, &m.SHA256, &m.Signature, &m.Lint, &m.PinnedBy, &m.PinnedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SaveUpstreamNote keeps the text of a quarantined upstream result and returns its number;
// the agent gets the reference $up_<number>.
func (s *Store) SaveUpstreamNote(ctx context.Context, tenantID, server, tool, traceID, body, label string) (int64, error) {
	if tenantID == "" {
		return 0, errors.New("an upstream note needs a tenant")
	}
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO upstream_notes (tenant_id, server, tool, trace_id, body, taint_label) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		tenantID, server, tool, traceID, body, label).Scan(&id)
	return id, err
}

// HasEmailTemplate reports whether the tenant has this mail template.
func (s *Store) HasEmailTemplate(ctx context.Context, tenantID, templateID string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM email_templates WHERE tenant_id = $1 AND template_id = $2)`, tenantID, templateID).Scan(&ok)
	return ok, err
}
