-- +goose Up
-- Effects with a guarantee of at most one per business key (wave 2-5): idempotency keys,
-- the outbox the relay delivers, the price list the order prices come from, the
-- internal business key of a document, and the contact and ship-to tables of partners.

-- bk_ref is the raw business reference (BEG03 of an 850, BIG02 of an 810), never shown
-- to the agent: the body shows a note reference when the value failed its format.
ALTER TABLE edi_documents ADD COLUMN bk_ref text NULL;
UPDATE edi_documents SET bk_ref = business_ref
 WHERE bk_ref IS NULL AND business_ref IS NOT NULL AND business_ref NOT LIKE '$note\_%';

-- audit_log gets a free place for details of an event (for example both request hashes
-- of a conflicting duplicate). Updates and deletes stay forbidden by the trigger.
ALTER TABLE audit_log ADD COLUMN detail jsonb NULL;

CREATE TABLE idempotency_keys (
    key           bytea PRIMARY KEY CHECK (octet_length(key) = 32),
    tenant_id     text NOT NULL REFERENCES tenants (id),
    tool          text NOT NULL,
    business_key  jsonb NOT NULL,
    req_hash      bytea NOT NULL,
    state         text NOT NULL CHECK (state IN ('reserved', 'completed', 'failed', 'reopened')),
    txn_id        uuid NOT NULL UNIQUE,
    result        jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    completed_at  timestamptz NULL
);

CREATE TABLE outbox (
    id              bigserial PRIMARY KEY,
    tenant_id       text NOT NULL REFERENCES tenants (id),
    idem_key        bytea NOT NULL REFERENCES idempotency_keys (key),
    effect_type     text NOT NULL,
    payload         jsonb NOT NULL,
    status          text NOT NULL CHECK (status IN ('pending', 'sent', 'acked', 'failed', 'dead')),
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    external_ref    text NULL,
    last_error      text NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX outbox_status_next ON outbox (status, next_attempt_at);

-- A key has at most one live outbox row (I3).
CREATE UNIQUE INDEX outbox_one_live_per_key ON outbox (idem_key) WHERE status IN ('pending', 'sent');

CREATE TABLE price_list (
    tenant_id  text NOT NULL REFERENCES tenants (id),
    sku        text NOT NULL,
    unit_price numeric(18, 2) NOT NULL,
    currency   char(3) NOT NULL,
    unit       char(2) NOT NULL,
    PRIMARY KEY (tenant_id, sku)
);

CREATE TABLE partner_contacts (
    tenant_id  text NOT NULL,
    partner_id text NOT NULL,
    email      text NOT NULL,
    PRIMARY KEY (tenant_id, partner_id, email),
    FOREIGN KEY (tenant_id, partner_id) REFERENCES partners (tenant_id, partner_id)
);

CREATE TABLE partner_ship_to (
    tenant_id  text NOT NULL,
    partner_id text NOT NULL,
    code       text NOT NULL,
    PRIMARY KEY (tenant_id, partner_id, code),
    FOREIGN KEY (tenant_id, partner_id) REFERENCES partners (tenant_id, partner_id)
);
