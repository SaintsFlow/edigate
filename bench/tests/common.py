"""Helpers shared by the tests. Run a test as a file: python3 -I bench/tests/test_<name>.py.

Isolated mode (-I) does not put the script's folder on the path, so this module is imported
after the tests add their own folder; it then adds the bench folder for the package.
"""
import sys
from pathlib import Path

TESTS = Path(__file__).resolve().parent
BENCH = TESTS.parent
if str(BENCH) not in sys.path:
    sys.path.insert(0, str(BENCH))

from edi_inject import generate  # noqa: E402

_CACHE = {}


def corpus(seed=1):
    """The corpus of a seed, built once per test process: (corpus dict, files dict)."""
    if seed not in _CACHE:
        built = generate.build_corpus(seed)
        _CACHE[seed] = (built, generate.corpus_files(built))
    return _CACHE[seed]


def labels(seed=1):
    return corpus(seed)[0]["labels"]


def segments(raw):
    """The segments of a raw document as lists of elements, cut with the separators of its ISA."""
    text = raw.decode("utf-8")
    es, term = text[3], text[105]
    return [seg.split(es) for seg in text[106:].split(term) if seg]
