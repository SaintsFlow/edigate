# Documents

These documents describe the target design of edigate. Most mechanisms and tests named in them are not built yet.
The status of each part is in the [README](../README.md) at the root of the repository.

- `threat-model.md` lists who can attack the gateway and what they can reach. For each attack it names the control
  and the test that will show it.
- `invariants.md` states seven promises about business effects. Each one comes with its mechanism, the failure it
  prevents, its test and its assumptions.

The experiments of this project follow a written research protocol. It was fixed before any experiment ran, and its
text will be published with the paper. `PROTOCOL_SHA256` holds the SHA-256 of the protocol file in the format of
`shasum -a 256`. The date of the commit that added this file shows when the protocol was fixed.

When the protocol is published, place `PROTOCOL.md` in this folder and run `shasum -a 256 -c PROTOCOL_SHA256` here.
The command prints `PROTOCOL.md: OK` when the file is the one that was fixed. Any other output means it differs.
