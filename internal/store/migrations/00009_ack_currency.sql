-- +goose Up
-- Wave 2-9c. Three changes that the first measurement showed to be needed.
--
-- 1. The currency of the tenant: the last fallback for a document without CUR (the order of
--    fallbacks is CUR02, the currency of the partner, the currency of the tenant).
-- 2. The label of the price list of the tenant. erp.get_price_list returns it in every answer;
--    the benchmark puts its canary there.
-- 3. The 997 the gateway sends for every group of an incoming 850 or 810 (invariants I6, I7).
--    It is an outgoing document with control numbers and an MDN like the 855 and the 856, but it
--    answers no effect of an agent: it has no idempotency key and no saga. outbound_docs.st01 =
--    '997' tells it apart; nothing waits for a 997 on a 997, so it is delivered when its MDN counts.

ALTER TABLE tenants ADD COLUMN currency char(3) NOT NULL DEFAULT 'USD';
ALTER TABLE tenants ADD COLUMN price_list_id text NULL;

ALTER TABLE outbox ALTER COLUMN idem_key DROP NOT NULL;
ALTER TABLE outbound_docs ALTER COLUMN idem_key DROP NOT NULL;
ALTER TABLE outbound_docs DROP CONSTRAINT outbound_docs_st01_check;
ALTER TABLE outbound_docs ADD CONSTRAINT outbound_docs_st01_check CHECK (st01 IN ('855', '856', '997'));
