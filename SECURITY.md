# Security policy

## Reporting a vulnerability

Please report a vulnerability in edigate privately, through a GitHub Security Advisory of this repository:
open the Security tab, choose "Report a vulnerability" and describe the problem. Do not open a public issue for it.

Please include the version or commit, the steps to reproduce it, and what an attacker gains. A short proof of concept
helps. Use synthetic data only; do not send real partner documents.

You will get an answer within seven days. Fixes for confirmed problems are prepared in the private advisory and
published together with the advisory. The usual period before public disclosure is 90 days, shorter if a fix is
ready, longer only by agreement.

## What is in scope

The code in this repository and the stand it starts: the gateway, `edigatectl`, the policy, the mocks and the
scripts. The threat model in [docs/threat-model.md](docs/threat-model.md) says what the gateway claims to stop and what
it does not. A bypass of a control listed there is a vulnerability. A risk the model names as remaining (for example
steering inside a closed schema) is known, but a stronger attack on it is still welcome.

## What is out of scope

- Attacks that need rights on the database or on the host, apart from the cases the audit chain is built to show.
  The chain has limits: rows after the last signed anchor (up to 99) can be recomputed by someone with database
  rights, and a cut tail shows only against an anchor file that the attacker cannot write. See the README.
- The simulated AS2 channel: it has no signatures by design, and the document says so.
- Findings that come from the local stand only, such as the default passwords of its PostgreSQL. The stand is for
  tests and is bound to 127.0.0.1.
- Denial of service by plain volume of traffic. A way around the rate limits or the limits of the parser is in scope.

## Versions

Version 0.1.0 is a research prototype. Only the latest commit of the main branch gets fixes.
