# Documents

These documents describe the design of edigate and the checks that go with it. The mechanisms they name are built in
v0.1.0. Tests marked T-BENCH need the experiment kit and have not been run with conclusions in this release. The parts
that do not exist (a real AS2 channel, other X12 versions) are listed as limits. What the gateway does today is in the
[README](../README.md) at the root of the repository. [demo.md](demo.md) is a walkthrough of the demo for recording it
on screen.

- `threat-model.md` lists who can attack the gateway and what they can reach. For each attack it names the control
  and the test that covers it. `make test` runs the tests, except those marked T-BENCH.
- `invariants.md` states seven promises about business effects. Each one comes with its mechanism, the failure it
  prevents, its test and its assumptions.

The experiments of this project follow a written research protocol. It was fixed before any experiment of the protocol
ran (pilot runs of the harness on the separate pilot corpus are not experiments of the protocol). Its text will be
published with the paper. `PROTOCOL_SHA256` holds the SHA-256 of the protocol file in the format of `shasum -a 256`.

The date of a commit is set by its author, so it proves little. An independent mark is the time of the push that
GitHub recorded: commit `e31f05d` on the branch `feature/gateway-core`, 19 October 2026.

When the protocol is published, place `PROTOCOL.md` in this folder and run `shasum -a 256 -c PROTOCOL_SHA256` here.
The command prints `PROTOCOL.md: OK` when the file is the one that was fixed. Any other output means it differs.
