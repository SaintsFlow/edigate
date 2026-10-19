"""Tests of the configurations (protocol section 2), the task tool lists (section 4), the choice of documents,
and the operator in its two modes (section 4). Run as a file from the edigate folder:
bench/.venv/bin/python -I bench/tests/test_harness_config.py
"""
import base64
import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import common  # noqa: E402,F401

from harness import configs, faults, operator  # noqa: E402
from harness.corpus import Corpus  # noqa: E402
from harness.stand import HarnessError, address_document  # noqa: E402


class Configurations(unittest.TestCase):
    def test_config_to_mode(self):
        want = {"C0": "plain", "C1": "reqhash", "C2": "plain", "C3": "full", "A1": "no-sanitize", "A2": "no-policy",
                "A3": "no-bizkey", "A4": "no-contracts"}
        self.assertEqual({c: configs.get(c).mode for c in want}, want)
        with self.assertRaises(ValueError):
            configs.get("C9")

    def test_open_configurations_have_no_gateway_tables(self):
        self.assertEqual([c for c in configs.CONFIGS if configs.is_open(c)], ["C0", "C1", "C2"])
        self.assertTrue(configs.has_gateway_tables("C3") and configs.has_gateway_tables("A4"))

    def test_tool_list_of_the_task_is_cut_by_tools_list(self):
        with_note = set(configs.TOOLS_850) | {"edi.extract_note", "erp.post_vendor_invoice"}
        self.assertEqual(configs.offered_tools("850", with_note), configs.TOOLS_850 + ["edi.extract_note"])
        self.assertEqual(configs.offered_tools("810", with_note), configs.TOOLS_810 + ["edi.extract_note"])
        # C0 and A1 do not list the note tool, so the agent is not offered it
        without = with_note - {"edi.extract_note"}
        self.assertNotIn("edi.extract_note", configs.offered_tools("850", without))
        # a tool the mode does not list is not offered either
        self.assertNotIn("wms.reserve", configs.offered_tools("850", without - {"wms.reserve"}))
        self.assertNotIn("erp.post_vendor_invoice", configs.offered_tools("850", with_note))

    def test_c2_needs_the_agentgateway_image_and_names_the_reason(self):
        real = configs._image_present
        try:
            configs._image_present = lambda: False
            with self.assertRaises(RuntimeError) as ctx:
                configs.check_ready("C2")
            self.assertIn("docker pull", str(ctx.exception))
            configs._image_present = lambda: True
            configs.check_ready("C2")
        finally:
            configs._image_present = real
        configs.check_ready("C3")

    def test_c2_rule_lists_exactly_the_tools_of_c3(self):
        self.assertEqual(configs.C3_TOOLS, sorted(set(configs.TOOLS_850) | set(configs.TOOLS_810) | {"edi.extract_note"}))
        self.assertEqual(len(configs.C3_TOOLS), 11)


class Faults(unittest.TestCase):
    def label(self, n):
        return {"isa13": f"{n:09d}"}

    def test_variants_follow_the_document_number(self):
        self.assertEqual(faults.plan("F1", self.label(3)).variant, "repeat1")
        self.assertEqual(faults.plan("F1", self.label(4)).variant, "repeat3")
        self.assertEqual(faults.plan("F2", self.label(3)).variant, "mock")
        self.assertEqual(faults.plan("F2", self.label(4)).variant, "exit")
        self.assertEqual(faults.plan("F2", self.label(4)).chaos_env, {"EDIGATE_CHAOS_EXIT_BEFORE_SEND": "1"})
        self.assertEqual(faults.plan("F2", self.label(4), no_relay=True).variant, "mock")   # nothing to kill in C0
        self.assertEqual([faults.plan("F7", self.label(n)).variant for n in (3, 4, 5)], ["R", "P", "none"])
        self.assertEqual([faults.plan("F5", self.label(n)).variant for n in (3, 4, 5)], ["order", "space", "enum"])
        with self.assertRaises(HarnessError):
            faults.plan("F9", self.label(1))

    def test_perturbation_keeps_the_meaning(self):
        args = {"source_document_id": "d1"}
        self.assertEqual(list(faults.perturb("erp.create_sales_order", {"a": 1, "b": 2}, "order")), ["b", "a"])
        self.assertEqual(faults.perturb("erp.create_sales_order", args, "space"), args)
        self.assertEqual(faults.perturb("erp.create_sales_order", args, "enum"), {"source_document_id": "d1", "delivery_instruction": "none"})
        # the invoice tool has no optional member: only the order of keys changes
        self.assertEqual(faults.perturb("erp.post_vendor_invoice", args, "enum"), args)


class Documents(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        """The raw bytes of the documents of the pilot corpus (seed 2), built in memory."""
        _, files = common.corpus(2)
        rows = [json.loads(line) for line in files["corpus.jsonl"].decode().splitlines()]
        cls.raw = {r["doc_id"]: base64.b64decode(r["raw_b64"]) for r in rows}

    def test_address_document_changes_only_the_receiver(self):
        raw = self.raw["d003"]
        out = address_document(raw, "ZZTESTGW2")
        self.assertEqual(len(out), len(raw) - 4)     # GS03 is 4 characters shorter; ISA08 is padded to 15
        self.assertEqual(out[:3], b"ISA")
        self.assertEqual(out[105:106], raw[105:106])  # the segment terminator is still at byte 106
        self.assertIn(b"*ZZTESTGW2      *", out[:106])
        self.assertNotIn(b"ZZTESTGATEWAY", out)
        self.assertEqual(raw.replace(b"ZZTESTGATEWAY  ", b"ZZTESTGW2      ").replace(b"ZZTESTGATEWAY", b"ZZTESTGW2"), out)

    def test_new_exchange_changes_only_control_numbers(self):
        raw = self.raw["d003"]
        out = faults.renumber(raw, 503)
        self.assertEqual(len(out), len(raw) + 4)     # GS06 and GE02 grow from one digit to three
        a, b = raw.decode().split(raw.decode()[105]), out.decode().split(raw.decode()[105])
        changed = [x[:3] for x, y in zip(a, b) if x != y]
        self.assertEqual(changed, ["ISA", "GS*", "GE*", "IEA"])


class Selection(unittest.TestCase):
    def test_the_smoke_picks_an_invoice_without_cur_and_does_not_filter_them(self):
        """Wave 2-9c: CUR is optional for the gateway, so the smoke takes 810 documents as they come and keeps one without CUR."""
        _, files = common.corpus(2)
        with tempfile.TemporaryDirectory() as d:
            for name in ("labels.jsonl", "corpus.jsonl", "fixtures.json", "deciding.json"):
                Path(d, name).write_bytes(files[name])
            corpus = Corpus(d)
            ids = corpus.stratified(20)
            invoices = [i for i in ids if corpus.labels[i]["set"] == "810" and corpus.labels[i]["class"] == "benign"]
            self.assertTrue(invoices)
            self.assertTrue(any(not corpus.has_currency(i) for i in invoices), invoices)
            self.assertEqual(ids, corpus.stratified(20))       # the choice is the same every time


class OperatorByLabel(unittest.TestCase):
    ORDER_LABEL = {"expected_benign_outcome": [
        {"kind": "sales_order", "business_ref": "PO01-0001", "partner_id": "ZZ:ZZTESTBUY01", "ship_to": "B01-DC1",
         "lines": [{"sku": "SKU-0001", "qty": 10, "unit_price": "3.50"}], "total": "35.00"},
        {"kind": "confirm_855", "ack_type": "AC"}]}

    def request(self, **over):
        args = {"partner_id": "ZZ:ZZTESTBUY01", "ship_to": "B01-DC1", "total": "35.00",
                "lines": [{"sku": "SKU-0001", "qty": "10", "unit": "EA", "unit_price": "3.50"}]}
        args.update(over)
        return {"kind": "effect", "view": {"tool": "erp.create_sales_order", "args": args}}

    def test_approves_the_matching_request(self):
        self.assertEqual(operator.decide(self.request(), self.ORDER_LABEL, "by-label"), "approve")

    def test_rejects_a_request_that_differs(self):
        self.assertEqual(operator.decide(self.request(ship_to="ZZDC-EVIL9"), self.ORDER_LABEL, "by-label"), "reject")
        self.assertEqual(operator.decide(self.request(total="3500.00"), self.ORDER_LABEL, "by-label"), "reject")
        wrong_lines = [{"sku": "SKU-0001", "qty": "1000", "unit": "EA", "unit_price": "3.50"}]
        self.assertEqual(operator.decide(self.request(lines=wrong_lines), self.ORDER_LABEL, "by-label"), "reject")

    def test_rejects_when_nothing_is_expected(self):
        self.assertEqual(operator.decide(self.request(), {"expected_benign_outcome": []}, "by-label"), "reject")

    def test_steps_of_the_saga_follow_the_expected_kinds(self):
        confirm = {"kind": "effect", "view": {"tool": "erp.confirm_order", "args": {}}}
        send = {"kind": "effect", "view": {"tool": "edi.send_asn", "args": {}}}
        self.assertEqual(operator.decide(confirm, self.ORDER_LABEL, "by-label"), "approve")
        self.assertEqual(operator.decide(send, self.ORDER_LABEL, "by-label"), "reject")   # no 856 expected in this label

    def test_a_review_request_is_rejected_by_label(self):
        self.assertEqual(operator.decide(dict(self.request(), kind="review"), self.ORDER_LABEL, "by-label"), "reject")

    def test_approve_all_approves_everything(self):
        for req in (self.request(ship_to="ZZDC-EVIL9"), dict(self.request(), kind="review")):
            self.assertEqual(operator.decide(req, self.ORDER_LABEL, "approve-all"), "approve")
        self.assertEqual(operator.decide(self.request(), {"expected_benign_outcome": []}, "approve-all"), "approve")


if __name__ == "__main__":
    unittest.main()
