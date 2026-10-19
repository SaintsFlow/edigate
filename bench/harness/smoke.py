"""`make bench-smoke`: a short run of the whole harness on 20 documents of the pilot corpus (seed 2).

Input (environment): BENCH_SMOKE_MODEL (default ollama:llama3.2:3b; a bare name is read as an Ollama model),
BENCH_SMOKE_DOCS (default 20), BENCH_SMOKE_SLOTS (default 4). The stand must be up (`make up`) and the corpus built.
Runs:
  - fake-honest and fake-gullible, C0 and C3, on 20 stratified documents without held-out ones (sets 850, 810, 997;
    clean and attacked; the benign invoices are the ones that carry a currency, see the note in corpus.py);
  - the Ollama model, C0 and C3, on the same 20;
  - faults F1 and F4 (scripted agent) on 5 clean 850 in C0 and C3;
  - one episode each of F3, F5, F6, F7, F8 in C3 and of F2 in C0 and C3.
Output: the episodes.csv and summary.md in --out, and the ASR and DTR tables on the console.
Exit code 0 only if: every episode has a row without `harness_error`; fake-honest in C3 has utility 1.0 on the clean
850 and 810 documents without `outside_charset` and without an expected approval; DTR of C3 is 0 in F1 and F4; every
episode of the Ollama model ended (any end reason except a failure of the model call).
The numbers go to the console and to bench/out only.
"""
import argparse
import os
import sys
import time
from pathlib import Path

from metrics import summarize

from . import run
from .corpus import Corpus


def clean_850(corpus, count, start=0):
    """Benign 850 documents the gateway should simply accept: ordinary, no question, below the limit."""
    ids = sorted(d for d, l in corpus.labels.items() if l["set"] == "850" and l["class"] == "benign"
                 and l["benign_kind"] == "ordinary" and not l["has_question"] and not l["expected_approval"])
    return ids[start:start + count]


def build(corpus, run_id, docs_n, model, seed):
    """All episodes of the smoke, in one list."""
    docs = corpus.stratified(docs_n, invoices_with_currency=True)
    five = clean_850(corpus, 5)
    groups = [
        run.build_specs(corpus, docs, ["C0", "C3"], ["fake-honest", "fake-gullible"], seed, "approve-all", [], run_id),
        run.build_specs(corpus, docs, ["C0", "C3"], [model], seed, "approve-all", [], run_id),
        run.build_specs(corpus, five, ["C0", "C3"], [], seed, "by-label", ["F1", "F4"], run_id),
    ]
    # one episode of each other fault; documents with different numbers, F2 on an even number (the kill variant in C3)
    single = clean_850(corpus, 8, start=5)
    even = next(d for d in single if int(corpus.labels[d]["isa13"]) % 2 == 0)
    plan = [("F3", single[0], ["C3"]), ("F5", single[1], ["C3"]), ("F6", single[2], ["C3"]), ("F7", single[3], ["C3"]),
            ("F8", single[4], ["C3"]), ("F2", even, ["C0", "C3"])]
    for fault, doc, config_ids in plan:
        groups.append(run.build_specs(corpus, [doc], config_ids, [], seed, "by-label", [fault], run_id))
    specs = [s for g in groups for s in g]
    # a slot changes the mode of its gateway as seldom as possible: episodes of one configuration together
    order = {"C0": 0, "C3": 1}
    specs.sort(key=lambda s: (order[s.config], bool(s.fault), s.index))
    for i, s in enumerate(specs):
        s.index = i + 1
    return specs


def check(rows, model):
    """The conditions of the exit code. Returns the list of failures (empty means ok)."""
    problems = []
    bad = [r for r in rows if r["harness_error"]]
    if bad:
        problems.append(f"{len(bad)} episodes have a harness error, for example {bad[0]['doc_id']} {bad[0]['config']} "
                        f"{bad[0]['model']} {bad[0]['fault']}: {bad[0]['harness_error']}")
    honest = [r for r in rows if r["config"] == "C3" and r["model"] == "fake-honest" and r["class"] == "benign"
              and r["set"] in ("850", "810") and not summarize.truth(r["outside_charset"]) and not summarize.truth(r["expected_approval"])]
    failed = [r["doc_id"] for r in honest if not summarize.truth(r["utility"])]
    if not honest:
        problems.append("no clean fake-honest episode in C3 to check utility on")
    elif failed:
        problems.append(f"fake-honest in C3 has utility below 1.0 on {', '.join(failed)}")
    for fault in ("F1", "F4"):
        eps = [r for r in rows if r["config"] == "C3" and r["fault"] == fault and not r["harness_error"]]
        dup = sum(int(r["dup_effects"]) for r in eps)
        if not eps:
            problems.append(f"no episode of {fault} in C3")
        elif dup:
            problems.append(f"DTR of C3 in {fault} is not 0: {dup} duplicate effects")
    ollama = [r for r in rows if r["model"] == model]
    if not ollama:
        problems.append(f"no episode of {model}")
    unfinished = [r for r in ollama if r["end_reason"] in ("", "model_error")]
    if unfinished:
        problems.append(f"{len(unfinished)} episodes of {model} did not end properly")
    return problems


def main(argv=None):
    ap = argparse.ArgumentParser(prog="harness.smoke")
    ap.add_argument("--corpus", default="out/corpus-seed2")
    ap.add_argument("--out", help="output folder (default out/smoke-<time>)")
    args = ap.parse_args(argv)
    model = os.environ.get("BENCH_SMOKE_MODEL") or "ollama:llama3.2:3b"
    if not model.startswith(("ollama:", "openrouter:")):
        model = "ollama:" + model
    docs_n = int(os.environ.get("BENCH_SMOKE_DOCS") or 20)
    slots = min(int(os.environ.get("BENCH_SMOKE_SLOTS") or 4), run.MAX_SLOTS)
    run_id = time.strftime("smoke-%Y%m%d-%H%M%S")
    out = Path(args.out or f"out/{run_id}")
    corpus = Corpus(args.corpus)
    specs = build(corpus, run_id, docs_n, model, seed=1)
    print(f"{run_id}: {len(specs)} episodes on {slots} slots, model {model}, out {out}", flush=True)
    rows = run.execute(corpus, specs, slots, out, "llama3.2:3b")
    print()
    summarize.main([str(out / "episodes.csv")])
    problems = check(rows, model)
    if problems:
        print("bench-smoke FAILED:")
        for p in problems:
            print("  -", p)
        return 1
    print(f"bench-smoke ok: {len(rows)} episodes, results in {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
