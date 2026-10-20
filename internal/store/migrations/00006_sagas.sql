-- +goose Up
-- Sagas and outgoing documents (wave 2-7): the order saga (create the order, confirm it with
-- an 855) and the shipment saga (reserve, ship, notify with an 856), the control numbers of
-- outgoing interchanges, and the record of what was sent to a partner and how it was acknowledged.

-- One saga per business key and kind. payload holds the order as the gateway built it (typed
-- values: lines with prices of the price list, total, currency, ship-to, order date); the
-- documents sent later are made from it and from the facts of the warehouse, never from the
-- arguments of the agent. source_document_id is the 850 the order came from.
CREATE TABLE sagas (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          text NOT NULL REFERENCES tenants (id),
    kind               text NOT NULL CHECK (kind IN ('order', 'shipment')),
    state              text NOT NULL DEFAULT 'running'
                       CHECK (state IN ('running', 'done', 'compensating', 'compensated', 'incident')),
    business_key       jsonb NOT NULL,
    payload            jsonb NOT NULL,
    source_document_id uuid NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, kind, business_key)
);

CREATE INDEX sagas_open ON sagas (state) WHERE state IN ('running', 'compensating');

-- idem_key is the key of the effect of the step (null until the step is called).
-- compensation_key is the key of its compensation: SHA-256 of the key and "cancel" or "release".
CREATE TABLE saga_steps (
    saga_id                uuid NOT NULL REFERENCES sagas (id),
    step_no                integer NOT NULL,
    name                   text NOT NULL,
    status                 text NOT NULL DEFAULT 'pending'
                           CHECK (status IN ('pending', 'done', 'failed', 'compensated', 'skipped')),
    idem_key               bytea NULL REFERENCES idempotency_keys (key),
    outbox_id              bigint NULL REFERENCES outbox (id),
    compensation_key       bytea NULL REFERENCES idempotency_keys (key),
    compensation_outbox_id bigint NULL REFERENCES outbox (id),
    PRIMARY KEY (saga_id, step_no)
);

CREATE INDEX saga_steps_idem_key ON saga_steps (idem_key);
CREATE INDEX saga_steps_compensation_key ON saga_steps (compensation_key);

-- last_value is the last number given out. ISA13 has nine digits, GS06 and ST02 are
-- taken from the same kind of counter, one counter per kind for each tenant and partner.
CREATE TABLE control_numbers (
    tenant_id  text NOT NULL,
    partner_id text NOT NULL,
    kind       text NOT NULL CHECK (kind IN ('isa', 'gs', 'st')),
    last_value bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, partner_id, kind),
    FOREIGN KEY (tenant_id, partner_id) REFERENCES partners (tenant_id, partner_id)
);

-- One row per document sent to a partner. The raw bytes are in edi_interchanges (direction
-- out) and in the payload of the outbox row; here are the numbers, the receipts and the state:
-- queued (the outbox row waits), sent (an MDN was counted), delivered (MDN and a 997 with
-- AK5 A or E), incident (a person decides).
CREATE TABLE outbound_docs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       text NOT NULL,
    partner_id      text NOT NULL,
    saga_id         uuid NULL REFERENCES sagas (id),
    step_no         integer NULL,
    outbox_id       bigint NOT NULL REFERENCES outbox (id),
    idem_key        bytea NOT NULL REFERENCES idempotency_keys (key),
    st01            text NOT NULL CHECK (st01 IN ('855', '856')),
    isa13           char(9) NOT NULL,
    gs06            text NOT NULL,
    st02            text NOT NULL,
    message_id      text NOT NULL,
    mic_sha256      bytea NOT NULL,
    mdn_at          timestamptz NULL,
    mdn_ok          boolean NULL,
    ack_status      char(1) NULL,
    ack_at          timestamptz NULL,
    transmissions   integer NOT NULL DEFAULT 0,
    state           text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'sent', 'delivered', 'incident')),
    incident_reason text NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, partner_id, gs06, st02),
    UNIQUE (tenant_id, partner_id, isa13),
    FOREIGN KEY (tenant_id, partner_id) REFERENCES partners (tenant_id, partner_id)
);

CREATE INDEX outbound_docs_idem_key ON outbound_docs (idem_key);
CREATE INDEX outbound_docs_sent ON outbound_docs (state, mdn_at) WHERE state = 'sent';
