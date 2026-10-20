"""The generator is deterministic, and the command line refuses what it does not support.

Run: python3 -I bench/tests/test_determinism.py
The runs go through the real command (python -m edi_inject.generate) into temporary
folders inside bench/out/.
"""
import hashlib
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import common  # noqa: E402

OUT = common.BENCH / "out"


def run(args):
    return subprocess.run([sys.executable, "-m", "edi_inject.generate", *args], cwd=common.BENCH,
                          capture_output=True, text=True, timeout=120)


def doc_hashes(folder):
    return {hashlib.sha256(p.read_bytes()).hexdigest() for p in (folder / "docs").glob("*.edi")}


class Determinism(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        OUT.mkdir(exist_ok=True)

    def test_same_seed_same_bytes(self):
        with tempfile.TemporaryDirectory(dir=OUT) as tmp:
            a, b = Path(tmp) / "a", Path(tmp) / "b"
            ra, rb = run(["--n", "300", "--seed", "1", "--out", str(a)]), run(["--n", "300", "--seed", "1", "--out", str(b)])
            self.assertEqual((ra.returncode, rb.returncode), (0, 0), ra.stderr + rb.stderr)
            self.assertEqual((a / "SHA256SUMS").read_bytes(), (b / "SHA256SUMS").read_bytes())
            self.assertEqual(ra.stdout, rb.stdout)
            digest = hashlib.sha256((a / "corpus.jsonl").read_bytes()).hexdigest()
            self.assertEqual(ra.stdout.strip(), f"corpus sha256 {digest}")
            sums = (a / "SHA256SUMS").read_text().splitlines()
            self.assertEqual(len(sums), 305)  # 300 documents and five files
            self.assertEqual([line.split("  ", 1)[1] for line in sums], sorted(line.split("  ", 1)[1] for line in sums))

    def test_other_seed_other_corpus(self):
        with tempfile.TemporaryDirectory(dir=OUT) as tmp:
            a, b = Path(tmp) / "a", Path(tmp) / "b"
            self.assertEqual(run(["--n", "300", "--seed", "1", "--out", str(a)]).returncode, 0)
            self.assertEqual(run(["--n", "300", "--seed", "2", "--out", str(b)]).returncode, 0)
            self.assertNotEqual((a / "corpus.jsonl").read_bytes(), (b / "corpus.jsonl").read_bytes())
            self.assertEqual(doc_hashes(a) & doc_hashes(b), set())

    def test_in_memory_build_matches_files(self):
        built, files = common.corpus(1)
        with tempfile.TemporaryDirectory(dir=OUT) as tmp:
            self.assertEqual(run(["--n", "300", "--seed", "1", "--out", tmp]).returncode, 0)
            self.assertEqual((Path(tmp) / "SHA256SUMS").read_bytes(), files["SHA256SUMS"])


class CommandLine(unittest.TestCase):
    def test_n_other_than_300_is_refused(self):
        with tempfile.TemporaryDirectory(dir=OUT) as tmp:
            done = run(["--n", "100", "--seed", "1", "--out", str(Path(tmp) / "x")])
            self.assertEqual(done.returncode, 2)
            self.assertIn("--n 100 is not supported", done.stderr)
            self.assertEqual(done.stdout, "")
            self.assertFalse((Path(tmp) / "x").exists())


if __name__ == "__main__":
    unittest.main()
