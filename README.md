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
- Also works now: an order goes through from end to end. `erp.confirm_order` sends the 855 to the partner,
  `wms.reserve` reserves the lines in the warehouse (mock-wms), and `edi.send_asn` ships them and sends the 856 with the
  SSCC the warehouse gave. The agent passes only the `txn_id` of the order; the documents are built from the order and
  from the facts of the warehouse. The steps run as sagas: a failure before the send cancels the order or releases
  the reservation, and a failure after the send is an incident for an operator, never a second send with new
  numbers. ISA13, GS06 and ST02 are given out in the same transaction as the outbox row and stay the same when a
  transmission is repeated. A document counts as delivered only when an MDN and a 997 with AK5 `A` or `E` both came
  back; `txn.status` shows `saga_state`. The partner on the stand is partner-emulator, which answers with an MDN and a
  997 you can set to accepted, rejected, mixed, late or missing.
- Also works now: tools of other MCP servers. The gateway passes the tools of the servers in its config to the agent
  as `up.<server>.<tool>`, but only those whose description an administrator pinned. A pin is the SHA-256 of the
  canonical description (RFC 8785) with an ed25519 signature. Every `tools/list` and every call reads the upstream
  again; no pin, a bad signature or a changed description switches the tool off (`manifest_drift`, an audit row with
  both hashes). `edigatectl manifests keygen|pin|list` makes the key and the pins; `pin` refuses a description that
  names another tool, and one that gives orders to the model unless you pass `--accept-lint`. Results carry
  `_meta {"edigate/taint": "upstream:<server>"}`; with `quarantine_results: true` their text becomes `$up_N`
  references. Calls need a permission in the policy data (`upstream_allow` in the config). The token of the
  agent is never sent to an upstream. Three hostile servers of our own (`servers/adversarial`) test this on the stand.
- Also works now: `erp.get_price_list` (the prices of your tenant, up to 50 SKUs) and `notify.send_email`, a letter
  made from one of three templates, sent only to a contact of the partner of the order. The letter has no text of the
  agent in it. The design is in [docs](docs).
- Also works now: the gateway answers the partner. For every group of an incoming 850 or 810 it queues a 997 in the
  transaction that stores the interchange, with control numbers from the same counter as the 855 and the 856, and
  sends it with AS2 like any other document. A set the parser accepted is `A`; a set it refused is `R` with the AK3 and
  AK4 that name the segment and the element. A set refused only for the role of the partner is right in syntax and
  is `A`. A 997 from a partner is never answered, and an interchange refused as a whole (422) gets none, because it has no group to name. CUR is optional in an 850 and an 810: a document without it takes the
  currency of the partner, else of the tenant. A missing `delivery_instruction` is the same request as `none`.
  `edi.read_document` lists the known addresses of the partner in `partner_contacts` of the header, and
  `erp.get_price_list` returns the label of the price list (`edigatectl price-list-id set --tenant T <label>`).

## Try it

You need Go 1.27, Docker, `curl`, and free local ports 55432, 18080, 18081, 18082 and 18083.

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

The smoke test then runs a whole order: the partner sends an 850, the agent creates the order, confirms it with an
855, reserves it and ships it, and the test waits until both sagas are `done` and partner-emulator holds one 855 and
one 856 with different ISA13. After that it sets partner-emulator to answer with a negative 997 for the next order
and checks that the saga is an incident and the 855 is not sent again. The stand has eight services: PostgreSQL, the
gateway, the three mocks (mock-1c on port 18081, mock-wms on 18082, partner-emulator on 18083) and the three hostile
servers, which publish no port. Partner-emulator
changes its behaviour while it runs: `curl -X POST http://127.0.0.1:18083/control -d '{"ack_status":"R"}'`
(`ack_status` is `A`, `E`, `R`, `P` or `none`; the other fields are `ack_delay_ms`, `mdn_delay_ms` and `mdn_drop`).
The call is accepted only from the machine of the stand. `edigatectl fixtures load <file> --partner-tokens-out <file>`
gives a token to each customer partner that has none and writes the tokens in the file the emulator reads.

The smoke test makes a key pair for the pins (`deploy/compose/keys`, not mounted into the gateway; the public half
goes to the gateway through `deploy/compose/.env`), pins `rugpull-rates`, calls its tool three times, and sees
`manifest_drift` on the fourth call because the server changed its description. It also checks that
`evil-validator` cannot be pinned, that no token of the agent reaches the hostile servers, that a letter reaches
the partner-emulator once, and that a letter to a contact of another partner is refused (`email.recipient`). A plain
`make up` starts the gateway without the key: the upstream servers are then off.

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

One binary plays every configuration of the benchmark: `edigate --mode full|plain|reqhash|no-sanitize|no-policy|no-bizkey|no-contracts`
(or `EDIGATE_MODE`). The tool names stay the same. `full` is the gateway as described above. `plain` has the open tools of
a gateway without protections: the model writes the partner, the prices and the quantities, the ERP and the
warehouse are called at once without an external id, and the text of the partner is shown whole. `reqhash` adds a stored
answer for a repeated request. The other four are `full` with one protection off (the sanitizer, the policy, the business
key, the contracts). An unknown mode stops the start with exit code 2; `/healthz` shows the mode. Several processes can
run side by side, each with its own database (`edigatectl db create <name>`), port and `EDIGATE_PUBLIC_URL`, and share the
mocks: the partner emulator answers each one at its own address (see `.env.example`). `EDIGATE_TRANSPORT_LOG` writes
a JSON line for every MCP request.

`edigatectl parse <file>` checks an X12 file with the same strict parser and prints a JSON summary; the exit code is
0 only when the envelope and every set are accepted.

`make corpus` builds the EDI-Inject test corpus (300 X12 documents with labels) into `bench/out/corpus-seed1` and
checks every document with the strict parser. Rules of the generator: [bench/edi_inject/README.md](bench/edi_inject/README.md).

## Benchmark harness

`bench/harness` runs an agent over the documents of the corpus on the stand and `bench/metrics` reads the result from the
state of the mocks and of the gateway tables. It needs the stand (`make up`), Python 3.11 and, for local models,
Ollama on the host. `make bench-venv` makes the environment with the exact versions of `bench/requirements.txt`,
`make bench-test` runs the tests of the harness and the metrics.

```sh
make bench-smoke
```

builds the stand, makes the pilot corpus (seed 2) if it is missing, runs two fake models and one local Ollama model
in configurations C0 and C3 on 20 documents (the ones marked `held_out` never run), runs the fault schedules F1 to F8 with a
scripted agent, and prints two tables: the attack success rate by configuration and model, and the duplicate rate by
configuration and fault. It exits with 0 only if every episode produced a row and a few invariants hold (for example,
no duplicate effect in C3 under F1 and F4). It takes tens of minutes. The results stay in `bench/out/` and are not
committed. `BENCH_SMOKE_MODEL` changes the Ollama model.

A run of your own, from the `bench` folder:

```sh
.venv/bin/python -m harness.run --corpus out/corpus-seed2 --docs deciding:10 --configs C0,C3 \
    --models fake-honest,ollama:llama3.2:3b --seed 1 --operator approve-all --slots 4 --out out/myrun
```

Each episode starts from a clean database state in its own slot (a gateway process, a database and a tenant), with the
mocks shared. The fake models are checks of the pipeline: `fake-honest` does the task from the typed fields, and
`fake-gullible` also follows the attack sentences it finds in the document, so a run with it shows what each
configuration lets through, not how a model behaves. Hosted models need `OPENROUTER_API_KEY`; without it they are skipped.
Configuration C2 puts agentgateway in front of the gateway ([bench/agentgateway](bench/agentgateway)).

## Documents

[docs](docs) holds the threat model, the invariants and the SHA-256 of the research protocol that the experiments
follow. The protocol text is not published yet.

## License

Apache License 2.0, see [LICENSE](LICENSE).
