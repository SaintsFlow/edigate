-- +goose Up
-- EDI intake: the gateway's interchange id per tenant, partners, interchanges
-- (raw bytes, MDN), documents (typed body only), and rejected interchanges.

ALTER TABLE tenants ADD COLUMN interchange_id text NULL UNIQUE;

CREATE TABLE partners (
    tenant_id       text NOT NULL REFERENCES tenants (id),
    partner_id      text NOT NULL,
    role            text NOT NULL CHECK (role IN ('customer', 'supplier')),
    name            text NOT NULL,
    active          boolean NOT NULL DEFAULT true,
    auto_max_amount numeric(18, 2) NULL,
    daily_auto_max  numeric(18, 2) NULL,
    currency        char(3) NULL,
    PRIMARY KEY (tenant_id, partner_id)
);

-- A partner token must point at a registered partner. The key is not enforced
-- while partner_id is null (every other kind of principal).
ALTER TABLE principals
    ADD CONSTRAINT principals_partner_fk
    FOREIGN KEY (tenant_id, partner_id) REFERENCES partners (tenant_id, partner_id);

CREATE TABLE edi_interchanges (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       text NOT NULL REFERENCES tenants (id),
    direction       text NOT NULL CHECK (direction IN ('in', 'out')),
    counterparty_id text NOT NULL,
    isa13           char(9) NOT NULL,
    message_id      text NOT NULL,
    raw             bytea NOT NULL,
    raw_sha256      bytea NOT NULL,
    mic_sha256      bytea NOT NULL,
    mdn             jsonb NULL,
    received_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, direction, counterparty_id, isa13)
);

CREATE TABLE edi_documents (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    interchange_id uuid NOT NULL REFERENCES edi_interchanges (id),
    tenant_id    text NOT NULL,
    partner_id   text NOT NULL,
    gs06         text NOT NULL,
    st01         text NOT NULL,
    st02         text NOT NULL,
    business_ref text NULL,
    body         jsonb NULL,
    status       text NOT NULL CHECK (status IN ('received', 'processed', 'rejected')),
    errors       jsonb NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (interchange_id, gs06, st02)
);

CREATE INDEX edi_documents_tenant_status_created ON edi_documents (tenant_id, status, created_at);

CREATE TABLE edi_rejected (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   text NOT NULL,
    partner_id  text NOT NULL,
    isa13       char(9) NULL,
    raw         bytea NOT NULL,
    raw_sha256  bytea NOT NULL,
    reason      text NOT NULL CHECK (reason IN ('isa13_conflict', 'envelope_error')),
    detail      text NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX edi_rejected_tenant_received ON edi_rejected (tenant_id, received_at);
