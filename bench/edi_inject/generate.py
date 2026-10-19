"""Command line of the EDI-Inject v1 generator.

    python -m edi_inject.generate --n 300 --seed 1 [--out out/corpus-seed1]

Input: the arguments above. Output: a directory with docs/<doc_id>.edi, corpus.jsonl,
labels.jsonl, fixtures.json, deciding.json, LICENSE.txt and SHA256SUMS, and one line
"corpus sha256 <hex>" on stdout (hash of corpus.jsonl). Exit 0 on success, 2 on bad arguments,
1 when a document cannot be built.

The same seed gives the same bytes on every run: one Random(seed), fixed orders, no dates of
the run. The bench stand and the metrics read fixtures.json and labels.jsonl.
"""
import argparse
import base64
import hashlib
import json
import random
import sys
from pathlib import Path

from . import content, fixtures, labels, plan, x12
from .transplant import TEMPLATES, Transplanter

LICENSE_TEXT = """EDI-Inject v1: corpus and labels

The documents, labels and seed data in this directory are licensed under the Creative Commons
Attribution 4.0 International licence (CC-BY-4.0), https://creativecommons.org/licenses/by/4.0/legalcode
Attribution: the EDI-Inject authors.

All data is synthetic. Partner identifiers carry the prefix ZZTEST and mail addresses are on .test domains.

The held-out documents contain text of attack templates of two projects under the MIT licence.
Their notices follow in full. Sources and commits: bench/edi_inject/templates/NOTICE.md of the generator.
"""
_LICENSE_FILES = (("InjecAgent", "injecagent/LICENCE"), ("AgentDojo", "agentdojo/LICENSE"))


def license_text():
    """LICENSE.txt of the corpus: the CC-BY-4.0 notice and both MIT notices, copied from the
    licence files of the templates."""
    parts = [LICENSE_TEXT]
    for name, path in _LICENSE_FILES:
        text = (TEMPLATES / path).read_text(encoding="utf-8").rstrip("\n")
        parts.append(f"\n{name}\n\n{text}\n")
    return "".join(parts)


def build_corpus(seed):
    """Plan, compose and label the 300 documents. Returns a dict with the pieces."""
    rng = random.Random(seed)
    fx = fixtures.build()
    specs = plan.make_plan(rng)
    held_exfiltration = sum(1 for s in specs if s.held_out and s.goal == "exfiltration")
    transplanter = Transplanter(rng, held_exfiltration)
    composed = {}
    for spec in specs:
        if spec.set != "997":
            composed[spec.doc_id] = content.compose(spec, rng, fx, spec.number, transplanter)
    for spec in specs:
        if spec.set == "997":
            partner = composed[spec.bound]["partner_id"]
            composed[spec.doc_id] = content.compose(spec, rng, fx, spec.number, transplanter, partner)
    deciding = plan.pick_deciding(rng, specs)
    recs = [labels.make_label(s, composed[s.doc_id]) for s in specs]
    return {"specs": specs, "composed": composed, "labels": recs, "fixtures": fx,
            "deciding": [s.doc_id for s in deciding]}


def _lines(rows):
    return ("\n".join(labels.dumps(r) for r in rows) + "\n").encode("utf-8")


def corpus_files(corpus):
    """{relative path: bytes} of every file of the output, SHA256SUMS last."""
    files = {}
    rows = []
    for spec in corpus["specs"]:
        raw = corpus["composed"][spec.doc_id]["raw"]
        files[f"docs/{spec.doc_id}.edi"] = raw
        rows.append({"doc_id": spec.doc_id, "set": spec.set, "raw_b64": base64.b64encode(raw).decode("ascii")})
    files["corpus.jsonl"] = _lines(rows)
    files["labels.jsonl"] = _lines(corpus["labels"])
    files["fixtures.json"] = (json.dumps(corpus["fixtures"], sort_keys=True, ensure_ascii=True, indent=2) + "\n").encode()
    files["deciding.json"] = (json.dumps(corpus["deciding"], ensure_ascii=True, indent=1) + "\n").encode()
    files["LICENSE.txt"] = license_text().encode("utf-8")
    sums = "".join(f"{hashlib.sha256(files[n]).hexdigest()}  {n}\n" for n in sorted(files))
    files["SHA256SUMS"] = sums.encode("utf-8")
    return files


def write_files(files, out):
    out = Path(out)
    for name, data in files.items():
        path = out / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)


def main(argv=None):
    parser = argparse.ArgumentParser(prog="edi_inject.generate", description="EDI-Inject v1 corpus generator")
    parser.add_argument("--n", type=int, default=300, help="number of documents; only 300 is supported")
    parser.add_argument("--seed", type=int, default=1, help="seed; 1 is the corpus of the main run, 2 the pilot")
    parser.add_argument("--out", help="output directory (default out/corpus-seed<seed>)")
    args = parser.parse_args(argv)
    if args.n != 300:
        print(f"error: --n {args.n} is not supported: the corpus has a fixed mix of 300 documents",
              file=sys.stderr)
        return 2
    out = args.out or f"out/corpus-seed{args.seed}"
    try:
        files = corpus_files(build_corpus(args.seed))
    except x12.GenerationError as err:
        print(f"error: {err}", file=sys.stderr)
        return 1
    write_files(files, out)
    print("corpus sha256", hashlib.sha256(files["corpus.jsonl"]).hexdigest())
    return 0


if __name__ == "__main__":
    sys.exit(main())
