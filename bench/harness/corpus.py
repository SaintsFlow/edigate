"""The corpus for the harness: labels, raw documents, fixtures, and the choice of documents for a run.

Input: a corpus folder made by `python -m edi_inject.generate` (labels.jsonl, corpus.jsonl, fixtures.json,
deciding.json).
Output: `Corpus` with `labels` (a dict by doc_id), `raw(doc_id)` and `select(spec)`.

Documents with `held_out: true` are never run in the pilot and the harness never decodes them: they are dropped
when the corpus is loaded unless `allow_held_out` is set (the main run of stage 3 sets it; this wave never does).
A 997 needs its bound 850, which is a benign document and never held out.
"""
import base64
import json
from pathlib import Path


class Corpus:
    def __init__(self, folder, allow_held_out=False):
        self.folder = Path(folder)
        self.fixtures = json.loads((self.folder / "fixtures.json").read_text(encoding="utf-8"))
        self.deciding = json.loads((self.folder / "deciding.json").read_text(encoding="utf-8"))
        self.labels = {}
        for line in (self.folder / "labels.jsonl").read_text(encoding="utf-8").splitlines():
            label = json.loads(line)
            if label["held_out"] and not allow_held_out:
                continue
            self.labels[label["doc_id"]] = label
        self._raw = {}
        for line in (self.folder / "corpus.jsonl").read_text(encoding="utf-8").splitlines():
            row = json.loads(line)
            if row["doc_id"] in self.labels:
                self._raw[row["doc_id"]] = row["raw_b64"]

    def raw(self, doc_id):
        return base64.b64decode(self._raw[doc_id])

    def has_currency(self, doc_id):
        """True if the document carries a CUR segment. CUR is optional: the gateway takes the currency of the
        partner or of the tenant for a document without it."""
        raw = self.raw(doc_id).decode("utf-8")
        es, term = raw[3], raw[105]
        return any(seg.startswith("CUR" + es) for seg in raw.split(term))

    def select(self, spec):
        """`all`, `deciding:N` (the first N of deciding.json that may run) or a comma list of doc ids."""
        if spec == "all":
            return sorted(self.labels)
        if spec.startswith("deciding:"):
            n = int(spec.split(":", 1)[1])
            return [d for d in self.deciding if d in self.labels][:n]
        ids = [s.strip() for s in spec.split(",") if s.strip()]
        missing = [d for d in ids if d not in self.labels]
        if missing:
            raise ValueError(f"documents not in the corpus or held out: {', '.join(missing)}")
        return ids

    def stratified(self, n):
        """n documents spread over (set, class): 850, 810 and 997, clean and attacked, deterministic.
        Within a stratum the picks are evenly spaced over documents sorted by goal, so the goals vary.
        Of the benign 810 documents one has no CUR segment, so that the default currency is reached."""
        weights = {("850", "benign"): 5, ("850", "attacked"): 5, ("810", "benign"): 2, ("810", "attacked"): 2,
                   ("997", "benign"): 3, ("997", "attacked"): 3}
        total = sum(weights.values())
        quota = {k: max(1, round(n * w / total)) for k, w in weights.items()}
        while sum(quota.values()) > n:
            quota[max(quota, key=quota.get)] -= 1
        while sum(quota.values()) < n:
            quota[min(quota, key=quota.get)] += 1
        out = []
        for key, q in quota.items():
            pool = sorted((l for l in self.labels.values() if (l["set"], l["class"]) == key),
                          key=lambda l: (str(l["goal"]), str(l["benign_kind"]), l["doc_id"]))
            picked = []
            tests = {
                # one document of each path the smoke must reach: a question (email), an expected approval, a rejected one
                ("850", "benign"): (lambda l: l["has_question"], lambda l: l["expected_approval"], lambda l: l["outside_charset"]),
                # an invoice without a currency
                ("810", "benign"): (lambda l: not self.has_currency(l["doc_id"]),),
            }.get(key, ())
            for test in tests:
                first = next((l for l in pool if test(l) and l not in picked), None)
                if first and len(picked) < q - 1:
                    picked.append(first)
            rest = [l for l in pool if l not in picked]
            need = q - len(picked)
            step = len(rest) / need
            picked += [rest[int(i * step)] for i in range(need)]
            out += [l["doc_id"] for l in picked]
        return out
