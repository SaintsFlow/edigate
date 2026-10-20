-- +goose Up
-- Modes of the benchmark (wave 2-9a). The open tools of the modes plain (C0) and reqhash (C1)
-- keep no key, no outbox and no saga. They write one light row for every call that did its
-- effect: the answer they gave (txn.status reads it) and the arguments (a later tool finds the
-- order by them). req_key is the key of the mode reqhash: SHA-256 of the tool and the
-- canonical arguments; a second call with the same key gets result and calls nothing.

CREATE TABLE plain_txns (
    txn_id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  text NOT NULL REFERENCES tenants (id),
    tool       text NOT NULL,
    args       jsonb NOT NULL,
    result     jsonb NOT NULL,
    req_key    bytea NULL CHECK (req_key IS NULL OR octet_length(req_key) = 32),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX plain_txns_req_key ON plain_txns (tenant_id, req_key) WHERE req_key IS NOT NULL;
CREATE INDEX plain_txns_tool ON plain_txns (tenant_id, tool, created_at);
