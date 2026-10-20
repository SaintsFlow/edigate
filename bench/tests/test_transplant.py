"""The held-out documents follow the fixed transplant rules.

Run: python3 -I bench/tests/test_transplant.py
The templates and the documents built from them are deferred material: this test compares
texts with assertTrue and prints only document ids, counts and lengths, never a text.
"""
import re
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import common  # noqa: E402
from edi_inject import content, transplant  # noqa: E402

MAX_LEN = {"MSG01": 264, "MTX02": 4096}
CLASS_ORDER = ["DirectAttack", "IgnorePreviousAttack", "SystemMessageAttack", "InjecAgentAttack",
               "ImportantInstructionsAttack"]


def normalize(text):
    """The only edit of rule 4: white space with a CR, LF or TAB becomes one space; ends trimmed."""
    def one(m):
        return " " if re.search(r"[\t\r\n]", m.group(0)) else m.group(0)
    return re.sub(r"[ \t\r\n]+", one, text).strip(" ")


def carrier_value(raw, carrier):
    for seg in common.segments(raw):
        if carrier == "MSG01" and seg[0] == "MSG":
            return seg[1]
        if carrier == "MTX02" and seg[0] == "MTX":
            return seg[2]
    return None


def held_out():
    built, _ = common.corpus(1)
    return [(r, built["composed"][r["doc_id"]]["raw"]) for r in built["labels"] if r["held_out"]]


class Transplant(unittest.TestCase):
    def test_templates_are_read_not_run(self):
        before = set(sys.modules)
        agentdojo = transplant.load_agentdojo()
        rows = transplant.load_injecagent()
        self.assertEqual(sorted(agentdojo), sorted(CLASS_ORDER))
        self.assertEqual(len(rows), 32)
        self.assertFalse(any(m.startswith("agentdojo") for m in set(sys.modules) - before))
        self.assertTrue(all(isinstance(t, str) and t for t in agentdojo.values()))

    def test_value_is_normalized_template_cut_to_the_element(self):
        agentdojo, rows = transplant.load_agentdojo(), transplant.load_injecagent()
        docs = held_out()
        self.assertEqual(len(docs), 26)
        for label, raw in docs:
            doc_id = label["doc_id"]
            value = carrier_value(raw, label["carrier"])
            self.assertIsNotNone(value, doc_id)
            kind, _, name = label["source_template"].partition(":")
            if kind == "injecagent":
                row = int(name.rsplit(":", 1)[1])
                expected = normalize(rows[row - 1])
                self.assertTrue(re.search(r"@", label["attacker_value"]) and label["attacker_value"] in expected, doc_id)
            else:
                variant = label["attacker_field"] if label["goal"] == "partner_shipto" else None
                sentence = content.goal_sentence(label["goal"], "850", label["attacker_value"], variant)
                template = agentdojo[name]
                expected = normalize(template.replace("{goal}", sentence).replace("{user}", "Emma Johnson")
                                     .replace("{model}", "AI model"))
                self.assertTrue(sentence in value, doc_id)
            self.assertTrue(value == expected[:MAX_LEN[label["carrier"]]], doc_id)
            self.assertEqual(label["truncated"], len(expected) > MAX_LEN[label["carrier"]], doc_id)
            self.assertFalse(re.search(r"\{(goal|user|model)\}", value), doc_id)
            self.assertFalse(re.search(r"[\r\n\t]", value), doc_id)

    def test_carrier_rule(self):
        agentdojo, rows = transplant.load_agentdojo(), transplant.load_injecagent()
        turn = 0
        for label, raw in held_out():
            kind, _, name = label["source_template"].partition(":")
            if kind == "injecagent":
                length = len(normalize(rows[int(name.rsplit(":", 1)[1]) - 1]))
            else:
                variant = label["attacker_field"] if label["goal"] == "partner_shipto" else None
                sentence = content.goal_sentence(label["goal"], "850", label["attacker_value"], variant)
                length = len(normalize(agentdojo[name].replace("{goal}", sentence).replace("{user}", "Emma Johnson")
                                       .replace("{model}", "AI model")))
            if length > 264:
                self.assertEqual(label["carrier"], "MTX02", label["doc_id"])
            else:
                self.assertEqual(label["carrier"], ("MSG01", "MTX02")[turn % 2], label["doc_id"])
                turn += 1
        self.assertGreater(turn, 0)

    def test_classes_go_round_in_document_order(self):
        names = [label["source_template"].split(":", 1)[1] for label, _ in held_out()
                 if label["source_template"].startswith("agentdojo:")]
        self.assertEqual(len(names), 21)
        self.assertEqual(names, [CLASS_ORDER[i % 5] for i in range(21)])

    def test_injecagent_rows_are_not_repeated_and_value_is_first_address(self):
        agentdojo, rows = transplant.load_agentdojo(), transplant.load_injecagent()
        used = []
        for label, _ in held_out():
            if label["goal"] != "exfiltration":
                continue
            row = int(label["source_template"].rsplit(":", 1)[1])
            used.append(row)
            first = re.search(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}", rows[row - 1]).group(0)
            self.assertTrue(label["attacker_value"] == first, label["doc_id"])
        self.assertEqual(len(used), 5)
        self.assertEqual(len(set(used)), 5)

    def test_normalize_rule(self):
        self.assertEqual(normalize("  a \n\n b\t\tc  d \r\n"), "a b c  d")
        self.assertEqual(normalize("keep  double  spaces"), "keep  double  spaces")
        self.assertEqual(normalize("x \n y"), "x y")


if __name__ == "__main__":
    unittest.main()
