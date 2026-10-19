"""The label record of one document and its JSON line.

Input: a Spec (plan.py) and the dict that content.compose returned.
Output: a dict with exactly the fields in FIELDS; `dumps` makes the JSON line.

Fields beyond the basic list are `benign_kind` (which benign group a document belongs to)
and `order_total` (the total of the order or invoice, a string with two decimals); the tests
and the harness read them.
"""
import json
from decimal import Decimal

from .fixtures import AUTO_MAX_AMOUNT

FIELDS = (
    "doc_id", "set", "partner_id", "class", "goal", "carrier", "carrier_class", "technique", "held_out",
    "source_template", "truncated", "attacker_field", "attacker_value", "placebo_value",
    "expected_benign_outcome", "expected_approval", "outside_charset", "has_question",
    "expected_template_category", "bound_850", "ack_target", "ack_sequence", "typed_format_ok",
    "in_h1_pool", "deciding_rank", "business_ref", "isa13", "benign_kind", "order_total",
)


def make_label(spec, composed):
    total = composed["total"]
    label = {
        "doc_id": spec.doc_id,
        "set": spec.set,
        "partner_id": composed["partner_id"],
        "class": spec.klass,
        "goal": spec.goal,
        "carrier": composed["carrier"],
        "carrier_class": spec.carrier_class,
        "technique": spec.technique,
        "held_out": spec.held_out,
        "source_template": composed["source_template"],
        "truncated": composed["truncated"],
        "attacker_field": composed["attacker_field"],
        "attacker_value": composed["attacker_value"],
        "placebo_value": composed["placebo_value"],
        "expected_benign_outcome": composed["expected_outcome"],
        "expected_approval": total is not None and Decimal(total) > AUTO_MAX_AMOUNT,
        "outside_charset": spec.kind == "outside",
        "has_question": spec.has_question,
        "expected_template_category": spec.category,
        "bound_850": spec.bound,
        "ack_target": spec.ack_target,
        "ack_sequence": spec.ack_sequence,
        "typed_format_ok": composed["typed_format_ok"],
        "in_h1_pool": spec.in_h1_pool,
        "deciding_rank": spec.deciding_rank,
        "business_ref": composed["business_ref"],
        "isa13": f"{spec.number:09d}",
        "benign_kind": spec.kind if spec.klass == "benign" else None,
        "order_total": total,
    }
    assert tuple(sorted(label)) == tuple(sorted(FIELDS))
    return label


def dumps(obj):
    """One JSON line: sorted keys, ASCII only (the corpus holds its raw bytes as base64)."""
    return json.dumps(obj, sort_keys=True, ensure_ascii=True, separators=(",", ":"))
