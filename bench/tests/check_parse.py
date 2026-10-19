"""Runs `edigatectl parse` on every document of a corpus and compares with the labels.

    python3 -I bench/tests/check_parse.py <corpus directory> <path to edigatectl>

Expectation: exit code 0, envelope and set accepted, sender and ISA13 as in the label, for
every document except those labelled `outside_charset`; for those exit code 1, envelope
accepted, set rejected with an element error "invalid character" (AK403 = 6).
Prints a table set x expectation x result and exits 0 only if every document matched.
"""
import json
import subprocess
import sys
from collections import Counter
from pathlib import Path

INVALID_CHAR = "6"


def run_parse(tool, path):
    done = subprocess.run([tool, "parse", str(path)], capture_output=True, text=True, timeout=30)
    try:
        summary = json.loads(done.stdout.strip().splitlines()[-1])
    except (IndexError, json.JSONDecodeError):
        summary = {}
    return done.returncode, summary


def judge(label, code, summary):
    """Returns (expectation, result, matched)."""
    sets = [s for g in summary.get("groups", []) for s in g["sets"]]
    if label["outside_charset"]:
        errors = [e for s in sets for e in s.get("errors", [])]
        ok = (code == 1 and summary.get("envelope") == "accepted" and len(sets) == 1
              and sets[0]["status"] == "rejected" and any(e.get("ak403") == INVALID_CHAR for e in errors))
        return "rejected (invalid character)", "as expected" if ok else f"code {code}", ok
    ok = (code == 0 and summary.get("envelope") == "accepted" and len(sets) == 1
          and sets[0]["status"] == "accepted" and sets[0]["st01"] == label["set"]
          and summary.get("sender") == label["partner_id"] and summary.get("isa13") == label["isa13"])
    return "accepted", "accepted" if ok else f"code {code}, {summary.get('envelope')}", ok


def main(argv):
    if len(argv) != 3:
        print("usage: check_parse.py <corpus directory> <path to edigatectl>", file=sys.stderr)
        return 2
    corpus, tool = Path(argv[1]), argv[2]
    rows = [json.loads(line) for line in (corpus / "labels.jsonl").read_text().splitlines()]
    table = Counter()
    bad = []
    for label in rows:
        code, summary = run_parse(tool, corpus / "docs" / f"{label['doc_id']}.edi")
        expect, result, ok = judge(label, code, summary)
        table[(label["set"], expect, result)] += 1
        if not ok:
            bad.append(label["doc_id"])
    print(f"{'set':<5}{'expected':<30}{'result':<30}{'count':>5}")
    for (set_id, expect, result), count in sorted(table.items()):
        print(f"{set_id:<5}{expect:<30}{result:<30}{count:>5}")
    accepted = sum(c for (_, e, r), c in table.items() if e == "accepted" and r == "accepted")
    rejected = sum(c for (_, e, r), c in table.items() if e != "accepted" and r == "as expected")
    print(f"accepted {accepted}, rejected as expected {rejected}, mismatches {len(bad)}")
    if bad:
        print("mismatching documents:", " ".join(bad))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
