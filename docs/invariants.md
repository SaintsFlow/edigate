# edigate invariants

Version 2, 19 October 2026. Companion file: `threat-model.md` (terms are explained in its glossary).

Seven invariants describe what the gateway promises about business effects. The promise is effect-once under stated
assumptions, not exactly-once delivery: a message can arrive more than once, and the effect still happens once because
the receiver and the keys are built for it. Each invariant has a statement, the mechanism that enforces it, the failure
it prevents (row IDs of the threat model), the test that checks it, and the assumptions it needs. Test IDs are the names of the
tests in this repository. An ID whose test does not exist yet is reserved (registry in section 4.5 of the threat
model). Section 3 lists
what the invariants do not promise.

## 1. Notation

### 1.1 Symbols

- t: tenant. τ: tool name (for example `erp.create_sales_order`). b: business key (section 1.2).
- H: SHA-256. The operator ‖ joins its parts with a length prefix for each part, so two different triples never give
  the same input.
- k = H(t ‖ τ ‖ b): the idempotency key of a call. It is computed by the gateway. The model does not supply it.
- r = H(canonical(args)): the request hash. `canonical` is the canonical JSON form of RFC 8785 (`rfc8785`). It is
  stored next to k.
- E(k): the set of external effects, that is, the effects in the adapters (mock-1c, mock-wms, partner-emulator) whose
  `external_id` equals k.
- The model passes to a tool only references issued by the gateway (`source_document_id`, or a `txn_id` that the
  gateway returned earlier) and values of closed enums. The gateway finds b from the reference.

### 1.2 Business key

The business key b is computed by the gateway from the business identifiers of the source document. It is not taken
from the model.

- For an order, and for the tools that act on an order (`erp.create_sales_order`, `erp.confirm_order`, `wms.reserve`):
  b = (partner_id, BEG03). partner_id is ISA05:ISA06 of the interchange that carried the order, without trailing
  spaces. BEG03 is the purchase order number of the 850, without spaces at both edges; case and leading zeros are kept.
- For `edi.send_asn`: the same order. There is one ASN per order; partial shipments are outside the MVP.
- For `notify.send_email`: the order plus the template identifier, b = (partner_id, BEG03, template_id). It allows one
  email of a template per order.
- For `erp.post_vendor_invoice`: b = (partner_id of the supplier, BIG02), where BIG02 is the invoice number of the 810.
  BIG02 is trimmed by the same rule as BEG03.
- Envelope numbers are not part of b. A partner that sends the same order again in a new interchange therefore
  produces the same b and the same k.

### 1.3 Interchange control numbers

An outgoing document carries three control numbers: ISA13 (interchange), GS06 (functional group) and ST02
(transaction set). They are written n(d) = (ISA13, GS06, ST02) for a document d. They identify the envelope of one
transmission. They are used to match acknowledgements (invariant I6) and are allocated under invariant I7.

## 2. The invariants

### I1. At most one effect per key, one record for every caller

**Statement.** Any number of calls with the same key k produce at most one external effect: |E(k)| ≤ 1. All of them
receive the same key record: the same `txn_id` and the current status of that one transaction. The status may advance
(for example from `pending_approval` to `accepted`), but a second transaction never appears. The key record and the
response are written in the same transaction as the reservation (I3), so a parallel second call waits for the commit
of the first one on the unique index and then reads the same record.

**Mechanism.** Table `idempotency_keys` has k as primary key. The call reserves k in a database transaction
(`IdempotencyStore.Reserve`) before any effect is queued. The key record is created together with the response in that
reservation transaction. It moves to the state `completed` later, when the effect is confirmed (I3,
`IdempotencyStore.Complete`). Effects are produced only by the relay from the outbox, never by the request path.

**Failure prevented.** F1 (lost response, agent retries), F4 (same order, new interchange, together with I2), DOS-03
(repeated writes).

**Test.** `T-IDEM-01` (`make chaos-dup`): the same call is sent ten times in parallel and then again after the
first response was dropped. The test expects one row in `mock_onec.sales_orders`, one `txn_id` in all responses, and a
status that is either the same or a later one for the same transaction. The ablation without the key in the benchmark
(`T-BENCH-03`) measures the duplicate rate without it.

**Assumptions.** One database primary with durable commits. The primary key is enforced for concurrent inserts.
SHA-256 has no collisions in practice.

### I2. The key comes from business identifiers, not from the model or the envelope

**Statement.** k depends only on (t, τ, b) with b as defined in section 1.2. Two calls with equal (t, τ, b) have the
same k, whatever the control numbers of their interchanges and whatever the model produced. If k is equal and r is
different, the call returns `conflicting_duplicate`, produces no effect, and opens a request for an operator. E(k) does
not change.

**Mechanism.** The tool code (`internal/tools`) reads the source document, derives b under section 1.2 and computes k.
Columns `business_key` and `req_hash` are stored in `idempotency_keys`. Deduplication of a whole interchange by ISA13
(flow P1) is a separate, earlier check. It catches only the repeat of the same transmission.

**Failure prevented.** F4 (same order in a new interchange), F5 (arguments rebuilt after a retry, the case in
`zheng2026acrfence`), VAL-03 (an old order number reused with other content), XT-04 (keys of different tenants differ
because t is part of k).

**Test.** `T-IDEM-02` (the same 850 in two interchanges with different ISA13 and GS06 gives one order),
`T-IDEM-03` (the same call with a changed request hash gives `conflicting_duplicate` and no new effect), `T-IDEM-04`
(the same business key under two tenants gives two keys), `T-IDEM-06` (the same order under another BEG03 gives two
orders; this is the documented limit). The benchmark ablation "business key replaced by request hash" measures the
duplicate rate of the weaker design.

**Assumptions.** The partner keeps BEG03 stable for the same order and does not reuse it for another order. A changed
BEG03 is a different order for the gateway (row VAL-04): the gateway cannot know that another number means the same
order. A legitimate replacement of an order under the same number ends in `conflicting_duplicate` and a person decides.
The partner_id comes from an interchange that passed the partner token check.

### I3. Reservation and its continuation in one transaction

**Statement.** The reservation of k and its continuation are written in one transaction. The record of a key has three
states. `reserved` has two forms: with an outbox row (the effect is queued) or with an approval request (waiting for a
decision). After an approval, the outbox row appears in the same transaction as the decision. `completed` follows the
confirmation of the effect. `failed` follows a rejection, an expiry or the status `dead`. A key never has more than one
outbox row at any moment. A repeat of a call with a `failed` key receives the recorded outcome. Only an operator can
reopen such a key, with `edigatectl txn reopen <key>`, and the reopening is written to the audit log.

**Mechanism.** `IdempotencyStore.Reserve`, `Outbox.Enqueue`, the approval insert and the audit append take the same
`pgx.Tx`. Foreign key `outbox.idem_key` references `idempotency_keys.key`, and a uniqueness rule allows one live outbox
row per key. The request path does not call adapters.

**Failure prevented.** F2 (a failure between commit and send must leave neither a key without a queued effect or
request, nor a queued effect without a key).

**Test.** `T-OUTBOX-01` checks the schema and the code path, not the database engine: a failure is injected
between the writes of the one transaction, and afterwards both records exist or neither does. A second check tries to
insert a second outbox row for the same key and expects a refusal. `T-OUTBOX-02` stops the relay after the commit and
before the send, and expects the effect after restart.

**Assumptions.** A single database transaction is atomic and durable. The relay is the only writer of effects. No
component outside the transaction writes to the outbox.

### I4. At-least-once delivery with an idempotent receiver gives one effect

**Statement.** The relay delivers each outbox row at least once until it is acknowledged or marked `dead` after 8
attempts. If the adapter treats `external_id` as an idempotency key, then |E(k)| ≤ 1 holds after any number of attempts
(safety), and |E(k)| = 1 holds once the row is `acked` (liveness under eventual delivery).

**Mechanism.** The relay selects rows with `SELECT … FOR UPDATE SKIP LOCKED`, calls `Adapter.Exists(k)`, and if no
effect exists calls `Adapter.Apply` with `external_id = k`. A second `Apply` with the same `external_id` returns the
existing reference. The existence check saves calls. Correctness does not depend on it, because the receiver
deduplicates.

**Failure prevented.** F2 (the adapter commits and the answer is lost), F3 and F6 on the partner side (retransmission
of the same interchange), DOS-03.

**Test.** `T-OUTBOX-03`: mock-1c with `MOCK_FAIL_AFTER_COMMIT_RATE` set high; the test checks one row per
`external_id`. `T-ACK-05`: partner-emulator receives the same 855 and the same 856 more than once and records
one document per ISA13.

**Assumptions.** The receiver is idempotent on `external_id` (mock-1c is by construction; a real 1C service must
implement the same check inside its own transaction). The partner side deduplicates on ISA13 or Message-Id. This is
an assumption: partner-emulator implements it, a real partner may not, and the text of RFC 4130 on duplicate
detection was not checked for this work. After `dead` the relay asks `Adapter.Exists(k)` once more and the saga uses
the answer (I5). If the adapter cannot answer, the row goes to an operator and no liveness is claimed for it.

### I5. A saga ends in all steps or in compensation, and the irreversible send comes last

**Statement.** For a saga with steps s1 to sn, where sn is the only irreversible step (sending a document to the
partner): (a) sn is not issued until s1 to s(n-1) are done; (b) if a step before sn fails for good, the compensations of
the completed steps run, and the saga ends `compensated`; (c) after sn is issued there is no compensation, a failure
leads to `incident`, and a person decides. The saga is `done` only when the document sent in sn is delivered under I6.
A step fails for good when the adapter returns a permanent error (an HTTP 4xx answer) or when the 8 attempts are used
up (`dead`). A step that failed for good may still have produced its effect (the request reached the adapter and the
answer was lost). Such a step is resolved by `Adapter.Exists(k)`: if the effect exists, the step counts as completed
and is compensated together with the others; if the adapter cannot answer, the compensation is issued anyway, because
it is a no-op when no effect exists. An approval request that is rejected or expires makes its step fail for good in
the same way: for example, when the 855 is not approved, the order already created in mock-1c is cancelled.
Compensations are idempotent and have keys of their own: cancelling an order in mock-1c uses k ‖ "cancel", and
releasing a reservation in mock-wms uses k ‖ "release". If a compensation itself fails, the saga goes to `incident` and
a request goes to an operator. The timeouts are those of I6: 30 minutes per MDN wait with at most three repeats, and
24 hours for the 997.

In the order saga, sn is the 855 (the order in the ERP comes first). In the shipment saga, sn is the 856 (the
reservation and the shipment confirmation come first).

**Mechanism.** Tables `sagas` and `saga_steps`; each step is tied to an outbox row; the relay advances the saga and
writes the state in the same transaction as the step result. Compensations are internal actions of the relay, not tools
of the agent.

**Failure prevented.** F2 and F6 (an order in the ERP with no confirmation sent), F7 (a rejected 997 treated as
success), ACK-01.

**Test.** `T-SAGA-01` (scenario S2 end to end): a failure before the send gives `compensated` with no partner
document. `T-SAGA-02`: a failure of a compensation gives `incident`. `T-SAGA-03`: a failure after the send gives
`incident`, and no saga stays `running` after the timeouts. The benchmark counts the share of sagas that end
`done` or `compensated`.

**Assumptions.** Every step before sn has a compensation (the cancel call in mock-1c and the release call in mock-wms
exist). Timeouts are enforced by the relay clock.

### I6. Delivered means MDN and 997 both agree

**Statement.** An outgoing document d is delivered if and only if (a) an MDN arrived whose Received-Content-MIC equals
the MIC of the bytes sent, and (b) a 997 arrived whose AK5 for the set of d is A or E. An AK9 of P is resolved by the AK5
of each set. AK5 = R is not delivered. A saga does not become `done` before its last document is delivered. The first
final receipt decides. A repeat of the same 997 is ignored. A later contradicting receipt (R after A) puts the saga in
`incident` and is written to the audit log. An MDN that arrives after `incident` is only written to the audit log.

**Mechanism.** An MDN is matched to the transmission by `original_message_id`. The incoming 997 passes flow P1 and is
matched to the outgoing document by (tenant, partner that sent the 997, GS06 from AK102, ST02 from AK202). The 997 does
not carry the ISA13 of the original interchange; ISA13 appears only in TA1, which is outside the MVP. The result is
recorded in `saga_steps`. AK5 = E is written to the audit log as accepted with remarks. If no MDN arrives in 30 minutes,
the same transmission is repeated with the same Message-Id and ISA13 (I7), at most three times, then `incident`. If no
997 arrives in 24 hours, a request goes to an operator.

**Failure prevented.** F6 (missing MDN), F7 (negative or partial 997), F8 (acknowledgement matched to the wrong
document), ACK-01 and ACK-02 (false, repeated and contradicting receipts).

**Test.** `T-ACK-01` to `T-ACK-04`: partner-emulator is set with `PARTNER_ACK_STATUS` to A and to R, with and
without MDN, with a delayed 997 (`PARTNER_ACK_DELAY_MS`), with a repeated 997 and with a contradicting one. The tests
check the final saga state, the audit records, and that no resend follows an R.

**Assumptions.** The MIC is computed over the bytes sent, with SHA-256 (RFC 4130 defines sha1 and md5 only; SHA-256 as a
MIC algorithm comes from RFC 5751). The 997 reports syntax only. The emulated MDN is not signed.
Matching works only if I7 holds.

### I7. Control numbers are allocated with the outbox row and stay fixed

**Statement.** For each outgoing document, all three numbers n(d) = (ISA13, GS06, ST02) are allocated in the same
transaction that writes its outbox row. Every retransmission uses the same n(d). For two different outgoing documents of
the same tenant and partner, the ISA13 values differ, the GS06 values differ, and the ST02 values differ.

**Mechanism.** Table `control_numbers` with primary key (tenant_id, partner_id, kind) and kinds isa, gs, st. The counter
is updated and returned in the transaction of the outbox insert, so a rollback also rolls back the counter. The numbers
are stored with the document. ISA13 is written with nine digits and ST02 is padded with zeros to at least four
characters. Unique constraint on `edi_interchanges` (tenant_id, direction, counterparty_id, isa13).

**Failure prevented.** F8 (number reuse after a crash, change on retransmission), F6 (a retransmission that the partner
can recognise as the same interchange).

**Test.** `T-ACK-05` and `T-ACK-06`: the relay is stopped after the allocation and before the send; the
retransmission carries the same three numbers; 1000 documents give 1000 distinct sets of numbers.

**Assumptions.** The counters stay below 10^9 (ISA13 has nine digits; wrap-around is not handled). The standard asks
only that ST02 be unique inside a group. The gateway makes all three unique per tenant and partner, which is stronger and
makes the matching of acknowledgements unambiguous.

## 3. What the invariants do not guarantee

- **Effects without a source document.** Every tool with an effect goes back to a source document, directly or through a
  `txn_id`. Actions of an agent with no source document are not supported, so nothing is promised for them.
- **A partner that changes BEG03 for the same order.** A changed number is a new order (row VAL-04). Keys cannot tell
  the two cases apart. Amount limits per document and per partner per day bound the exposure.
- **A supplier that resends the same invoice under another BIG02.** The key sees a new invoice. T3 with two operators
  and the check against the purchase order are the remaining controls.
- **Semantic correctness of values that pass policy.** A price, quantity or ship-to that is allowed by policy can still be
  wrong. Steering inside the closed schema of the quarantine reader (row INJ-10) is bounded, not removed.
- **Delivery to the partner's ERP.** A 997 says the partner's translator accepted the syntax. It does not say the
  partner's business system processed the order. A partner can also send an A without processing.
- **Anything about the model's reasoning.** The invariants constrain effects at the gateway. They do not say that the
  model understood a document or chose a good action.
- **Failures outside the model of the threat model.** A compromised database or host, and effects of a receiver that is
  not idempotent.

## 4. Relation to prior work

`li2026exactlyonce` showed in a sandbox of duplicate side effects that an idempotency key on every write lowers the share
of duplicates from 28 percent to 4 percent, and that the contract of the tool matters. `mansoor2026verified` wraps tool
calls on the client with postcondition checks, checks before retry and idempotency keys. `mcpidempotent` is a proxy that
drops repeated `tools/call` requests by tool, argument hash and run identifier. The idea of a deduplicating proxy is not
ours. `zheng2026acrfence` shows that an agent restored from a checkpoint rebuilds a slightly different request and the
server counts it as new, which is the case behind F5. `chen2026cordon` buffers external effects in an outbox and
releases them after the whole flow is checked. `chang2025sagallm` and `garciamolina1987sagas` give the saga pattern for
agent planning and for long transactions.

What these invariants add: the key is computed on the gateway side from business identifiers of the source document
(partner and BEG03), so it does not depend on the model or on envelope numbers and also catches the same order resent in
a new interchange (I2). The saga closes on partner acknowledgements, not on the send: the MDN and the 997 decide whether a
document is delivered, and a negative 997 stops resending (I5 and I6). Control numbers of outgoing documents are
allocated together with the outbox row (I7). We do not claim a new transaction model. We claim that these properties,
together, hold at an MCP gateway for X12 documents, and the benchmark measures what they change.

## 5. References

- `li2026exactlyonce`: J. Li, "Where Does Exactly-Once Live? Model, Harness, and Tool-Contract Effects on Duplicate Side
  Effects in LLM Agents", 2026, https://arxiv.org/abs/2609.29095
- `mansoor2026verified`: I. K. Mansoor, A. Phadke, P. Rana, "Verified Tool Calls Improve LLM Agent Reliability Under
  Non-Atomic Failures", 2026, https://arxiv.org/abs/2608.02645
- `mcpidempotent`: krish-shahh, "mcp-idempotent", https://github.com/krish-shahh/mcp-idempotent
- `zheng2026acrfence`: Y. Zheng et al., "ACRFence: Preventing Semantic Rollback Attacks in Agent Checkpoint-Restore",
  2026, https://arxiv.org/abs/2603.20625
- `chen2026cordon`: Z. Chen et al., "Cordon: Semantic Transactions for Tool-Using LLM Agents", 2026,
  https://arxiv.org/abs/2606.17573
- `chang2025sagallm`: E. Y. Chang, L. Geng, "SagaLLM: Context Management, Validation, and Transaction Guarantees for
  Multi-Agent LLM Planning", 2025, https://arxiv.org/abs/2503.11951
- `garciamolina1987sagas`: H. Garcia-Molina, K. Salem, "Sagas", SIGMOD 1987, https://doi.org/10.1145/38713.38742
- `rfc8785`: A. Rundgren, B. Jordan, S. Erdtman, "JSON Canonicalization Scheme (JCS)", RFC 8785, 2020,
  https://www.rfc-editor.org/rfc/rfc8785
- `rfc4130`: D. Moberg, R. Drummond, "MIME-Based Secure Peer-to-Peer Business Data Interchange Using HTTP, Applicability
  Statement 2 (AS2)", RFC 4130, 2005, https://www.rfc-editor.org/rfc/rfc4130
- RFC 5751, "Secure/Multipurpose Internet Mail Extensions (S/MIME) Version 3.2 Message Specification", 2010,
  https://www.rfc-editor.org/rfc/rfc5751
