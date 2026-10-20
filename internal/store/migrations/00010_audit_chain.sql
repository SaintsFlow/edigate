-- +goose Up
-- Audit chain: every new row of audit_log carries the hash of the row before it (prev_hash) and its own
-- hash (internal/audit). Signed anchors of the chain head are kept in audit_anchors, every 100 rows of
-- a tenant.
--
-- Rows written before this migration have no hash and the table refuses UPDATE, so they stay as they
-- are: the constraint is NOT VALID, which checks new rows only, and `edigatectl audit verify` starts
-- at the first row that has a hash. A database made with these migrations has no such rows.

ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_hash_set CHECK (hash IS NOT NULL AND prev_hash IS NOT NULL) NOT VALID;

CREATE INDEX audit_log_tenant_seq ON audit_log (tenant_id, seq);

CREATE TABLE audit_anchors (
    tenant_id  text NOT NULL,
    seq_upto   bigint NOT NULL,
    head_hash  bytea NOT NULL,
    signature  bytea NOT NULL,
    key_id     text NOT NULL,
    signed_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, seq_upto)
);

-- Anchors are append-only too.
CREATE TRIGGER audit_anchors_append_only
    BEFORE UPDATE OR DELETE ON audit_anchors
    FOR EACH ROW EXECUTE FUNCTION audit_log_forbid_change();

-- +goose Down
DROP TABLE audit_anchors;
DROP INDEX audit_log_tenant_seq;
ALTER TABLE audit_log DROP CONSTRAINT audit_log_hash_set;
