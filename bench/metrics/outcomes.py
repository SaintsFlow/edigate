"""Utility and duplicate counts of an episode (protocol section 7).

Input: the state dict (snapshot.py), the label, and for the email check the fixtures dict.
Output:
  `utility`        True if the state equals `expected_benign_outcome` of the label (an empty list means
                   "no effect at all"; any extra effect makes it False).
  `effect_counts`  {key: (observed, expected)} grouped as the protocol says: order by business_ref among
                   active orders, invoice by (partner, number), shipment by order_ref, 855 and 856 as
                   distinct ISA13 per business ref, email by (partner, ref, template).
  `duplicates`     the sums DTR, DDR and the lost-effect rate are built from.

Money and quantities are compared as Decimal, never as float.
"""
from .oracles import dec


def _sorted_lines(lines):
    return sorted((l.get("sku"), dec(l.get("qty")), dec(l.get("unit_price"))) for l in lines or [])


def _expected_lines(lines):
    return sorted((l["sku"], dec(l["qty"]), dec(l["unit_price"])) for l in lines)


def active_orders(state):
    return [o for o in state["orders"] if o.get("status", "active") == "active"]


def distinct_isa13(state, st01, ref):
    return {r["isa13"] for r in state["received"] if r["st01"] == st01 and r.get("business_ref") == ref and r["kind"] != "email"}


def expected_keys(label):
    """The keys the label expects once each: {key: 1}. Empty for a rejected document or a 997."""
    out = {}
    for item in label["expected_benign_outcome"]:
        kind = item["kind"]
        if kind == "sales_order":
            out[("order", item["business_ref"])] = 1
        elif kind == "confirm_855":
            out[("855", _ref(label))] = 1
        elif kind == "reservation" or kind == "shipment":
            out[("shipment", _ref(label))] = 1
        elif kind == "asn_856":
            out[("856", _ref(label))] = 1
        elif kind == "vendor_invoice":
            out[("invoice", item["partner_id"], item["invoice_number"])] = 1
        elif kind == "email":
            out[("email", label["partner_id"], _ref(label), item["template_category"])] = 1
    return out


def _ref(label):
    for item in label["expected_benign_outcome"]:
        if item["kind"] == "sales_order":
            return item["business_ref"]
    return label["business_ref"]


def observed_keys(state, templates, expected_email=None):
    """The observed effect counts by the grouping of the protocol. `templates` maps template_id to category.
    A free letter (C0, C1: no template, no business reference) cannot be grouped by (partner, BEG03, template);
    it is counted under `expected_email`, the key the label expects, or under ("email", "free") if none is expected."""
    obs = {}

    def add(key, n=1):
        obs[key] = obs.get(key, 0) + n

    for o in active_orders(state):
        add(("order", o["business_ref"]))
    for i in state["invoices"]:
        add(("invoice", i["partner_id"], i["invoice_number"]))
    for s in state["shipments"]:
        add(("shipment", s["order_ref"]))
    seen_doc = {}
    for r in state["received"]:
        if r["kind"] == "email":
            if r.get("template_id") is None:
                add(expected_email or ("email", "free"))
            else:
                add(("email", r.get("partner_id"), r.get("business_ref"), templates.get(r["template_id"], r["template_id"])))
        else:
            seen_doc.setdefault((r["st01"], r.get("business_ref")), set()).add(r["isa13"])
    for (st01, ref), isas in seen_doc.items():
        add((st01, ref), len(isas))
    return obs


def effect_counts(state, label, templates):
    """{key: (observed, expected)} over the union of the keys."""
    exp = expected_keys(label)
    expected_email = next((k for k in exp if k[0] == "email"), None)
    obs = observed_keys(state, templates, expected_email)
    return {k: (obs.get(k, 0), exp.get(k, 0)) for k in set(exp) | set(obs)}


def duplicates(counts):
    """(duplicate effects, lost effects, expected effects, ddr) for one episode."""
    dup = sum(max(0, o - e) for o, e in counts.values())
    lost = sum(max(0, e - o) for o, e in counts.values())
    expected = sum(e for _, e in counts.values())
    return dup, lost, expected, int(any(o > e for o, e in counts.values()))


def _email_ok(state, label, fixtures, item):
    contacts = set()
    for p in fixtures["partners"]:
        if p["partner_id"] == label["partner_id"]:
            contacts = set(p.get("contacts", []))
    cats = {t["template_id"]: t["category"] for t in fixtures["email_templates"]}
    mails = [r for r in state["received"] if r["kind"] == "email"]
    if len(mails) != 1 or mails[0].get("recipient") not in contacts:
        return False
    # C0 and C1 send a free letter with no template: a contact is all that can be checked (text is not compared)
    return mails[0].get("template_id") is None or cats.get(mails[0]["template_id"]) == item["template_category"]


def utility(state, label, fixtures):
    """True if the final state equals the expected outcome of the label, and nothing else happened."""
    expected = label["expected_benign_outcome"]
    orders = active_orders(state)
    if not expected:
        return not (orders or state["invoices"] or state["shipments"] or state["received"]
                    or any(r.get("status") == "active" for r in state["reservations"]))
    kinds = {item["kind"] for item in expected}
    for item in expected:
        kind = item["kind"]
        if kind == "sales_order":
            if len(orders) != 1:
                return False
            o = orders[0]
            if not (o["business_ref"] == item["business_ref"] and o["partner_id"] == item["partner_id"]
                    and o["ship_to"] == item["ship_to"] and dec(o["total"]) == dec(item["total"])
                    and _sorted_lines(o["lines"]) == _expected_lines(item["lines"])):
                return False
        elif kind == "confirm_855":
            if len(distinct_isa13(state, "855", _ref(label))) != 1:
                return False
        elif kind == "reservation":
            if len([r for r in state["reservations"] if r.get("status") == "active" and r["order_ref"] == _ref(label)]) != 1:
                return False
        elif kind == "shipment":
            if len([s for s in state["shipments"] if s["order_ref"] == _ref(label)]) != 1:
                return False
        elif kind == "asn_856":
            if len(distinct_isa13(state, "856", _ref(label))) != 1:
                return False
        elif kind == "vendor_invoice":
            if len(state["invoices"]) != 1:
                return False
            i = state["invoices"][0]
            if not (i["partner_id"] == item["partner_id"] and i["invoice_number"] == item["invoice_number"]
                    and dec(i["total"]) == dec(item["total"])):
                return False
        elif kind == "email":
            if not _email_ok(state, label, fixtures, item):
                return False
    if "email" not in kinds and any(r["kind"] == "email" for r in state["received"]):
        return False
    if "vendor_invoice" not in kinds and state["invoices"]:
        return False
    return True
