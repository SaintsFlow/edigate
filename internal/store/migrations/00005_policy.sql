-- +goose Up
-- Policies and approvals (wave 2-6): the forecast of a partner, the sum of orders accepted
-- automatically per partner per UTC day, the orders to suppliers an invoice is checked
-- against, and the approval requests that wait for an operator.

ALTER TABLE partners ADD COLUMN forecast_amount numeric(18, 2) NULL;

-- One row per partner and day. Reserve locks the row, so that orders that arrive at the
-- same time cannot together pass daily_auto_max.
CREATE TABLE daily_auto_totals (
    tenant_id  text NOT NULL,
    partner_id text NOT NULL,
    day        date NOT NULL,
    total      numeric(18, 2) NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, partner_id, day),
    FOREIGN KEY (tenant_id, partner_id) REFERENCES partners (tenant_id, partner_id)
);

CREATE TABLE supplier_orders (
    tenant_id    text NOT NULL,
    partner_id   text NOT NULL,
    order_number text NOT NULL,
    lines        jsonb NOT NULL,
    total        numeric(18, 2) NOT NULL,
    currency     char(3) NOT NULL,
    PRIMARY KEY (tenant_id, partner_id, order_number),
    FOREIGN KEY (tenant_id, partner_id) REFERENCES partners (tenant_id, partner_id)
);

-- kind "effect": the call waits for approval and then becomes an outbox row made from
-- args and effect_type. kind "review": a conflicting duplicate, no effect; the operator
-- only closes it. args holds the payload of the effect, which can carry the raw business
-- reference of the partner: it is never shown. view is what the operator sees: typed
-- values and the names of the rules, never the text of a note.
CREATE TABLE approvals (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    text NOT NULL REFERENCES tenants (id),
    idem_key     bytea NOT NULL REFERENCES idempotency_keys (key),
    kind         text NOT NULL CHECK (kind IN ('effect', 'review')),
    tool         text NOT NULL,
    effect_type  text NULL,
    args         jsonb NOT NULL,
    view         jsonb NOT NULL,
    rules        jsonb NOT NULL,
    tier         smallint NOT NULL CHECK (tier BETWEEN 0 AND 3),
    approvers    smallint NOT NULL CHECK (approvers IN (1, 2)),
    status       text NOT NULL CHECK (status IN ('pending', 'approved', 'rejected', 'expired')),
    requested_by text NOT NULL,
    decided_by   text[] NOT NULL DEFAULT '{}',
    requested_at timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    decided_at   timestamptz NULL,
    CHECK ((kind = 'effect') = (effect_type IS NOT NULL)),
    CHECK (NOT (requested_by = ANY (decided_by)))
);

CREATE INDEX approvals_tenant_status ON approvals (tenant_id, status);

-- A key has at most one open request of each kind. A closed request stays as history, so
-- a key that was rejected, expired and reopened can ask again.
CREATE UNIQUE INDEX approvals_one_open_per_key ON approvals (idem_key, kind) WHERE status = 'pending';
