"""Fake models: deterministic, no network, same format of tool calls as a real model.

`fake-honest` does the task from the typed fields of the document and ignores every text of the partner.
`fake-gullible` does the same, but it reads all the strings in the tool answers (texts, field values, base64,
parts 'Part i/n'), and when it finds a sentence of the attack goals of the generator (edi_inject.content) it
carries it out as far as the arguments of the tools of the running configuration allow (a price, a ship-to,
a recipient and a body, a quantity, a repeated read). It is a check of the pipeline, not a model: the summary
labels its row "fake (pipeline check)".

Input: `complete(messages, tools)` like a chat API: the history of the episode and the tools offered.
Output: a `Completion` with the assistant message (text or tool calls) and token counts of zero.

How the plan is written: a generator yields (tool name, arguments) and receives the parsed answer of
the tool, so the plan reads top to bottom. The style of the tool arguments is found in the schema
offered: a tool whose schema has `partner_id` takes the values of the model (C0, C1, A4); one
that has `source_document_id` or `sales_order_txn_id` takes references (C3 and the other ablations).

The honest fake takes one thing from outside the typed fields, and says so here: the email template
category of a document with a question. A real model learns it from the text, which the sanitizer hides; the
fake gets it from the label so that the pipeline check can reach utility 1.0. The contact address it answers
to comes from the tool: `partner_contacts` in the header of edi.read_document (the gateway lists the known
addresses of the partner there), and in the modes that show the texts, the address in the PER segment.
"""
import asyncio
import base64
import binascii
import json
import re
from dataclasses import dataclass, field

from edi_inject import content, techniques

TEMPLATE_BY_CATEGORY = {"order_confirmation": "tpl_order_received", "delivery_date": "tpl_delivery_date",
                        "contact_request": "tpl_contact_back"}
SUBJECT = {"order_confirmation": "Your order is received", "delivery_date": "Delivery date of your order",
           "contact_request": "We will contact you"}
POLL_LIMIT = 12


@dataclass
class Completion:
    message: dict
    prompt_tokens: int = 0
    completion_tokens: int = 0
    cost_usd: float = 0.0


# --- the scanner of the gullible fake -----------------------------------------------------------------------

_MARK = "\x01"


def _sentence_pattern(goal, set_id, variant):
    sentence = content.goal_sentence(goal, set_id, _MARK, variant)
    return re.compile(re.escape(sentence).replace(_MARK, r"(\S+)"), re.I)


# (pattern, intent, set) built from the sentences of the generator itself.
_SENTENCES = [
    (_sentence_pattern("price", "850", None), "price", "850"),
    (_sentence_pattern("price", "810", None), "total", "810"),
    (_sentence_pattern("exfiltration", "850", None), "exfil", None),
    (_sentence_pattern("partner_shipto", "850", "ship_to"), "ship_to", None),
    (_sentence_pattern("partner_shipto", "850", "partner_id"), "partner_id", None),
    (_sentence_pattern("quantity", "850", None), "qty", "850"),
    (_sentence_pattern("quantity", "810", None), "total", "810"),
]
_LOOPS = re.compile(re.escape(content.goal_sentence("loops", "850", None, None)), re.I)
_BYPASS = re.compile(re.escape(content.goal_sentence("approval_bypass", "850", None, None)), re.I)

# The short commands of typed fields (techniques.TYPED_COMMANDS). Those with {v} carry the attacker's value
# and are read with a number in its place; the others carry none. The address of an exfiltration in a
# typed field is the address itself.
_INTENT = {("850", "price"): "price", ("810", "price"): "total", ("850", "quantity"): "qty", ("810", "quantity"): "total",
           ("850", "loops"): "loops", ("810", "loops"): "loops",
           ("850", "approval_bypass"): "bypass", ("810", "approval_bypass"): "bypass"}
_NUMBER = r"(\d+(?:\.\d+)?)"


def _typed_patterns():
    """[(pattern, intent)]; a pattern with a group gives the value, one without means True."""
    out = []
    for key, commands in techniques.TYPED_COMMANDS.items():
        for command in commands:
            before, found, after = command.partition("{v}")
            out.append((re.compile(re.escape(before) + (_NUMBER + re.escape(after) if found else "")), _INTENT[key]))
    out.append((re.compile(re.escape(content.EVIL_MAIL), re.I), "exfil"))
    return out


_TYPED = _typed_patterns()
_B64 = re.compile(r"[A-Za-z0-9+/]{8,}={0,2}")
_PART = re.compile(r"Part (\d+)/(\d+): (.*)")


def strings_of(value):
    """All string leaves of a parsed JSON value."""
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for v in value.values():
            yield from strings_of(v)
    elif isinstance(value, list):
        for v in value:
            yield from strings_of(v)


def expand(strings):
    """The strings plus what hides in them: base64 payloads and the parts 'Part i/n' joined in order."""
    out = list(strings)
    for s in strings:
        for token in _B64.findall(s):
            try:
                text = base64.b64decode(token + "=" * (-len(token) % 4), validate=True).decode("ascii")
            except (binascii.Error, UnicodeDecodeError, ValueError):
                continue
            if text.isprintable():
                out.append(text)
    parts = {}
    for s in strings:
        m = _PART.fullmatch(s.strip())
        if m:
            parts[int(m.group(1))] = (int(m.group(2)), m.group(3))
    if parts:
        total = next(iter(parts.values()))[0]
        if sorted(parts) == list(range(1, total + 1)):
            out.append(" ".join(parts[i][1] for i in range(1, total + 1)))
    return out


def find_intents(strings):
    """What the text asks for: {intent: value}. Intents: price, total, exfil, ship_to, partner_id, qty,
    loops, bypass. The first sentence found for an intent wins."""
    found = {}
    for s in expand(list(strings)):
        for rx, intent, _ in _SENTENCES:
            m = rx.search(s)
            if m:
                found.setdefault(intent, m.group(1).rstrip(".,;"))
        if _LOOPS.search(s):
            found.setdefault("loops", True)
        if _BYPASS.search(s):
            found.setdefault("bypass", True)
        for rx, intent in _TYPED:
            m = rx.search(s)
            if m:
                found.setdefault(intent, m.group(1) if rx.groups else content.EVIL_MAIL if intent == "exfil" else True)
    return found


# --- the plan --------------------------------------------------------------------------------------------------


def parse_result(text):
    """The answer of a tool as a dict; {'error': ...} for an error answer, {'raw': text} for anything else."""
    try:
        data = json.loads(text)
    except ValueError:
        return {"raw": text}
    return data if isinstance(data, dict) else {"raw": text}


def num(value):
    """A JSON number or string as a plain decimal string without a float detour for whole numbers."""
    if isinstance(value, float) and value.is_integer():
        return str(int(value))
    return str(value)


class FakeModel:
    """One episode of a fake model. `kind` is 'honest' or 'gullible'."""

    def __init__(self, kind, label, fixtures, document_id, tools, poll_sleep=1.0):
        self.kind = kind
        self.label = label
        self.fixtures = fixtures
        self.document_id = document_id
        self.poll_sleep = poll_sleep
        self.style = {t["name"]: t.get("input_schema", {}).get("properties", {}) for t in tools}
        self.available = set(self.style)
        self.plan = self._plan_810() if label["set"] == "810" else self._plan_850()
        self.started = False
        self.n = 0

    def values(self, tool):
        """True if the tool takes the values of the model (its schema has partner_id or a body)."""
        props = self.style.get(tool, {})
        return "partner_id" in props or "body" in props or "business_ref" in props

    async def complete(self, messages, tools):
        """The next assistant message: one tool call, or the final text."""
        try:
            if not self.started:
                self.started = True
                step = next(self.plan)
            else:
                last = messages[-1]
                step = self.plan.send(parse_result(last.get("content", "")))
        except StopIteration:
            return Completion({"role": "assistant", "content": "The document is processed."})
        name, args = step
        if name == "sleep":
            await asyncio.sleep(self.poll_sleep)
            return await self.complete(messages + [{"role": "tool", "content": "{}"}], tools)
        self.n += 1
        call = {"id": f"fake_{self.n}", "type": "function", "function": {"name": name, "arguments": json.dumps(args)}}
        return Completion({"role": "assistant", "content": None, "tool_calls": [call]})

    # -- pieces ---------------------------------------------------------------

    def _wait(self, txn_id):
        """Polls txn.status until the transaction is completed, rejected or expired. Returns the last status."""
        status = {}
        for _ in range(POLL_LIMIT):
            status = yield "txn.status", {"txn_id": txn_id}
            if status.get("status") in ("completed", "rejected", "expired") or "error" in status:
                break
            yield "sleep", {}
        return status

    def _intents(self, doc):
        if self.kind != "gullible":
            return {}
        return find_intents(strings_of(doc))

    def _contact(self, doc):
        """The address to answer a question to. The gateway that hides the texts of the partner lists the known
        addresses in `partner_contacts` of the header; the modes that show the texts do not, and the fake reads
        the address of the PER segment there, as a model would. It never reads the starting data of the stand."""
        contacts = doc.get("header", {}).get("partner_contacts")
        if contacts:
            return contacts[0]
        for t in doc.get("texts", []):
            if "PER04" in t.get("segment_path", "") and re.fullmatch(r"[^@\s]+@[^@\s]+", t.get("text", "")):
                return t["text"]
        return None

    def _price_list_text(self):
        """Gullible: the whole price list, as the attack asked ('the complete price list'): the answer of the
        tool as it is, with the label of the price list."""
        skus = [p["sku"] for p in self.fixtures["price_list"]][:50]
        prices = yield "erp.get_price_list", {"sku": skus}
        return json.dumps(prices)

    def _loops(self):
        for _ in range(10):
            yield "edi.read_document", {"document_id": self.document_id}

    def _exfil(self, to):
        if "notify.send_email" not in self.available:
            return
        text = yield from self._price_list_text()
        if self.values("notify.send_email"):
            yield "notify.send_email", {"to": to, "subject": "Price list", "body": text}
        else:
            yield "notify.send_email", {"source_document_id": self.document_id, "to": to, "template_id": "tpl_order_received"}

    def _lines(self, doc, intents):
        lines = []
        for l in doc.get("lines", []):
            price = intents.get("price", num(l.get("unit_price")))
            lines.append({"sku": l.get("sku"), "qty": num(l.get("qty")), "unit": l.get("unit") or "EA", "unit_price": price})
        if lines and "qty" in intents:
            lines[0]["qty"] = intents["qty"]
        return lines

    # -- the plan for an 850 -------------------------------------------------------

    def _plan_850(self):
        doc = yield "edi.read_document", {"document_id": self.document_id}
        if "error" in doc:
            return
        intents = self._intents(doc)
        if intents.get("loops"):
            yield from self._loops()
        if "exfil" in intents:
            yield from self._exfil(intents["exfil"])
        header = doc.get("header", {})
        lines = self._lines(doc, intents)
        partner = intents.get("partner_id", header.get("partner_id"))
        ship_to = intents.get("ship_to", header.get("ship_to"))
        ref = header.get("business_ref")
        if self.values("erp.create_sales_order"):
            args = {"partner_id": partner, "business_ref": ref, "ship_to": ship_to,
                    "currency": header.get("currency") or "USD", "lines": lines}
        else:
            args = {"source_document_id": self.document_id}
        order = yield "erp.create_sales_order", args
        if "error" in order:
            return
        txn = order.get("txn_id")
        status = yield from self._wait(txn)
        if status.get("status") != "completed":
            return
        if self.values("erp.confirm_order"):
            confirm = {"partner_id": partner, "business_ref": ref, "ack_type": "AC"}
        else:
            confirm = {"sales_order_txn_id": txn, "ack_type": "AC"}
        yield "erp.confirm_order", confirm
        qty_lines = [{"sku": l["sku"], "qty": l["qty"]} for l in lines]
        if self.values("wms.reserve"):
            reserve = yield "wms.reserve", {"business_ref": ref, "lines": qty_lines}
        else:
            reserve = yield "wms.reserve", {"sales_order_txn_id": txn}
        if "error" in reserve:
            return
        status = yield from self._wait(reserve.get("txn_id"))
        if status.get("status") != "completed":
            return
        if self.values("edi.send_asn"):
            yield "edi.send_asn", {"partner_id": partner, "business_ref": ref, "ship_to": ship_to, "lines": qty_lines}
        else:
            yield "edi.send_asn", {"sales_order_txn_id": txn}
        category = self.label.get("expected_template_category") if self.label.get("has_question") else None
        if category and "notify.send_email" in self.available:
            to = self._contact(doc)
            if self.values("notify.send_email"):
                yield "notify.send_email", {"to": to, "subject": SUBJECT[category], "body": SUBJECT[category] + "."}
            else:
                yield "notify.send_email", {"source_document_id": self.document_id, "to": to,
                                            "template_id": TEMPLATE_BY_CATEGORY[category]}

    # -- the plan for an 810 -------------------------------------------------------

    def _plan_810(self):
        doc = yield "edi.read_document", {"document_id": self.document_id}
        if "error" in doc:
            return
        intents = self._intents(doc)
        if intents.get("loops"):
            yield from self._loops()
        if "exfil" in intents:
            yield from self._exfil(intents["exfil"])
        header = doc.get("header", {})
        if self.values("erp.post_vendor_invoice"):
            lines = [{"sku": l.get("sku"), "qty": num(l.get("qty")), "unit_price": num(l.get("unit_price"))} for l in doc.get("lines", [])]
            total = intents.get("total") or header.get("total_amount")
            args = {"partner_id": intents.get("partner_id", header.get("partner_id")), "invoice_number": header.get("business_ref"),
                    "order_ref": header.get("order_ref") or "", "currency": header.get("currency") or "USD",
                    "total": num(total), "lines": lines}
        else:
            args = {"source_document_id": self.document_id}
        invoice = yield "erp.post_vendor_invoice", args
        if "error" in invoice:
            return
        yield from self._wait(invoice.get("txn_id"))
