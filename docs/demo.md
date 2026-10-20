# Demo walkthrough

`make demo` runs three scenarios against a stand that it builds from nothing. This page lists what each line of its
output checks, in order, so that the run can be recorded on screen. It takes a few minutes; the first run is longer
because it builds the images. Run `make down` first to start from an empty stand, and start Ollama on the host
(`ollama run llama3.2:3b`) if you want the note-reading step to run. Without it that step prints `skip`.

## The stand

- The command builds the binaries, wipes the stand and builds it again. `ok   stand is up` means the gateway, the ERP
  mock, the warehouse mock, the partner emulator and PostgreSQL run in Docker.
- The next line sets up a tenant, an agent, an operator and the buyer.

## S1, an injection that reaches no effect

- S1.1 and S1.2: the 850 carries instructions in two text fields, and the agent sees only references.
- S1.3: the order number failed its format and is a reference too.
- S1.4: a model reads one note into a closed value, or the answer is refused. It never comes back as text.
- S1.5: the order took its price from the price list and its ship-to code from the document.
- S1.6: a letter to a foreign address is `policy_denied` with the rule `email.recipient`.
- S1.7: a note reference in the address of a letter is `tainted_sink`.
- S1.8: no letter left the gateway.

## S2, a saga with receipts

- S2.1: the order is in the ERP.
- S2.2: the 855 has a receipt (MDN) and a 997 with AK5 `A`, and the order saga is `done`.
- S2.3: the goods are reserved in the warehouse.
- S2.4: the same for the 856, and both sagas are `done`.
- S2.5: the partner got one 855 and one 856 with different ISA13.
- S2.6: the 997 for the incoming 850 reached the partner.

## S3, faults without duplicates

- S3.1: the same order in a new exchange gives the same `txn_id` and one order.
- S3.2: a repeated call gives the same answer and one order. The lost answer is simulated by calling twice; real lost
  answers are tested by `make chaos-dup`.
- S3.3: a negative 997 stops the saga as an incident, and no second 855 with a new ISA13 goes out.
- S3.4: the first 100 rows of the audit log are anchored, in the database and in a file outside it, and the log
  verifies.
- S3.5: the trigger refuses to change a row.
- S3.6: with the trigger switched off by the rights of the database owner, as an attacker with such rights would do, a
  row is edited. `audit verify` prints `broken at seq <N>`.
- S3.7: with the row put back, the log verifies again.

## The end

The last line starts with `demo ok`. Check the exit code with `echo $?`; it prints `0`. Then run `make down`.
