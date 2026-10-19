# Changelog

## v0.1.0 (2026-10-19)

First release: a research prototype of the gateway, with its stand and its experiment kit.

- MCP endpoint (Streamable HTTP, stateless) with bearer tokens per agent, tenants, roles for agent, operator, admin
  and partner, rate limits, checks of the MCP headers against the body, and one error format.
- EDI intake on `POST /edi/inbound`: strict X12 004010 parser for 850, 810 and 997, receipts (MDN), replay and
  conflict checks by ISA13, tokens per trading partner, a 997 for every incoming group.
- A sanitizer with a white list of elements and their formats. Free text and values that fail a format become notes
  with references. `edi.list_inbox` and `edi.read_document` show typed fields only. `edi.extract_note` reads a
  note through a separate model call into a closed schema. A note reference in the arguments of a tool with an effect
  is refused (`tainted_sink`).
- One effect per business key: `erp.create_sales_order` and `txn.status`, keys built from business identifiers, the
  key, the answer and the outbox row in one transaction, a relay with retries and an idempotent receiver, and
  `edigatectl txn reopen` for a failed key.
- Business policy in embedded OPA with explanations, risk tiers, limits per document and per partner per day,
  approval requests for operators, two different operators for a supplier invoice, and
  `erp.post_vendor_invoice`.
- Sagas for the order and the shipment: `erp.confirm_order` (855), `wms.reserve`, `edi.send_asn` (856 with SSCC),
  compensations before the send and incidents after it, control numbers fixed with the outbox row, delivery counted
  only when both the MDN and a positive 997 came back.
- `erp.get_price_list` and `notify.send_email` (three templates, contacts of the partner only).
- Tools of other MCP servers as `up.<server>.<tool>`: pinned descriptions with ed25519 signatures, a check at every
  list and call, a linter for descriptions, taint labels on results, and three hostile servers of our own for tests.
- Audit log that shows tampering, within limits: a hash chain per tenant, signed anchors every 100 rows in the
  database and in a file outside it, an append-only trigger, and `edigatectl audit keygen` and `edigatectl audit
  verify`. Rows are protected once a signed anchor covers them. That needs the signing key and an anchor folder that an
  attacker with database rights cannot write to. Without a key there is a chain but no anchors. The rows after the last
  anchor (up to 99) can be recomputed by someone with database rights.
- Seven modes of one binary (`full`, `plain`, `reqhash`, `no-sanitize`, `no-policy`, `no-bizkey`, `no-contracts`) for
  the benchmark.
- The stand in Docker Compose with mock-1c, mock-wms and partner-emulator, and the commands `make demo` (three
  scenarios), `make smoke`, `make chaos-dup`, `make test`, `make fuzz`.
- The EDI-Inject corpus generator and the benchmark harness and metrics (`bench/`).
- Threat model and invariants in `docs/`, `SECURITY.md`.

Known limits: AS2 is emulated without signatures, X12 004010 only, synthetic data, mock ERP and warehouse, no failover.
See the README.
