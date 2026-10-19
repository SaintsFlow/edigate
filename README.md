# edigate

edigate sits between LLM agents that speak MCP (Model Context Protocol) and trading partners that send X12 EDI
documents such as purchase orders and invoices. It shows the agent each document without the partner's free text and
lets the agent create business effects, such as a sales order, only through checked tools.

Status: work in progress, v0.1 is not released.

- Works now: the MCP endpoint with bearer tokens per agent, tenants, an append-only audit log, and one tool,
  `edi.list_inbox`, which returns an empty list until EDI intake is built.
- Designed, not built yet: EDI intake, free-text quarantine, one effect per business key, the outbox and sagas that
  close on the 997 (functional acknowledgement) and the MDN (AS2 receipt). The design is in [docs](docs).

## Try it

You need Go 1.27, Docker, `curl`, and free local ports 55432 and 18080.

```sh
make smoke
```

This builds the binaries, starts PostgreSQL and the gateway in Docker, creates a tenant and two tokens, and checks
that an agent gets an empty inbox, that bad requests get the right errors, and that the audit log records the calls.
It prints `smoke ok` and exits with code 0 when everything matches. The stand keeps running afterwards; `make down`
stops it and deletes its data.

`make test` runs `go vet`, the tests with the race detector and `govulncheck`. It starts PostgreSQL in Docker and
downloads govulncheck on the first run.

## Documents

[docs](docs) holds the threat model, the invariants and the SHA-256 of the research protocol that the experiments
follow. The protocol text is not published yet.

## License

Apache License 2.0, see [LICENSE](LICENSE).
