# edigate

edigate is a gateway between LLM agents that speak MCP (Model Context Protocol) and trading partners that send X12
EDI documents such as purchase orders and invoices. It is for engineers who want an agent to work with business
documents without trusting the text inside them. The agent sees typed fields, never the partner's free text, and it
can cause an effect, such as a sales order or a shipment notice, only through checked tools. Short values that pass
the format of their field still reach the agent; see [What it does not do](#what-it-does-not-do).

Version 0.1.0 is a research prototype. It runs on a local stand with simulated partners, a simulated ERP and a
simulated warehouse. It is not a product and it is not ready for production traffic. The list of what is missing is
below.

## What it does

- **Reads EDI without handing the text to the agent.** Partners post X12 004010 interchanges (850 purchase orders,
  810 invoices, 997 acknowledgements) to `POST /edi/inbound`. A strict parser checks the envelope and the sets. A white
  list names the elements the agent may see, each with a format. Every free text and every value that fails its format
  becomes a note with a reference such as `$note_3`. The agent gets the reference, not the text.
- **Reads a note only through a closed schema.** `edi.extract_note` passes one note to a separate model call without
  tools. The call must answer with a value from a small list (for example a delivery instruction). Anything else is
  refused.
- **Refuses references where effects happen.** A note reference in the arguments of a tool with an effect is refused
  with `tainted_sink`.
- **Makes one effect per business key.** The key comes from the tenant, the tool, the partner and the business number
  of the document (BEG03, the order number), never from the model. A repeated call, a document that arrives again in a
  new exchange and a crash in the middle of delivery all end in one order, as long as the partner reuses the same
  BEG03 and the ERP accepts an external id only once (see the limits below). The key, the answer and an outbox row are
  written in one transaction; a relay delivers to the ERP.
- **Applies policy.** An embedded policy in OPA (Open Policy Agent) refuses an unknown partner, a wrong role, an unknown ship-to code and a
  supplier invoice without a matching order. Orders above a limit wait for an operator. A supplier invoice always
  needs two different operators.
- **Runs the order through to the partner.** `erp.confirm_order` sends an 855 (order acknowledgement),
  `wms.reserve` reserves stock and `edi.send_asn` ships and sends an 856 (ship notice) with the SSCC (shipping
  container code) from the warehouse. The steps are sagas with compensations. A document counts as delivered only
  when both a receipt (MDN, message disposition notification) and a 997 (functional acknowledgement) with AK5 `A`
  (accepted) or `E` (accepted with errors) came back. Control numbers such as ISA13 (the interchange number) are
  fixed in the same transaction as the outbox row and never change when a send is repeated.
- **Answers the partner.** Every incoming group gets a 997.
- **Passes tools of other MCP servers with pins.** Tools of the servers in the config appear as `up.<server>.<tool>`
  only after an administrator pinned their description (SHA-256 of the canonical text with an ed25519 signature). A
  changed description switches the tool off; a change of behaviour behind the same description is not caught (see
  the limits). Results carry a taint label.
- **Keeps a log that shows tampering, within limits.** Audit rows form a hash chain per tenant, and a signed anchor
  is made every 100 rows. A row is protected once a signed anchor covers it. That needs the signing key
  (`EDIGATE_AUDIT_SIGNING_KEY`) and an anchor folder (`EDIGATE_AUDIT_ANCHOR_DIR`) that an attacker with database
  rights cannot write to. Without a key the chain is written but there are no anchors. The rows after the last
  anchor, up to 99, can be recomputed by someone with database rights. `edigatectl audit verify` finds a changed row,
  a removed row and a cut tail inside the anchored part.

## What it does not do

- **AS2 is emulated, without signatures or encryption.** A bearer token per partner stands in for the certificate. It
  must not leak, and the channel must use TLS (the stand does not). The receipt (MDN) is a JSON object, not a signed
  MIME receipt, so it does not prove receipt to a third party.
- **X12 004010 only.** No EDIFACT, no other X12 versions, no 999.
- **Synthetic data only.** Partner identifiers carry the prefix `ZZTEST`. Real partners with odd separators or
  retailer-specific guides are not covered. The strict parser refuses text outside printable ASCII.
- **The ERP and the warehouse are mocks.** "1C" is an ERP system common in the CIS; mock-1c models only its
  idempotent receiver and accepts an external id once. A real service has to behave the same way, or the one-order
  guarantee does not hold. The guarantee also holds only for the same BEG03: the same order under another number is a
  new order.
- **Actions without a source document are not supported.** Every tool with an effect goes back to a received document.
- **No failover or distributed deployment.** Several gateway processes on one database are tested (`make chaos-dup`).
  Failover and running it as a cluster are not studied, in line with the "single gateway instance" assumption of the
  threat model.
- **Detection takes time.** A missing MDN is noticed after 30 minutes, a missing 997 after 24 hours, and a changed
  description of an upstream tool at the next tool list.
- **Risks that remain, named in the threat model** (the full list is in section 7 of
  [docs/threat-model.md](docs/threat-model.md)):
  - values in typed fields that fit their format, including a short instruction that fits a format;
  - steering inside the closed schema of the note reader;
  - the same order sent again under another number;
  - an upstream result that the model restates in its own words;
  - data leaving through the arguments of an `up.*` tool (TL-07): the data is not tainted, so only the choice of
    allowed upstream servers and tools holds it back;
  - an upstream server that changes its behaviour behind an unchanged description (TL-03): the pin checks the
    description, not what the tool does;
  - a harmful action that stays inside the limits (a captured agent, an operator who approves a harmful request that
    looks normal);
  - a poisoned description that an administrator pinned.
- **The audit log has gaps by design.** Rows after the last anchor, up to 99, can be recomputed by someone with
  database rights. Cutting the newest rows together with their anchors in the database shows only against the anchor
  file, and that file must be out of the attacker's reach.

Status of the controls: in v0.1.0 the mechanisms are built and covered by unit and integration tests named as in the
registry of the threat model. The tests named `T-BENCH` belong to the experiments and have not been run with
conclusions in this release. `make test` runs the others; the repository has no CI.

## Try it

You need Go 1.27, Docker, `curl` and free local ports 55432, 18080, 18081, 18082 and 18083. Ollama on the host (port
11434, model `llama3.2:3b`) is optional.

```sh
make demo
```

The command builds the binaries, wipes the stand, builds it again and runs three scenarios. The first run takes longer
because it builds the images. Each step prints one line and checks itself. A good run prints a line that starts with
`demo ok` and exits with code 0; a bad one prints `FAIL` and what did not match.

- **S1, an injection that reaches no effect.** An 850 carries instructions in a note field and in a business number
  that fails its format. The agent sees references only. The order follows the price list and the ship-to code of the
  document. A letter to a foreign address is refused by policy, and a note reference in the address of a letter is
  `tainted_sink`. One step reads a delivery note through the model and prints `skip` when Ollama is missing.
- **S2, a saga with receipts.** Order, 855, reservation, shipment, 856. Both sagas end `done`, and both documents have
  an MDN and a positive 997.
- **S3, faults without duplicates.** The same order in a new exchange gives one order, and so does a repeated call
  (a lost answer is simulated by calling twice; real lost answers are in `make chaos-dup`). A negative 997 turns the
  saga into an incident and no second 855 goes out. For the audit log the demo switches the trigger off with the rights
  of the database owner, as an attacker with such rights would, and changes a row. `audit verify` names it, and the log
  verifies again when the row is put back.

The stand stays up afterwards; `make down` removes it with its data. [docs/demo.md](docs/demo.md) is a walkthrough of
the demo for recording it on screen.

## How it works

```
 partner ──X12──▶ /edi/inbound ─▶ strict parser ─▶ sanitizer ─▶ documents + notes ─┐
    ▲                                                                            │
    │ 855, 856, 997, MDN                                                         ▼
 partner-emulator ◀── relay ◀── outbox ◀── transaction ◀── policy ◀── tools ◀── /mcp ◀── agent
                       │                     (key, answer, audit row)                      │
                       ├──▶ mock-1c, mock-wms                         quarantined note reader
                       └──▶ AS2 emulation                             upstream MCP servers (pinned)
```

One Go binary, `edigate`, serves `/mcp` (MCP over HTTP, stateless), `/edi/inbound` (partners), `/ops/` (operators) and
`/healthz`. It also runs the relay, the saga runner and the audit anchor writer. Everything is stored in PostgreSQL.

- `internal/edge`: bearer tokens, rate limits, header and body checks, one error format.
- `internal/edi`, `internal/sanitize`, `internal/inbound`: parser, white list and intake.
- `internal/contracts`: JSON Schema for the input and output of each tool.
- `internal/tools`, `internal/mcpserver`: the tools, wrapped once: schema, taint check, service, output check, audit.
- `internal/policy`, `internal/approval`: OPA rules and approval requests.
- `internal/txn`: keys, outbox, relay and sagas. `internal/adapters`: clients of the ERP, the warehouse and the partner.
- `internal/quarantine`: the note reader. `internal/upstream`: pinned upstream tools.
- `internal/audit`, `internal/store`: the hash chain, the anchors and every SQL query.
- `mocks/`: mock-1c, mock-wms and partner-emulator. `servers/adversarial/`: three hostile MCP servers of our own.

## Tools for the agent

Tiers: T0 reads, T1 is a reversible write after policy, T2 is irreversible and checked against the order, T3 is
booking a supplier invoice (the system makes no payments) and needs two operators. Every tool takes a closed JSON
object; extra fields are refused. The last column lists what the agent chooses itself; everything else is taken from
the stored document, the price list or the warehouse.

| Tool | Tier | What it does | What the agent chooses, and what is refused |
|---|---|---|---|
| `edi.list_inbox` | T0 | Lists received 850 and 810 documents of the tenant. | An optional status and a limit. Nothing else. |
| `edi.read_document` | T0 | Typed fields of one document, notes as references, known contacts of the partner. | A document id. A document of another tenant looks missing. |
| `edi.extract_note` | T0 | Reads one note through the quarantined model into a closed schema. | A note reference and a schema: `delivery_instruction`, `packaging` or `contact_request`. |
| `txn.status` | T0 | State of a transaction, its external reference and the state of its saga. | A `txn_id`. |
| `erp.get_price_list` | T0 | Prices of the tenant for up to 50 SKUs. | Up to 50 SKUs. |
| `erp.create_sales_order` | T1 | Creates an order from a received 850. Prices come from the price list. | A document id and an optional `delivery_instruction` (a fixed list). The value goes into the order, so this is the room for steering inside a closed schema. Prices, quantities, addresses and the partner are refused. |
| `wms.reserve` | T1 | Reserves the lines of an order in the warehouse. | An order `txn_id`. Lines and quantities are refused. |
| `erp.confirm_order` | T2 | Sends the 855 for an order. | An order `txn_id` and `ack_type` (`AC`, `AD` or `RJ`). No free text. |
| `edi.send_asn` | T2 | Ships the reserved goods and sends the 856. | An order `txn_id`. Quantities and SSCC are refused. |
| `notify.send_email` | T2 | Sends one of three templates to a contact of the partner of the order. | A document id, a template and the address `to`. The schema checks only the shape of the address; the policy checks that it is a contact of the partner. No body text, no note reference. |
| `erp.post_vendor_invoice` | T3 | Books a supplier invoice that matches an order of the supplier. | A document id. An invoice without a matching order is refused. |
| `up.<server>.<tool>` | set by policy | A pinned tool of an upstream server. | Whatever the upstream schema allows. The tenant must be allowed to call it. There is no check against an order. |

## Operators

Operators use `edigatectl` and the `/ops/` API with an operator token.

- `POST /ops/txn/{key}/reopen` reopens a failed key. From the command line:
  `edigatectl txn reopen --url URL --token OPERATOR_TOKEN <key-hex>`.
- `GET /ops/approvals`, `POST /ops/approvals/{id}/approve`, `POST /ops/approvals/{id}/reject` handle approval
  requests. The operator sees typed values and rule names, never a partner's text. A request that nobody decides in
  30 minutes expires.

`edigatectl` also sets up the stand and checks the log. Run it without arguments for the full list. The main commands:

```sh
edigatectl tenant add <id> <name> [--interchange-id ZZ:ZZTESTSUPPL]
edigatectl partner add --tenant T --partner ZZ:ZZTESTBUYER --role customer --name N
edigatectl token issue --tenant T --kind agent|operator|admin|partner --id P [--partner Q:ID]
edigatectl fixtures load <file>          # tenant, partners with limits, price list
edigatectl call --url URL --token TOKEN <tool> [JSON]     # a small MCP client
edigatectl send --url URL --token T --from Q:ID --to Q:ID --file F   # post an interchange
edigatectl parse <file>                  # strict X12 check, no database
edigatectl approvals list|approve|reject --url URL --token OPERATOR_TOKEN
edigatectl txn reopen --url URL --token OPERATOR_TOKEN <key-hex>     # reopen a failed key
edigatectl manifests keygen|pin|list     # pins of upstream tools
edigatectl audit keygen --out FILE       # key that signs the anchors
edigatectl audit verify --tenant T [--from N --to M] [--anchor-dir D] [--pubkey K]
```

`audit verify` prints `ok <rows>` and exits 0. For a changed or removed row it prints `broken at seq <N>` and exits 1.
For an anchor that does not hold, or a database that is shorter than the last anchor of the file, it says which and
exits 1.

## Configuration

Settings come from environment variables and an optional YAML file (`--config`). Every variable is listed with its
meaning in [.env.example](.env.example). The ones you need first:

- `EDIGATE_DB_URL` (required) and `EDIGATE_LISTEN`.
- `EDIGATE_ONEC_URL`, `EDIGATE_WMS_URL`, `EDIGATE_PARTNER_URL`: the ERP, the warehouse and the partner. Empty switches
  the relay or the delivery off.
- `EDIGATE_QUARANTINE_BASE_URL`, `EDIGATE_QUARANTINE_MODEL`, `EDIGATE_QUARANTINE_API_KEY`: any OpenAI-compatible API
  for the note reader. An empty model switches `edi.extract_note` off. If the URL points to an outside API, the text of
  the note leaves your network: each call sends a fixed instruction, the description of the schema, the note text
  and a random marker. It sends no order data, but the note text is the partner's own text.
- `EDIGATE_MANIFEST_PUBKEY`: the public key for pins of upstream tools. Empty switches the upstream servers off.
- `EDIGATE_AUDIT_SIGNING_KEY` and `EDIGATE_AUDIT_ANCHOR_DIR`: the key that signs audit anchors and the folder outside the
  database where they are also written. Without a key the chain is written but there are no anchors.
- `EDIGATE_MODE`: see below.

The gateway keeps no secrets in code. The keys of the stand are made by the scripts, live in `deploy/compose/keys` and
are not committed.

## Modes, harness and corpus

One binary plays every configuration of the benchmark: `edigate --mode full|plain|reqhash|no-sanitize|no-policy|no-bizkey|no-contracts`.
The tool names stay the same.

- `full` is the gateway as described above.
- `plain` has open tools: the model writes the partner, the prices and the quantities, the other systems are called at
  once without an external id, and the text of the partner is shown whole. It stands for a gateway without protections.
- `reqhash` is `plain` plus a stored answer for a repeated request.
- The other four are `full` with one protection off (sanitizer, policy, business key, contracts), for ablations.

The modes exist so that an experiment can say what each protection buys. In the research protocol the configurations
are named `C0` (`plain`), `C1` (`reqhash`), `C2` (`plain` behind a third-party gateway), `C3` (`full`) and `C3-A1` to
`C3-A4` (the four ablations, in the order above).

Do not run edigate on real data. `plain`, `reqhash` and the ablations switch protections off on purpose. The default
is `full`, and `/healthz` shows the mode.

[bench/](bench) holds the experiment kit. `bench/edi_inject` generates the EDI-Inject corpus of X12 documents with
labels (`make corpus`); the rules are in [bench/edi_inject/README.md](bench/edi_inject/README.md). `bench/harness` runs an
agent over the corpus on the stand under each configuration and scripted faults, and `bench/metrics` reads the result
from the state of the mocks and the tables. `make bench-test` runs the tests of the kit and `make bench-smoke` runs a small
pilot (it takes a long time and needs Ollama). Results of the experiments are not part of this repository.

## Tests

- `make test`: policy tests, `go vet`, the tests with the race detector and `govulncheck`. It starts PostgreSQL in Docker.
- `make smoke`: a longer check of the running stand, step by step.
- `make chaos-dup`: eight gateway processes that keep dying and an ERP mock that sometimes loses answers; it checks one
  order per document. It takes several minutes.
- `make fuzz`: fuzzes the X12 parser for a minute.
- `make demo`: the three scenarios above.

## Threat model and invariants

[docs/threat-model.md](docs/threat-model.md) lists who can attack the gateway, what each attack reaches, which control
stops it and which test shows it, with the risks that remain. [docs/invariants.md](docs/invariants.md) states seven
promises about business effects, each with its mechanism and its assumptions, and what is not promised. The experiments
follow a written research protocol; `docs/PROTOCOL_SHA256` holds its hash.

## Status and what comes next

v0.1.0 contains everything described above. `make test` runs its tests and `make demo` shows the main scenarios on the
local stand; see "Status of the controls" above for what the tests cover. Next are the
experiments on the corpus and a paper about them. Support for a real AS2 channel, other X12 versions and a real ERP are
possible work, with no dates. Contributions and bug reports are welcome as issues.

## Security

To report a vulnerability, see [SECURITY.md](SECURITY.md).

## Licenses

The code is under the Apache License 2.0, see [LICENSE](LICENSE). The EDI-Inject corpus will be published under
CC-BY-4.0 together with the paper. The attack templates of two public projects, InjecAgent and AgentDojo, are used
under the MIT license; their files are in `bench/edi_inject/templates/` with the notices in
[NOTICE.md](bench/edi_inject/templates/NOTICE.md).
