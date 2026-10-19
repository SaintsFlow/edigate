"""The harness operator (protocol section 4): decides the approval requests of a slot while an episode runs.

Mode `approve-all` approves every request (a request of tier T3 gets two different operators). It decides H1.
Mode `by-label` approves a request only if its typed arguments equal `expected_benign_outcome` of the label and
rejects it otherwise. It is the sensitivity analysis, and it is the mode of the fault episodes.

Input: the URL of the slot, the tokens of operators 1 and 2, the label of the document.
Output: `Operator.requested` (the approval ids it saw) and `Operator.log` (what it decided).
The operator reads only `view` of a request (tool, tier, rule names, typed arguments): the gateway never shows it
the text of a note, as control M7 says. It polls `GET /ops/approvals?status=pending` every 0.5 s in a thread.
"""
import threading

import httpx

from metrics.oracles import dec

POLL_SECONDS = 0.5


def _lines(lines):
    return sorted((l.get("sku"), dec(l.get("qty")), dec(l.get("unit_price"))) for l in lines or [])


def matches(view, label):
    """True if the typed arguments of the request equal what the label expects."""
    expected = {item["kind"]: item for item in label["expected_benign_outcome"]}
    if not expected:
        return False
    tool, args = view.get("tool"), view.get("args") or {}
    if tool == "erp.create_sales_order":
        so = expected.get("sales_order")
        return so is not None and args.get("partner_id") == so["partner_id"] and args.get("ship_to") == so["ship_to"] \
            and dec(args.get("total")) == dec(so["total"]) and _lines(args.get("lines")) == _lines(so["lines"])
    if tool == "erp.post_vendor_invoice":
        inv = expected.get("vendor_invoice")
        return inv is not None and args.get("supplier_id") == inv["partner_id"] and dec(args.get("total")) == dec(inv["total"])
    if tool == "erp.confirm_order":
        return "confirm_855" in expected
    if tool == "wms.reserve":
        return "reservation" in expected
    if tool == "edi.send_asn":
        return "asn_856" in expected
    if tool == "notify.send_email":
        return "email" in expected
    return False


def decide(approval, label, mode):
    """'approve' or 'reject' for one request."""
    if mode == "approve-all":
        return "approve"
    if approval.get("kind") != "effect":
        return "reject"
    return "approve" if matches(approval.get("view") or {}, label) else "reject"


class Operator:
    def __init__(self, url, token1, token2, label, mode):
        self.url, self.label, self.mode = url, label, mode
        self.tokens = (token1, token2)
        self.requested = set()
        self.log = []
        self.errors = []
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, daemon=True)

    def start(self):
        self._thread.start()

    def stop(self):
        self._stop.set()
        self._thread.join(timeout=10)

    def _call(self, method, path, token):
        r = httpx.request(method, self.url + path, headers={"Authorization": f"Bearer {token}"}, timeout=15)
        return r

    def _run(self):
        while not self._stop.is_set():
            try:
                self.poll()
            except httpx.HTTPError as e:   # the gateway may be restarting (fault episodes)
                self.errors.append(str(e)[:120])
            self._stop.wait(POLL_SECONDS)

    def poll(self):
        r = self._call("GET", "/ops/approvals?status=pending", self.tokens[0])
        if r.status_code != 200:
            self.errors.append(f"list: HTTP {r.status_code}")
            return
        for a in r.json().get("approvals", []):
            self.requested.add(a["id"])
            action = decide(a, self.label, self.mode)
            decided = a.get("decided_by") or []
            # the first operator decides; for two approvers the second operator adds the second approval
            token = self.tokens[1] if (action == "approve" and decided) else self.tokens[0]
            r = self._call("POST", f"/ops/approvals/{a['id']}/{action}", token)
            self.log.append({"id": a["id"], "tool": (a.get("view") or {}).get("tool"), "action": action, "http": r.status_code})
