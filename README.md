# edigate

edigate sits between LLM agents that speak MCP (Model Context Protocol) and trading partners that send X12 EDI
documents such as purchase orders and invoices. It shows the agent each document without the partner's free text and
lets the agent create business effects, such as a sales order, only through checked tools.

Status: work in progress, v0.1 is not released.

- Works now: the MCP endpoint with bearer tokens per agent, tenants, an append-only audit log; EDI intake on
  `POST /edi/inbound` (strict X12 004010 parser for 850, 810 and 997, a receipt (MDN) per interchange, replay and
  conflict checks by ISA13, tokens per trading partner); a white list of elements with a format for each (a value
  that fails its format, and every free text, becomes a note such as `$note_3`); and three tools:
  `edi.list_inbox` and `edi.read_document` show an agent the typed fields of a document and none of the partner's
  text, and `edi.extract_note` reads one note through a separate model call without tools and returns only the
  values of a closed schema. A note reference in the arguments of a tool with an effect is refused (`tainted_sink`).
- Also works now: the first tool with an effect, `erp.create_sales_order`, and `txn.status`. The agent passes the id of
  a received 850 and gets a `txn_id`; the price of every line comes from the price list, not from the 850. The key is
  built from the tenant, the tool, the partner and BEG03, so the same order that arrives again in a new exchange is
  the same order. A repeated call gets the same `txn_id`; the same BEG03 with another content is a
  `conflicting_duplicate`. The key, the answer and an outbox row are written in one transaction, and a relay delivers
  the outbox to the ERP (mock-1c on the stand) with retries and an idempotent receiver, so a crash of the gateway or a
  lost answer of the ERP does not make a second order. `make chaos-dup` tests this (see below).
- Also works now: business policies. Every call with an effect goes through an embedded OPA policy
  (`policies/edigate.rego`, tests in `policies/edigate_test.rego`, run by `make policy-test`). It refuses an unknown
  or inactive partner, a wrong role, a ship-to that is not one of the partner's codes, and a supplier invoice
  without a matching order (`policy_denied`, with the rule names in `data.explain`). An order above the limit of a
  document, or above the limit of a partner for the day (UTC), is not refused: the answer is `pending_approval` and
  an operator decides. A supplier invoice, `erp.post_vendor_invoice`, always needs two different operators. The
  operator sees typed values and rule names, never the text of a note, over `/ops/` or with
  `edigatectl approvals list|approve|reject`. A request that nobody decides in 30 minutes expires.
- Not yet: sagas, outgoing documents (855, 856), the other tools of risk level 2 (`erp.confirm_order`,
  `edi.send_asn`, `notify.send_email`; their rules are written and tested, the tools are not) and the 997 and MDN
  receipts. The design is in [docs](docs).

## Try it

You need Go 1.27, Docker, `curl`, and free local ports 55432, 18080 and 18081.

```sh
make smoke
```

This builds the binaries, starts PostgreSQL and the gateway in Docker, creates a tenant, an agent and a trading
partner, and checks that an agent gets an empty inbox, that bad requests get the right errors, that the audit log
records the calls, and that a partner can send a purchase order, get a receipt, and see the agent read it without
the partner's free text. If Ollama runs on the host (port 11434, model `llama3.2:3b`), it also extracts a
delivery instruction from a note; otherwise it prints `skip: n (ollama not reachable)` for that step. It prints
`smoke ok` and exits with code 0 when everything matches. The stand keeps running afterwards; `make down` stops
it and deletes its data.

The smoke test also loads a small price list (`edigatectl fixtures load <file>`; the corpus generator writes a file
of the same format), creates an order from the received 850, repeats the call, waits until `txn.status` says
`completed` and checks the order in mock-1c.

`make chaos-dup` is the test of the one-order guarantee. It starts a stand with fault injection (settings in
`deploy/compose/chaos.yml`, test only): eight gateway processes that exit at random, and a mock-1c that sometimes saves
an order and then answers 500. It sends 50 purchase orders, creates an order for each, repeats every call after any
error, and keeps repeating until Docker has restarted the gateway at least 100 times. It exits with code 0 only if
mock-1c holds exactly 50 orders, one per document, and each document got one `txn_id` in the whole run. It takes
several minutes and puts the normal stand back at the end.

An operator reopens a failed order key over HTTP, not in the database:
`edigatectl txn reopen --url http://127.0.0.1:18080 --token <operator token> <key-hex>`.

The smoke test also sends an order above the limit and approves it as an operator, sends an order with an unknown
ship-to and sees `policy_denied`, and books a supplier invoice with two different operators. By hand:

```sh
bin/edigatectl approvals list --url http://127.0.0.1:18080 --token <operator token>
bin/edigatectl approvals approve --url http://127.0.0.1:18080 --token <operator token> <approval id>
```

The same operator cannot approve twice, and an operator sees only the requests of their own tenant. Limits, ship-to
codes, the forecast amount and the orders to suppliers come from the fixtures file (`edigatectl fixtures load`); a
limit that is not set means that nothing goes through without an operator.

`make test` runs the policy tests, `go vet`, the tests with the race detector and `govulncheck`. It starts PostgreSQL in Docker and
downloads govulncheck on the first run. `make fuzz` fuzzes the X12 parser for 60 seconds. A test against a real
local model runs with
`EDIGATE_TEST_OLLAMA_URL=http://127.0.0.1:11434/v1 go test -v -run Live ./internal/quarantine/...` and is skipped
without that variable.

The quarantined reader is configured with `EDIGATE_QUARANTINE_BASE_URL` (any OpenAI-compatible API),
`EDIGATE_QUARANTINE_MODEL` and, if the API needs one, `EDIGATE_QUARANTINE_API_KEY`. An empty model switches
`edi.extract_note` off.

`edigatectl parse <file>` checks an X12 file with the same strict parser and prints a JSON summary; the exit code is
0 only when the envelope and every set are accepted.

`make corpus` builds the EDI-Inject test corpus (300 X12 documents with labels) into `bench/out/corpus-seed1` and
checks every document with the strict parser. Rules of the generator: [bench/edi_inject/README.md](bench/edi_inject/README.md).

## Documents

[docs](docs) holds the threat model, the invariants and the SHA-256 of the research protocol that the experiments
follow. The protocol text is not published yet.

## License

Apache License 2.0, see [LICENSE](LICENSE).
