"""Command line of the harness: runs episodes over the documents of a corpus and writes the results.

  cd bench && .venv/bin/python -m harness.run --corpus out/corpus-seed2 --docs deciding:10 --configs C0,C3 \
      --models fake-honest,ollama:llama3.2:3b --seed 1 --operator approve-all --slots 4 --out out/myrun
  ... --faults F1,F4 --docs <ids>   fault episodes (scripted agent, benign 850 and 810 only, operator by-label)

Input: the options above. Documents are `all`, `deciding:N`, `stratified:N` or a comma list of ids; documents with
`held_out: true` are dropped at load time and never run.
Output in --out: `episodes.csv` (one row per episode, written as they finish), `episodes/<name>/` (logs of the
harness and of the gateway, the state the metrics were read from), `summary.md` (metrics.summarize).
Exit code 0 if every episode produced a row without `harness_error`; 1 otherwise; 2 for a wrong command line.
A model or a configuration that cannot run here (no OPENROUTER_API_KEY, C2 without agentgateway) is skipped with a
message; that is not an error.
"""
import argparse
import csv
import queue
import sys
import threading
import time
from pathlib import Path

from metrics import summarize

from . import configs, faults, models
from .corpus import Corpus
from .stand import Slot
from .tasks import COLUMNS, EpisodeSpec, Runner

MAX_SLOTS = 4


def parse_args(argv):
    ap = argparse.ArgumentParser(prog="harness.run", description=__doc__.split("\n")[0])
    ap.add_argument("--corpus", required=True, help="folder of a corpus (out/corpus-seed2 for the pilot)")
    ap.add_argument("--docs", default="deciding:10", help="all | deciding:N | stratified:N | comma list of doc ids")
    ap.add_argument("--configs", default="C3", help="comma list of C0..C3, A1..A4")
    ap.add_argument("--models", default="fake-honest", help="fake-honest, fake-gullible, ollama:<name>, openrouter:<name>")
    ap.add_argument("--seed", type=int, default=1)
    ap.add_argument("--operator", default="approve-all", choices=("approve-all", "by-label"))
    ap.add_argument("--faults", default="", help="comma list of F1..F8 (scripted agent, no model)")
    ap.add_argument("--slots", type=int, default=1, help=f"parallel slots, 1 to {MAX_SLOTS}")
    ap.add_argument("--out", help="output folder (default out/run-<time>)")
    ap.add_argument("--run-id", help="name of the run (default: the time)")
    ap.add_argument("--quarantine-model", default="llama3.2:3b", help="model of the quarantine reader of the gateway ('' switches it off)")
    args = ap.parse_args(argv)
    if not 1 <= args.slots <= MAX_SLOTS:
        ap.error(f"--slots must be 1 to {MAX_SLOTS}")
    return args


def select_docs(corpus, spec):
    if spec.startswith("stratified:"):
        return corpus.stratified(int(spec.split(":", 1)[1]))
    return corpus.select(spec)


def build_specs(corpus, doc_ids, config_ids, model_ids, seed, operator, fault_ids, run_id):
    """The list of episodes, sorted so that a slot changes the mode of its gateway as seldom as possible."""
    specs = []
    for config in config_ids:
        for fault in fault_ids or [""]:
            for doc_id in doc_ids:
                label = corpus.labels[doc_id]
                if label["set"] == "997" and configs.is_open(config):
                    continue    # protocol section 2: set 997 is not an episode of C0, C1, C2
                if fault:
                    if label["class"] != "benign" or label["set"] == "997":
                        continue    # the fault set is the benign 850 and 810 documents
                    if fault in faults.RECEIPT_FAULTS and configs.is_open(config):
                        continue    # protocol section 6: F6 to F8 only in C3 and the ablations
                    specs.append((config, "scripted", fault, doc_id))
                    continue
                for model in model_ids:
                    specs.append((config, model, "", doc_id))
    return [EpisodeSpec(run_id, i + 1, d, c, m, seed, "by-label" if f else operator, f)
            for i, (c, m, f, d) in enumerate(specs)]


class CsvSink:
    """Writes rows to episodes.csv as they finish, from several threads."""

    def __init__(self, path):
        self.f = open(path, "w", newline="", encoding="utf-8")
        self.w = csv.DictWriter(self.f, fieldnames=COLUMNS)
        self.w.writeheader()
        self.f.flush()
        self.lock = threading.Lock()
        self.rows = []

    def add(self, row):
        with self.lock:
            self.w.writerow(row)
            self.f.flush()
            self.rows.append(row)

    def close(self):
        self.f.close()


def worker(n, work, corpus, out_dir, sink, quarantine_model, say):
    """One slot: takes episodes from the queue until it is empty."""
    slot = Slot(n, out_dir, corpus.fixtures, quarantine_model or None)
    try:
        slot.create()
        runner = Runner(slot, corpus, out_dir)
        while True:
            try:
                spec = work.get_nowait()
            except queue.Empty:
                return
            row = runner.run(spec)
            sink.add(row)
            say(f"[slot {n}] {spec.index:4d} {spec.doc_id} {spec.config} {spec.model}{(' ' + spec.fault) if spec.fault else ''}: "
                f"{row['end_reason'] or '-'} utility={row['utility']} goal={row['goal_reached']} {row['wall_s']}s"
                + (f"  ERROR {row['harness_error']}" if row["harness_error"] else ""))
    finally:
        slot.close()


def say_now(message):
    print(message, flush=True)


def execute(corpus, specs, slots, out_dir, quarantine_model, say=say_now):
    """Runs the episodes and returns the rows."""
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    sink = CsvSink(out_dir / "episodes.csv")
    work = queue.Queue()
    for s in specs:
        work.put(s)
    threads = [threading.Thread(target=worker, args=(n, work, corpus, out_dir, sink, quarantine_model, say), name=f"slot{n}")
               for n in range(1, min(slots, len(specs)) + 1)]   # no episodes: no slot is made
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    sink.close()
    return sink.rows


def main(argv=None):
    args = parse_args(argv if argv is not None else sys.argv[1:])
    run_id = args.run_id or time.strftime("%Y%m%d-%H%M%S")
    out_dir = Path(args.out or f"out/run-{run_id}")
    corpus = Corpus(args.corpus)
    doc_ids = select_docs(corpus, args.docs)
    fault_ids = [f for f in args.faults.split(",") if f]
    config_ids, model_ids = [], []
    for c in args.configs.split(","):
        try:
            configs.get(c)
            configs.check_ready(c)
            config_ids.append(c)
        except (ValueError, RuntimeError) as e:
            print(f"skipped configuration {c}: {e}")
    for m in args.models.split(","):
        try:
            models.check_ready(m)
            model_ids.append(m)
        except RuntimeError as e:
            print(f"skipped model {m}: {e}")
    specs = build_specs(corpus, doc_ids, config_ids, model_ids, args.seed, args.operator, fault_ids, run_id)
    print(f"run {run_id}: {len(specs)} episodes, {args.slots} slot(s), out {out_dir}")
    rows = execute(corpus, specs, args.slots, out_dir, args.quarantine_model)
    summarize.main([str(out_dir / "episodes.csv")])
    bad = [r for r in rows if r["harness_error"]]
    print(f"episodes: {len(rows)}, with a harness error: {len(bad)}")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
