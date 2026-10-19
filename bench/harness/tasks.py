"""The flow of one episode (protocol sections 4 and 6): clean state, the document, the agent, the settle time, the metrics.

Input: an `EpisodeSpec` (document, configuration, model, seed, operator mode, fault) and a `Runner` that owns one slot.
Output: one dict per episode with the columns of episodes.csv, and the folder of the episode with the
logs (`harness_transport.jsonl` of the agent, `gateway_transport.jsonl` cut from the log of the slot, `gateway.log`
of the slot stays in the slot folder, `state.json` with the state the metrics were read from).

Order of one episode:
  1. start the gateway process of the slot in the mode of the configuration (a restart only when the mode changes);
  2. reset: empty tables, no rows of the tenant in the mocks, default knobs, fixtures, tokens, canary;
  3. send the document as the partner on /edi/inbound; the id comes from `edi.list_inbox` (a document the strict
     parser rejects is not in the inbox: the episode ends `rejected_at_intake` and the state is still read);
  4. run the agent (a model, a fake model, or the scripted agent of a fault) with the operator in the background;
  5. settle time: the outbox of the slot empty and the receipts in, then 5 s more, at most 30 s of waiting
     for the outbox; for faults about receipts longer (the shortened timeouts of the gateway);
  6. read the state of the mocks and of the gateway tables, compute the oracles, the utility and the duplicate counts.
For a 997 the agent first handles the bound 850 (T5), then the harness sends the 997 with the numbers of the
outgoing documents.
"""
import asyncio
import json
import time
import traceback
from dataclasses import dataclass
from pathlib import Path

from metrics import oracles, outcomes, snapshot

from . import agent, configs, faults, models
from .operator import Operator
from .stand import HarnessError

PROMPTS = Path(__file__).resolve().parents[1] / "prompts"
SETTLE_AFTER_QUIET = 5.0
OUTBOX_WAIT = 30.0
RECEIPT_WAIT = 90.0
RECEIPT_WAIT_FAULT = 150.0   # F6/F7 and the 997 episodes: three MDN repeats of 10 s, a 997 that never comes (120 s)
POLL_SLEEP = 1.0

COLUMNS = [
    "run_id", "index", "doc_id", "set", "class", "goal", "carrier_class", "technique", "held_out", "in_h1_pool",
    "expected_approval", "outside_charset", "config", "model", "seed", "operator", "fault", "end_reason", "steps",
    "prompt_tokens", "completion_tokens", "cost_usd", "wall_s", "goal_reached", "placebo_hits", "utility",
    "intake_rejected", "effects", "dup_effects", "lost_effects", "expected_effects", "ddr", "saga_states",
    "sagas_total", "sagas_closed", "saga_waiting_agent", "approvals_requested", "blocked_calls", "harness_error",
]


@dataclass
class EpisodeSpec:
    run_id: str
    index: int
    doc_id: str
    config: str
    model: str
    seed: int
    operator: str
    fault: str = ""


def system_prompt():
    return (PROMPTS / "system_v1.txt").read_text(encoding="utf-8").strip()


def task_text(set_id, document_id):
    return (PROMPTS / f"task_{set_id}.txt").read_text(encoding="utf-8").format(document_id=document_id).strip()


class Runner:
    """Runs the episodes of one slot, one after another."""

    def __init__(self, slot, corpus, out_dir, poll_sleep=POLL_SLEEP):
        self.slot, self.corpus, self.out_dir, self.poll_sleep = slot, corpus, Path(out_dir), poll_sleep

    # --- one episode ---------------------------------------------------------------------------

    def run(self, spec):
        label = self.corpus.labels[spec.doc_id]
        row = {c: "" for c in COLUMNS}
        row.update(run_id=spec.run_id, index=spec.index, doc_id=spec.doc_id, set=label["set"], **{"class": label["class"]},
                   goal=label["goal"] or "", carrier_class=label["carrier_class"], technique=label["technique"],
                   held_out=label["held_out"], in_h1_pool=label["in_h1_pool"], expected_approval=label["expected_approval"],
                   outside_charset=label["outside_charset"], config=spec.config, model=spec.model, seed=spec.seed,
                   operator=spec.operator, fault=spec.fault)
        name = f"{spec.index:05d}-{spec.doc_id}-{spec.config}-{spec.model.replace(':', '_')}" + (f"-{spec.fault}" if spec.fault else "")
        ep_dir = self.out_dir / "episodes" / name
        ep_dir.mkdir(parents=True, exist_ok=True)
        started = time.time()
        operator = None
        try:
            if label["held_out"]:
                raise HarnessError("a held-out document must not run in this wave")
            configs.check_ready(spec.config)
            if not spec.fault:
                models.check_ready(spec.model)
            if spec.config not in ("C3", "A1", "A2", "A3", "A4") and label["set"] == "997":
                raise HarnessError("set 997 runs only in C3 and the ablations (protocol section 2)")
            if spec.fault in faults.RECEIPT_FAULTS and configs.is_open(spec.config):
                raise HarnessError(f"{spec.fault} runs only in C3 and the ablations (protocol section 6)")
            cfg = configs.get(spec.config)
            plan = faults.plan(spec.fault, label) if spec.fault else None
            if plan and plan.chaos_exit and not configs.has_gateway_tables(spec.config):
                plan = faults.plan(spec.fault, label, no_relay=True)
            slot = self.slot
            chaos = plan.chaos_env if plan else None
            # the process is started again only when the mode changes, it died, or chaos is switched on or off
            if chaos or slot.chaos or not slot.alive() or slot.mode != cfg.mode:
                slot.start(cfg.mode, chaos=chaos)
            slot.reset(ep_dir)
            slot.use_proxy(spec.config == "C2")
            slot.mark_log()
            operator = Operator(slot.url, slot.tokens["op1"], slot.tokens["op2"], label, spec.operator)
            operator.start()
            log = agent.JsonlLog(ep_dir / "harness_transport.jsonl")
            try:
                result, wait_for_receipts = self._play(spec, label, plan, log, ep_dir)
            finally:
                log.close()
            self._settle(spec, label, wait_for_receipts)
            self._measure(spec, label, result, row, ep_dir)
        except (Exception, asyncio.CancelledError) as e:  # any failure of the harness is a row with harness_error, never a crash of the run
            row["harness_error"] = f"{type(e).__name__}: {e}"[:300]
            if not isinstance(e, HarnessError):
                row["harness_error"] += " [unexpected]"
                (ep_dir / "error.txt").write_text(traceback.format_exc(), encoding="utf-8")
        finally:
            if operator is not None:
                operator.stop()
                if row["approvals_requested"] == "":
                    row["approvals_requested"] = len(operator.requested)
            try:
                self.slot.cut_log(ep_dir / "gateway_transport.jsonl")
            except OSError:
                pass
            row["wall_s"] = round(time.time() - started, 1)
        (ep_dir / "row.json").write_text(json.dumps(row, default=str), encoding="utf-8")
        return row

    # --- steps -------------------------------------------------------------------------------

    def _play(self, spec, label, plan, log, ep_dir):
        """Sends the document and runs the agent. Returns (AgentResult, whether to wait for receipts)."""
        slot = self.slot
        eff = self.corpus.labels[label["bound_850"]] if label["set"] == "997" else label
        if label["set"] == "997":
            slot.set_knobs("partner", ack_status="none")   # the harness sends this 997 itself, with real numbers
        if plan:
            plan.prepare(slot)
        status, body = slot.send_document(self.corpus.raw(eff["doc_id"]), eff["partner_id"])
        log.write(kind="intake", http=status, body=body[:300])
        inbox = asyncio.run(self._inbox())
        if not inbox:
            return agent.AgentResult(end_reason="rejected_at_intake"), False
        document_id = inbox[0]["id"]
        if plan:
            ctx = faults.Context(slot=slot, label=label, corpus=self.corpus, document_id=document_id, log=log,
                                 poll_sleep=self.poll_sleep, task_url=slot.mcp_url)
            result = asyncio.run(plan.run(ctx))
            return result, plan.waits_for_receipts
        make_model = self._model_factory(spec, label, document_id)
        result = asyncio.run(agent.run_agent(slot.mcp_url, slot.tokens["agent"], make_model, system_prompt(),
                                             task_text(eff["set"], document_id), eff["set"], log))
        if label["set"] == "997":
            self._send_acks(label, eff, log)
        return result, configs.has_gateway_tables(spec.config)

    async def _inbox(self):
        async with agent.McpClient(self.slot.mcp_url, self.slot.tokens["agent"]) as c:
            text, error = await c.call("edi.list_inbox", {})
        if error:
            raise HarnessError(f"edi.list_inbox failed: {error}")
        return json.loads(text).get("documents", [])

    def _model_factory(self, spec, label, document_id):
        if models.is_fake(spec.model):
            return lambda tools: models.make_fake(spec.model, self.corpus.labels[label["bound_850"]] if label["set"] == "997" else label,
                                                  self.corpus.fixtures, document_id, tools, self.poll_sleep)
        return lambda tools: models.ApiModel(spec.model, spec.seed)

    def _send_acks(self, label, eff, log):
        """T5: after the bound 850 is handled, the partner sends the 997 with the numbers of the outgoing document."""
        slot = self.slot
        self._wait_quiet(OUTBOX_WAIT, receipts=False)
        row = slot.slot_conn.execute(
            "SELECT gs06, st02 FROM outbound_docs WHERE st01 = %s ORDER BY created_at LIMIT 1", (label["ack_target"],)).fetchone()
        if row is None:
            log.write(kind="ack", sent=False, reason="no outgoing document to answer")
            return
        gs06, st02 = row
        raws = []
        if label["ack_sequence"] == "after_accept":
            raws.append(faults.build_ack("A", label, gs06, st02, number=label_number(label) + 1000))
        raws.append(faults.patch_ack(self.corpus.raw(label["doc_id"]), gs06, st02))
        if label["ack_sequence"] == "repeat":
            raws.append(raws[-1])
        for raw in raws:
            status, body = slot.send_document(raw, label["partner_id"])
            log.write(kind="ack", sent=True, http=status)
            time.sleep(0.6)   # the bucket of a partner is two interchanges a second

    def _wait_quiet(self, limit, receipts, receipt_limit=RECEIPT_WAIT):
        """Waits until the outbox is empty (at most `limit` s) and, with receipts, no outgoing document waits for an MDN or a 997."""
        end = time.time() + limit
        while time.time() < end:
            outbox, docs = self.slot.open_work()
            if outbox == 0:
                break
            time.sleep(0.5)
        if receipts:
            end = time.time() + receipt_limit
            while time.time() < end:
                if self.slot.open_work()[1] == 0:
                    break
                time.sleep(0.5)

    def _settle(self, spec, label, receipts):
        """Waits for the outbox and the receipts, then the 5 s of the protocol. For a 997 the document that gets no
        acknowledgement waits for the 997 timeout (120 s after the time scale), so the wait is longer."""
        gateway = configs.has_gateway_tables(spec.config)
        if gateway:
            limit = RECEIPT_WAIT
            if spec.fault in faults.RECEIPT_FAULTS:
                limit = RECEIPT_WAIT_FAULT
            elif label["set"] == "997":
                limit = RECEIPT_WAIT_FAULT
            self._wait_quiet(OUTBOX_WAIT, receipts, limit)
        time.sleep(SETTLE_AFTER_QUIET)

    # --- metrics -------------------------------------------------------------------------------

    def _measure(self, spec, label, result, row, ep_dir):
        slot = self.slot
        gateway = configs.has_gateway_tables(spec.config)
        state = slot.state(gateway, result.calls)
        eff = self.corpus.labels[label["bound_850"]] if label["set"] == "997" else label
        templates = {t["template_id"]: t["category"] for t in self.corpus.fixtures["email_templates"]}
        (ep_dir / "state.json").write_text(json.dumps(state, default=str), encoding="utf-8")
        counts = outcomes.effect_counts(state, eff, templates)
        dup, lost, expected, ddr = outcomes.duplicates(counts)
        sagas = state["sagas"]
        row.update(
            end_reason=result.end_reason, steps=result.steps, prompt_tokens=result.prompt_tokens,
            completion_tokens=result.completion_tokens, cost_usd=round(result.cost_usd, 6),
            intake_rejected=result.end_reason == "rejected_at_intake",
            utility=outcomes.utility(state, eff, self.corpus.fixtures),
            effects=json.dumps({"|".join(map(str, k)): list(v) for k, v in sorted(counts.items(), key=str)}),
            dup_effects=dup, lost_effects=lost, expected_effects=expected, ddr=ddr,
            saga_states=";".join(f"{s['kind']}:{s['state']}" for s in sagas), sagas_total=len(sagas),
            sagas_closed=sum(s["state"] in ("done", "compensated") for s in sagas),
            saga_waiting_agent=sum(s["waiting_agent"] for s in sagas),
            approvals_requested=snapshot.approvals_count(slot.slot_conn) if gateway else 0,
            blocked_calls=result.refusals,
        )
        if result.error:
            row["harness_error"] = result.error if result.end_reason == "model_error" else ""
        if label["class"] == "attacked":
            reached = oracles.goal_reached(label, state)
            row["goal_reached"] = "" if reached is None else reached
        elif label["placebo_value"]:
            row["placebo_hits"] = ";".join(oracles.placebo_hits(label, state))
        elif label["set"] == "997":
            row["placebo_hits"] = "receipt" if oracles.receipt_reached(state) else ""


def label_number(label):
    return int(label["isa13"])
