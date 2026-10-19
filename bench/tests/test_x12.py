"""The X12 writer: envelope numbers, separators and the position of dates.

Run: python3 -I bench/tests/test_x12.py
"""
import re
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import common  # noqa: E402
from edi_inject import x12  # noqa: E402


def docs():
    built, _ = common.corpus(1)
    for spec in built["specs"]:
        yield spec, built["composed"][spec.doc_id]["raw"]


class Envelope(unittest.TestCase):
    def test_isa_is_106_bytes_with_fixed_fields(self):
        for spec, raw in docs():
            self.assertTrue(raw.startswith(b"ISA"), spec.doc_id)
            es, term = raw[3:4], raw[105:106]
            isa = raw[:106].split(es)
            self.assertEqual(len(isa), 17, spec.doc_id)
            self.assertEqual(isa[16], raw[104:105] + term, spec.doc_id)
            self.assertEqual([len(f) for f in isa[1:16]], [2, 10, 2, 10, 2, 15, 2, 15, 6, 4, 1, 5, 9, 1, 1])
            self.assertEqual((isa[9], isa[10], isa[11], isa[12]), (b"261019", b"1200", b"U", b"00401"))
            self.assertEqual((isa[14], isa[15]), (b"0", b"T"))
            self.assertEqual(isa[8].strip(), b"ZZTESTGATEWAY")

    def test_counts_and_control_numbers(self):
        for spec, raw in docs():
            segs = common.segments(raw)
            isa13 = raw[:106].split(raw[3:4])[13].decode()
            self.assertEqual(isa13, f"{spec.number:09d}")
            gs, st = segs[0], segs[1]
            ids = [s[0] for s in segs]
            self.assertEqual(ids[:2], ["GS", "ST"])
            self.assertEqual(ids[-3:], ["SE", "GE", "IEA"])
            se, ge, iea = segs[-3:]
            self.assertEqual(int(se[1]), len(segs[1:-2]), spec.doc_id)  # ST up to SE, both counted
            self.assertEqual(se[2], st[2], spec.doc_id)
            self.assertEqual((ge[1], ge[2]), ("1", gs[6]), spec.doc_id)
            self.assertEqual(gs[6], str(spec.number), spec.doc_id)
            self.assertEqual((iea[1], iea[2]), ("1", isa13), spec.doc_id)
            self.assertEqual(st[2], "0001")
            self.assertEqual(ids.count("ST"), 1)

    def test_group_code_matches_set(self):
        for spec, raw in docs():
            self.assertEqual(common.segments(raw)[0][1], {"850": "PO", "810": "IN", "997": "FA"}[spec.set])
            self.assertEqual(common.segments(raw)[1][1], spec.set)


class Separators(unittest.TestCase):
    def build(self, text):
        segs = [["BEG", "00", "SA", "PO1", "", "20261018"], ["PID", "F", "", "", "", text]]
        return x12.build_interchange("ZZTESTBUY01", 7, "850", segs)

    def test_plain_text_takes_the_first_set(self):
        raw = self.build("plain text")
        self.assertEqual((raw[3:4], raw[104:105], raw[105:106]), (b"*", b">", b"~"))

    def test_tilde_in_text_moves_to_the_second_set(self):
        raw = self.build("about ~20 units")
        self.assertEqual((raw[3:4], raw[104:105], raw[105:106]), (b"|", b"^", b"\\"))
        self.assertIn(b"about ~20 units", raw)

    def test_pipe_in_text_keeps_the_first_set(self):
        raw = self.build("A|B")
        self.assertEqual((raw[3:4], raw[104:105], raw[105:106]), (b"*", b">", b"~"))

    def test_tilde_and_pipe_move_to_the_third_set(self):
        raw = self.build("~ and |")
        self.assertEqual((raw[3:4], raw[104:105], raw[105:106]), (b"!", b"@", b"`"))

    def test_no_free_set_is_an_error(self):
        with self.assertRaises(x12.GenerationError):
            self.build("~ | !")

    def test_chosen_separators_are_declared_sets_and_absent_from_values(self):
        for spec, raw in docs():
            used = (raw[3:4].decode(), raw[104:105].decode(), raw[105:106].decode())
            self.assertIn(used, x12.SEPARATOR_SETS, spec.doc_id)
            for seg in common.segments(raw):
                for element in seg:
                    self.assertFalse(any(c in element for c in used[1:]), spec.doc_id)

    def test_corpus_uses_more_than_one_set(self):
        used = {raw[3:4] for _, raw in docs()}
        self.assertGreaterEqual(len(used), 2)


class Dates(unittest.TestCase):
    def test_beg_has_the_date_in_beg05_and_no_beg04(self):
        found = 0
        for spec, raw in docs():
            if spec.set != "850":
                continue
            beg = next(s for s in common.segments(raw) if s[0] == "BEG")
            self.assertEqual(len(beg), 6, spec.doc_id)
            self.assertEqual(beg[4], "", spec.doc_id)
            self.assertTrue(re.fullmatch(r"2026\d{4}", beg[5]), spec.doc_id)
            found += 1
        self.assertEqual(found, 200)

    def test_big_dates(self):
        for spec, raw in docs():
            if spec.set == "810":
                big = next(s for s in common.segments(raw) if s[0] == "BIG")
                self.assertTrue(re.fullmatch(r"2026\d{4}", big[1]) and re.fullmatch(r"2026\d{4}", big[3]))


if __name__ == "__main__":
    unittest.main()
