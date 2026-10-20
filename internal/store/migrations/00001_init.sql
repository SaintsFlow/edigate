-- +goose Up
-- Gateway core: tenants, principals (bearer tokens stored as SHA-256 only) and the audit log.

CREATE TABLE tenants (
    id   text PRIMARY KEY,
    name text NOT NULL
);

CREATE TABLE principals (
    id           text PRIMARY KEY,
    tenant_id    text NOT NULL REFERENCES tenants (id),
    kind         text NOT NULL CHECK (kind IN ('agent', 'operator', 'admin', 'partner')),
    partner_id   text NULL,
    token_sha256 bytea NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz NULL,
    CONSTRAINT principals_partner_id_iff_partner CHECK ((partner_id IS NOT NULL) = (kind = 'partner'))
);

CREATE TABLE audit_log (
    seq             bigserial PRIMARY KEY,
    tenant_id       text NOT NULL,
    at              timestamptz NOT NULL DEFAULT now(),
    event_type      text NOT NULL,
    trace_id        text NOT NULL,
    principal_id    text NOT NULL,
    tool            text NULL,
    idem_key        bytea NULL,
    control_numbers jsonb NULL,
    policy_explain  jsonb NULL,
    manifest_sha256 bytea NULL,
    payload_sha256  bytea NULL,
    outcome         text NULL,
    prev_hash       bytea NULL,
    hash            bytea NULL
);

CREATE INDEX audit_log_tenant_at ON audit_log (tenant_id, at);

-- The audit log is append-only: UPDATE and DELETE always fail.
-- +goose StatementBegin
CREATE FUNCTION audit_log_forbid_change() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only: % is not allowed', TG_OP;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER audit_log_append_only
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_forbid_change();
