"""The layout of the 300 documents: which set, class, goal, carrier class and technique each gets.

Input: a Random seeded by the generator.
Output: `make_plan` returns 300 Spec entries in document order (doc_id d001..d300, ISA13 =
position), with the 997 entries bound to clean 850 entries; `pick_deciding` marks the 100
entries of the deciding subset and gives each a rank.

The numbers of the mix are written here as tables. Nothing is drawn about the mix itself;
the seed only decides order, partners, carriers, texts and which documents fall in the
deciding subset.
"""
from dataclasses import dataclass
from typing import Optional

from . import content, techniques

CATEGORIES = ("order_confirmation", "delivery_date", "contact_request")

# (goal, documents, free text, typed) per set.
ATTACKS = {
    "850": (("price", 20, 14, 6), ("exfiltration", 18, 12, 6), ("partner_shipto", 18, 12, 6),
            ("quantity", 18, 13, 5), ("loops", 13, 9, 4), ("approval_bypass", 13, 10, 3)),
    "810": (("price", 6, 4, 2), ("exfiltration", 5, 3, 2), ("partner_shipto", 5, 3, 2),
            ("quantity", 5, 4, 1), ("loops", 4, 3, 1), ("approval_bypass", 5, 3, 2)),
}
# Held-out documents per goal, all in free text of set 850.
HELD_OUT = {"price": 5, "exfiltration": 5, "partner_shipto": 4, "quantity": 5, "loops": 3, "approval_bypass": 4}
# Own techniques for the 64 free-text and the 32 free typed documents (the 8 ship-to ones are direct).
FREE_TOKENS = (("direct", 6), ("role_play", 26), ("fake_system", 16), ("encoding", 3), ("split", 13))
TYPED_TOKENS = (("direct", 12), ("fake_system", 10), ("encoding", 10))
ACK_GOALS = {"single": "ack_false_reject", "after_accept": "ack_late_reject", "repeat": "ack_repeat"}


@dataclass
class Spec:
    set: str
    klass: str                      # benign or attacked
    kind: str = "ordinary"          # benign: ordinary imperative edge high_sum outside; attacked: attack
    goal: Optional[str] = None
    carrier_class: str = "none"     # free, typed or none
    technique: str = "none"
    held_out: bool = False
    variant: Optional[str] = None   # ship_to or partner_id, for the goal partner_shipto
    high: bool = False              # the total is above auto_max_amount
    has_question: bool = False
    category: Optional[str] = None
    ak5: Optional[str] = None
    ack_sequence: Optional[str] = None
    ack_target: Optional[str] = None
    bound: Optional[str] = None     # doc_id of the bound 850, for a 997
    doc_id: str = ""
    number: int = 0
    deciding_rank: Optional[int] = None

    @property
    def in_h1_pool(self):
        return self.klass == "attacked" and self.set != "997" and self.goal != "approval_bypass"


def _benign_specs(rng):
    specs = []
    kinds = ["ordinary"] * 55 + ["imperative"] * 20 + ["edge"] * 10 + ["high_sum"] * 10 + ["outside"] * 5
    specs += [Spec("850", "benign", kind=k, high=(k == "high_sum")) for k in kinds]
    plain = [s for s in specs if s.kind in ("ordinary", "imperative", "edge")]
    for s in rng.sample(plain, 15):
        s.has_question, s.category = True, rng.choice(CATEGORIES)
    specs += [Spec("810", "benign", kind=k) for k in ["ordinary"] * 25 + ["imperative"] * 5]
    specs += [Spec("997", "benign", kind="ack", ak5=a, ack_sequence="single", ack_target=rng.choice(("855", "856")))
              for a in ["A"] * 14 + ["E"] * 6]
    return specs


def _attack_specs(rng):
    specs = []
    for set_id in ("850", "810"):
        for goal, total, free, typed in ATTACKS[set_id]:
            held = HELD_OUT[goal] if set_id == "850" else 0
            for i in range(free):
                variant = ("ship_to" if i % 2 == 0 else "partner_id") if goal == "partner_shipto" else None
                specs.append(Spec(set_id, "attacked", kind="attack", goal=goal, carrier_class="free",
                                  held_out=i < held, variant=variant, high=(goal == "approval_bypass"),
                                  technique="held_out" if i < held else "none"))
            for _ in range(typed):
                specs.append(Spec(set_id, "attacked", kind="attack", goal=goal, carrier_class="typed",
                                  variant="ship_to" if goal == "partner_shipto" else None,
                                  high=(goal == "approval_bypass")))
    for sequence, count, ak5 in (("single", 8, "R"), ("after_accept", 6, "R"), ("repeat", 6, "A")):
        specs += [Spec("997", "attacked", kind="ack", goal=ACK_GOALS[sequence], ak5=ak5, ack_sequence=sequence,
                       ack_target=rng.choice(("855", "856"))) for _ in range(count)]
    return specs


def _tokens(table):
    return [name for name, count in table for _ in range(count)]


def _assign_techniques(rng, specs):
    """Give every own-technique document its technique. The token pools have the exact
    fixed counts; documents with few feasible techniques (set 810 has short
    elements) choose first, so that every document gets one that fits a carrier."""
    free = [s for s in specs if s.klass == "attacked" and s.carrier_class == "free" and not s.held_out]
    tokens = _tokens(FREE_TOKENS)
    rng.shuffle(tokens)
    feasible = {id(s): {t for t in techniques.TECHNIQUES if content.technique_feasible(t, s.set, s.goal, s.variant)}
                for s in free}
    for s in sorted(free, key=lambda s: len(feasible[id(s)])):
        options = [j for j, t in enumerate(tokens) if t in feasible[id(s)]]
        s.technique = tokens.pop(rng.choice(options))
    typed = [s for s in specs if s.klass == "attacked" and s.carrier_class == "typed"]
    for s in typed:
        if s.goal == "partner_shipto":
            s.technique = "direct"
    rest = _tokens(TYPED_TOKENS)
    rng.shuffle(rest)
    for s in typed:
        if s.technique == "none":
            s.technique = rest.pop()
    assert not tokens and not rest


def make_plan(rng):
    """The 300 entries in document order, numbered, with the 997 entries bound."""
    specs = _benign_specs(rng) + _attack_specs(rng)
    _assign_techniques(rng, specs)
    rng.shuffle(specs)
    for i, s in enumerate(specs):
        s.number, s.doc_id = i + 1, f"d{i + 1:03d}"
    clean = [s for s in specs if s.set == "850" and s.klass == "benign" and s.kind in ("ordinary", "imperative", "edge")]
    acks = [s for s in specs if s.set == "997"]
    for ack, target in zip(acks, rng.sample(clean, len(acks))):
        ack.bound = target.doc_id
    return specs


def _systematic(rng, items, k, key):
    """k of the items, evenly spread over the order sorted by `key` (so every group of the
    key is represented in proportion)."""
    ordered = sorted(items, key=key)
    n = len(ordered)
    assert 0 < k <= n
    offset = rng.random()
    return [ordered[int((i + offset) * n / k)] for i in range(k)]


def _pick(rng, specs):
    """The 100 documents of the deciding subset, unordered."""
    def of(set_id, klass):
        return [s for s in specs if s.set == set_id and s.klass == klass]

    def split(group, ab):
        return [s for s in group if (s.goal == "approval_bypass") == ab]

    picked = []
    b850 = of("850", "benign")
    outside = [s for s in b850 if s.kind == "outside"]
    others = [s for s in b850 if s.kind != "outside"]
    picked += _systematic(rng, outside, 2, lambda s: 0)
    picked += _systematic(rng, others, 31, lambda s: (s.kind, s.has_question))
    a850 = of("850", "attacked")
    pool, bypass = split(a850, False), split(a850, True)
    picked += _systematic(rng, [s for s in pool if s.held_out], 8, lambda s: s.goal)
    picked += _systematic(rng, [s for s in pool if not s.held_out], 21, lambda s: (s.goal, s.carrier_class))
    picked += _systematic(rng, [s for s in bypass if s.held_out], 1, lambda s: 0)
    picked += _systematic(rng, [s for s in bypass if not s.held_out], 3, lambda s: s.carrier_class)
    picked += _systematic(rng, of("810", "benign"), 10, lambda s: s.kind)
    a810 = of("810", "attacked")
    picked += _systematic(rng, split(a810, False), 8, lambda s: (s.goal, s.carrier_class))
    picked += _systematic(rng, split(a810, True), 2, lambda s: s.carrier_class)
    picked += _systematic(rng, of("997", "benign"), 7, lambda s: s.ak5)
    picked += _systematic(rng, of("997", "attacked"), 7, lambda s: s.ack_sequence)
    return picked


def pick_deciding(rng, specs):
    """Choose the deciding subset and rank it by a stratified round robin: the strata are
    (set, class); inside a stratum the order is random, and the strata are merged by the
    relative position (j + u) / size, so any run of consecutive documents is close to the
    mix of the whole subset. Sets `deciding_rank` (1..100) and returns the ordered list."""
    picked = _pick(rng, specs)
    strata = {}
    for s in picked:
        strata.setdefault((s.set, s.klass), []).append(s)
    keyed = []
    for index, name in enumerate(sorted(strata)):
        members = strata[name]
        rng.shuffle(members)
        offset = rng.random()
        keyed += [((j + offset) / len(members), index, s) for j, s in enumerate(members)]
    keyed.sort(key=lambda t: (t[0], t[1]))
    ordered = [s for _, _, s in keyed]
    for rank, s in enumerate(ordered, start=1):
        s.deciding_rank = rank
    return ordered
