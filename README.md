# edigate

edigate sits between LLM agents that speak MCP (Model Context Protocol) and trading partners that send X12 EDI
documents such as purchase orders and invoices. It shows the agent each document without the partner's free text and
lets the agent create business effects, such as a sales order, only through checked tools.

Status: work in progress, v0.1 is not released.

- Works now: the MCP endpoint with bearer tokens per agent, tenants, an append-only audit log; EDI intake on
  `POST /edi/inbound` (strict X12 004010 parser for 850, 810 and 997, a receipt (MDN) per interchange, replay and
  conflict checks by ISA13, tokens per trading partner); and two tools, `edi.list_inbox` and `edi.read_document`,
  which show an agent the typed fields of a document and none of the partner's free text.
- Designed, not built yet: free-text quarantine (notes), one effect per business key, the outbox and sagas that
  close on the 997 (functional acknowledgement) and the MDN (AS2 receipt). The design is in [docs](docs).

## Try it

You need Go 1.27, Docker, `curl`, and free local ports 55432 and 18080.

```sh
make smoke
```

This builds the binaries, starts PostgreSQL and the gateway in Docker, creates a tenant, an agent and a trading
partner, and checks that an agent gets an empty inbox, that bad requests get the right errors, that the audit log
records the calls, and that a partner can send a purchase order, get a receipt, and see the agent read it without
the partner's free text. It prints `smoke ok` and exits with code 0 when everything matches. The stand keeps
running afterwards; `make down` stops it and deletes its data.

`make test` runs `go vet`, the tests with the race detector and `govulncheck`. It starts PostgreSQL in Docker and
downloads govulncheck on the first run. `make fuzz` fuzzes the X12 parser for 60 seconds.

`edigatectl parse <file>` checks an X12 file with the same strict parser and prints a JSON summary; the exit code is
0 only when the envelope and every set are accepted.

## Documents

[docs](docs) holds the threat model, the invariants and the SHA-256 of the research protocol that the experiments
follow. The protocol text is not published yet.

## License

Apache License 2.0, see [LICENSE](LICENSE).
