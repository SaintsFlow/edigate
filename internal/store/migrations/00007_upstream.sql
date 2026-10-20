-- +goose Up
-- Upstream MCP servers, price list and e-mail (wave 2-8): the pinned tool manifests, the opaque
-- references to quarantined upstream results, and the mail templates of a tenant.

-- One row per pinned tool of an upstream server. sha256 is the hash of the canonical (RFC 8785)
-- description; signature is the ed25519 signature of the admin over the server name and that hash.
-- lint holds the findings of the description linter at the time of the pin.
CREATE TABLE manifests (
    server    text NOT NULL,
    tool      text NOT NULL,
    sha256    bytea NOT NULL CHECK (octet_length(sha256) = 32),
    signature bytea NOT NULL,
    lint      jsonb NOT NULL DEFAULT '[]',
    pinned_at timestamptz NOT NULL DEFAULT now(),
    pinned_by text NOT NULL,
    PRIMARY KEY (server, tool)
);

-- The text of an upstream result that was replaced by a reference ($up_<id>) because the
-- server is quarantined. The agent only holds the reference; no tool opens it.
CREATE TABLE upstream_notes (
    id          bigserial PRIMARY KEY,
    tenant_id   text NOT NULL REFERENCES tenants (id),
    server      text NOT NULL,
    tool        text NOT NULL,
    trace_id    text NOT NULL,
    body        text NOT NULL,
    taint_label text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- The mail templates a tenant may use (from the starting data).
CREATE TABLE email_templates (
    tenant_id   text NOT NULL REFERENCES tenants (id),
    template_id text NOT NULL,
    category    text NOT NULL,
    PRIMARY KEY (tenant_id, template_id)
);
