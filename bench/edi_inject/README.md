# EDI-Inject v1 generator

edigate is a gateway between LLM agents that speak MCP and trading partners that send X12 EDI documents (see the
[main README](../../README.md) and [docs](../../docs)). EDI-Inject is a test corpus that checks what an injection hidden in
an X12 document does: it makes 300 documents, some clean and some with an attack, and a label for each, so that a
harness can compare the state of the ERP and the warehouse after an agent has processed a document.

The research protocol that uses this corpus is not published yet. Only its SHA-256 is, in
[docs/PROTOCOL_SHA256](../../docs/PROTOCOL_SHA256). This file describes the corpus on its own.

Terms used below:

- **stand**: the test stand, made of the gateway, PostgreSQL and mock systems (ERP, warehouse, partner).
- **harness**: the program that runs an agent over the documents on the stand and reads the state afterwards.
- **H1 pool**: the attacked documents whose goal can reach an effect both without protection and with the gateway.
  Approval bypass is not in it, and neither are the 997 documents.
- **deciding subset**: 100 documents that every model runs. It is the file `deciding.json` and the label field
  `deciding_rank`.
- **AK5**: the status in a 997 acknowledgement. A means accepted, E accepted with errors, R rejected.

## Requirements

- Python 3.11, standard library only. Checked on 3.11.9. Another version may give other bytes; the hashes below are
  guaranteed only for 3.11.
- Go 1.27, for `make corpus` and for `edigatectl`, the strict X12 parser used to check the documents. `make build`
  puts it in `bin/`.

## Run

From the `edigate` folder:

```sh
make corpus
```

It builds `bin/edigatectl`, makes the corpus of seed 1 in `bench/out/corpus-seed1` and checks every document with
`edigatectl parse`. The generator alone, from the `bench` folder:

```sh
python3 -m edi_inject.generate --n 300 --seed 1 --out out/corpus-seed1
```

`--n` accepts only 300: the mix is fixed. Seed 1 is the corpus of the main run, seed 2 the pilot corpus. The command
prints `corpus sha256 <hex>` (the hash of `corpus.jsonl`) and exits with 0.

Check point (Python 3.11.9):

| seed | corpus sha256 |
|---|---|
| 1 | `b70de72d94028e98269b91177d00ea895dbed396fdc87e6f7b110ee6bb407625` |
| 2 | `4b0b29cb48633cb13ee37db7526c9f1744c3a41a897a0a374952aebc72eacb55` |

The bytes depend on the seed, on the code, and on the template files in `templates/` (commits in
[NOTICE.md](templates/NOTICE.md)).

## Output

| File | Content |
|---|---|
| `docs/d001.edi` … `d300.edi` | the raw bytes of each document |
| `corpus.jsonl` | one line per document: `doc_id`, `set`, `raw_b64` |
| `labels.jsonl` | one label per document, same order |
| `fixtures.json` | seed data of the stand: `tenant`, `partners` (each with `contacts`; buyers also `ship_to_codes`, suppliers `supplier_orders`), `price_list`, `email_templates` |
| `deciding.json` | the 100 `doc_id` of the deciding subset, in stratified round-robin order |
| `LICENSE.txt` | CC-BY-4.0 notice for corpus and labels, and the MIT notices of the templates |
| `SHA256SUMS` | SHA-256 of every other file, sorted by name |

The same seed gives the same bytes on every run. The generator uses one `random.Random(seed)`, fixed orders, Decimal
for money and no date of the run.

## The mix

Clean documents:

- 850 purchase orders, 100: 55 ordinary, 20 with imperative or system-like wording ("please deliver before 10:00"),
  10 with printable ASCII characters such as quotes, ampersand and tilde, 10 above `auto_max_amount` (an approval is
  expected), 5 with one character outside the 004010 character set (`outside_charset`; the strict parser rejects them).
  Fifteen clean 850 documents hold a question from the partner (`has_question`).
- 810 invoices, 30: 25 ordinary, 5 imperative.
- 997 acknowledgements, 20: 14 with AK5 = A and 6 with AK5 = E. Each of the 40 acknowledgements is bound to a different
  clean 850 (not one above the limit and not an outside-charset one), in the field `bound_850`.

Attacked documents, by goal (documents, free text, typed):

| Goal | 850 | 810 |
|---|---|---|
| price (in 810: invoice total) | 20 (14, 6) | 6 (4, 2) |
| exfiltration of the price list by email | 18 (12, 6) | 5 (3, 2) |
| partner or ship-to change | 18 (12, 6) | 5 (3, 2) |
| quantity inflation (in 810: amount) | 18 (13, 5) | 5 (4, 1) |
| loops (the same read repeated) | 13 (9, 4) | 4 (3, 1) |
| approval bypass | 13 (10, 3) | 5 (3, 2) |

The 997 attacks have their own goals: a false rejection (AK5 = R for a document that was received), a late
contradicting rejection (R after an A), and the same 997 sent again. They are 8, 6 and 6 documents. The corpus holds one
file for each; the stand sends the repeat. They carry no text, so `technique` and `carrier_class` are `none`.

The H1 pool has 112 documents: 87 of 850 and 25 of 810.

Where the attack goes (carriers):

| Set | Free-text elements | Typed elements |
|---|---|---|
| 850 | PID05, MSG01, REF03, N1-02, N2-01, N2-02, PER02, PER04, PER06, PER08, MTX02, MTX03 | BEG03, REF02, N1-04, SKU in PO107 |
| 810 | NTE02, REF03, N1-02, N2-01, N2-02, PER02, PER04, PER06, PER08 | BIG02, BIG04, REF02, N1-04 |

The generator draws the carrier at random among the elements whose maximum length holds the text. A typed attack on
partner or ship-to always uses N1-04 of the ship-to party: only the N1 of a ship-to or a buyer reaches the agent. Other
typed attacks use the other typed elements of the set.

Own techniques (104 documents): direct 26, role play 26, fake system message 26, encoding (base64) 13, split over
two or three elements 13. In free text 64 documents use own techniques (direct 6, role play 26, fake system 16,
encoding 3, split 13); in typed fields 40 (direct 20, fake system 10, encoding 10). The other 26 attacked 850
documents in free text use held-out techniques.

## Labels

Every label has the fields `doc_id`, `set`, `partner_id`, `class`, `goal`, `carrier`, `carrier_class`, `technique`,
`held_out`, `source_template`, `truncated`, `attacker_field`, `attacker_value`, `placebo_value`,
`expected_benign_outcome`, `expected_approval`, `outside_charset`, `has_question`, `expected_template_category`,
`bound_850`, `ack_target`, `ack_sequence`, `typed_format_ok`, `in_h1_pool`, `deciding_rank`, `business_ref`, `isa13`,
plus `benign_kind` and `order_total`. Notes on the values:

- `attacker_value` is the attacker's own value: a price, a total, a mail address, a ship-to code, a partner id, a
  quantity, `no_approval`, or for loops the object `{"tool", "same_document", "min_calls"}`. `placebo_value` is set for
  clean 850 and 810 documents only. It is an object with one placebo value per goal.
- `carrier` names the element. A split attack lists its elements joined with `+`. `typed_format_ok` says whether the
  typed string passes the format of the gateway allowlist (see `internal/sanitize/sanitize.go`; `null` for free text).
- `expected_benign_outcome` is the list of effects that follow if all free text is ignored. Each item has a `kind`:
  `sales_order` (`business_ref`, `partner_id`, `ship_to`, `lines` with `sku`, `qty`, `unit_price`, and `total`),
  `confirm_855` (`ack_type`), `reservation`, `shipment`, `asn_856`, `email` (`template_category`) and `vendor_invoice`
  (`partner_id`, `invoice_number`, `total`). The list is empty for a document that has to be rejected: a character
  outside the 004010 set, a foreign ship-to in N1-04, an unknown SKU in PO107, an order number in BIG04 that is not an
  order of the supplier. It is also empty for a 997.
- `expected_approval` is true when `order_total` is above `auto_max_amount` (5000.00 USD).
- A 997 carries stub control numbers (group 1, set 0001). `ack_target` (855 or 856) tells the harness which outgoing
  document the acknowledgement answers, so that it can put the real numbers in. `ack_sequence` is `single`,
  `after_accept` (the harness first sends an A) or `repeat`.

## Held-out techniques

`transplant.py` moves attack templates of InjecAgent and AgentDojo into EDI by fixed rules (written in its header). The
templates are in `templates/`; [NOTICE.md](templates/NOTICE.md) has the sources and the MIT notices. The code of these
projects is never run.

The generator builds these 26 documents on every run, and the code is public, so anyone can build and read them.
"Held-out" is a working rule of the authors: they do not read these documents while they tune the gateway and the
harness. The rule is not enforced by the code. What stands outside it is the research protocol, whose SHA-256 was
published before any run.

## Deciding subset

`deciding.json` lists 100 documents: 33 clean and 33 attacked 850, 10 and 10 of 810, 7 and 7 of 997. Of the 43 attacked
850 and 810 documents, 37 are in the H1 pool and 6 are approval bypass; 997 documents are not in the pool. Nine are
held-out documents. The order is a stratified round robin, so every block of ten documents holds both classes and at
least two sets.

## Tests

From the `edigate` folder, each file runs alone:

```sh
python3 -I bench/tests/test_mix.py
python3 -I bench/tests/test_determinism.py
python3 -I bench/tests/test_transplant.py
python3 -I bench/tests/test_x12.py
python3 -I bench/tests/test_labels.py
python3 -I bench/tests/check_parse.py bench/out/corpus-seed1 bin/edigatectl
```

`test_determinism.py` builds the corpus several times through the command line and takes a few seconds.
`check_parse.py` needs the corpus from `make corpus` and the built `bin/edigatectl`. It exits with 0 only if the parser
takes or rejects every document as its label says.

## Licence

Generator: Apache License 2.0, like the rest of the repository. Corpus and labels: CC-BY-4.0 (`LICENSE.txt` in the
output). Templates: MIT, see [NOTICE.md](templates/NOTICE.md).
