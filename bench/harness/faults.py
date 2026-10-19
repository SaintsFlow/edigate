"""Fault schedules F1 to F8 (protocol section 6) and the scripted agent that runs them.

A fault episode has no model. The scripted agent follows the plan of the honest fake (fakes.py), a fixed call
sequence derived from the label, and the plan of the fault changes what happens around it:

  F1  the answer of the first call with an effect is lost: the agent drops it and repeats the call 1 or 3 times
      (odd or even document number). This is what a proxy that drops the response would cause; the agent does
      the repeat itself because the gateway cannot tell the difference.
  F2  the effect is lost between commit and send: even number, the gateway process exits after the row is chosen
      (EDIGATE_CHAOS_EXIT_BEFORE_SEND=1, one send) and starts again after 5 s; odd number, mock-1c of the tenant
      loses 30 % of the answers after the commit (`fail_after_commit_rate`). Configurations without a relay
      (C0, C1, C2) have nothing to kill and always take the mock variant.
  F3  the same exchange sent again after the first MDN (even number: the same bytes; odd: the same ISA13 with an
      altered body).
  F4  the same order in a new exchange (new ISA13 and GS06) 2 s later; the agent handles both documents.
  F5  after the lost answer the retry differs in a harmless way (key order, an optional enum value).
  F6  the partner emulator drops every MDN of the slot (`mdn_drop` 1).
  F7  document number modulo 3: the partner answers AK5 = R, AK9 = P with mixed verdicts, or sends no 997.
  F8  `kill -9` of the process of the slot after the control numbers are given out, a restart, then the 997.
F6 to F8 run in C3 and the ablations only.

The scripted agent retries a call that failed with a closed connection (the process is down: it waits for the restart
and opens a new session) and a call that answered `internal` (twice), as a client library would.
"""
import asyncio
import json
import time
from dataclasses import dataclass

from edi_inject import x12

from . import agent, fakes
from .stand import HarnessError

FAULTS = ("F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8")
RECEIPT_FAULTS = {"F6", "F7", "F8"}
EFFECT_TOOLS = {"erp.create_sales_order", "erp.post_vendor_invoice"}
RESTART_AFTER = 5.0
ALIVE_WAIT = 60.0


# --- helpers for documents --------------------------------------------------------------------------------


def _split(raw):
    text = raw.decode("utf-8")
    return text[3], text[105], text.split(text[105])


def renumber(raw, number):
    """The same interchange with new control numbers: ISA13 and IEA02, GS06 and GE02 (a new exchange of the same order)."""
    es, term, segs = _split(raw)
    out = []
    for seg in segs:
        parts = seg.split(es)
        if parts[0] == "ISA":
            parts[13] = f"{number:09d}"
        elif parts[0] == "IEA":
            parts[2] = f"{number:09d}"
        elif parts[0] == "GS":
            parts[6] = str(number)
        elif parts[0] == "GE":
            parts[2] = str(number)
        out.append(es.join(parts))
    return term.join(out).encode("utf-8")


def alter_body(raw):
    """The same ISA13 with another body: the date of the ISA changes (ISA09)."""
    if b"*261019*" not in raw[:120]:
        raise HarnessError("cannot alter the body: no ISA09 date of the corpus")
    return raw.replace(b"*261019*", b"*261020*", 1)


def patch_ack(raw, gs06, st02):
    """A 997 of the corpus with the numbers of the outgoing document it answers (AK102 = GS06, AK202 = ST02)."""
    es, term, segs = _split(raw)
    out = []
    for seg in segs:
        parts = seg.split(es)
        if parts[0] == "AK1":
            parts[2] = str(gs06)
        elif parts[0] == "AK2":
            parts[2] = str(st02)
        out.append(es.join(parts))
    return term.join(out).encode("utf-8")


def build_ack(ak5, label, gs06, st02, number):
    """A new 997 with the verdict `ak5` for the outgoing document, from the partner of the label."""
    segments = x12.segments_997({"ak5": ak5, "ack_target": label["ack_target"]})
    raw = x12.build_interchange(label["partner_id"].split(":")[1], number, "997", segments)
    return patch_ack(raw, gs06, st02)


def doc_number(label):
    return int(label["isa13"])


# --- the plans -----------------------------------------------------------------------------------------------


@dataclass
class Context:
    slot: object
    label: dict
    corpus: object
    document_id: str
    log: object
    poll_sleep: float
    task_url: str
    restarts: int = 0


@dataclass
class Plan:
    fault: str
    variant: str
    chaos_env: dict = None
    chaos_exit: bool = False
    waits_for_receipts: bool = True

    def prepare(self, slot):
        """Knobs set before the document arrives."""
        if self.fault == "F2" and self.variant == "mock":
            slot.set_knobs("onec", fail_after_commit_rate=0.3)
        elif self.fault == "F6":
            slot.set_knobs("partner", mdn_drop=1)
        elif self.fault == "F7":
            slot.set_knobs("partner", ack_status=self.variant)
        elif self.fault == "F8":
            slot.set_knobs("partner", ack_status="none")   # the harness sends the 997 itself after the restart

    async def run(self, ctx):
        return await ScriptedAgent(self, ctx).run()


def plan(fault, label, no_relay=False):
    """The plan of a fault for a document. Raises HarnessError for an unknown fault."""
    if fault not in FAULTS:
        raise HarnessError(f"unknown fault {fault!r} (want one of {', '.join(FAULTS)})")
    n = doc_number(label)
    if fault == "F1":
        return Plan(fault, "repeat1" if n % 2 else "repeat3")
    if fault == "F2":
        if no_relay or n % 2:
            return Plan(fault, "mock")
        return Plan(fault, "exit", chaos_env={"EDIGATE_CHAOS_EXIT_BEFORE_SEND": "1"}, chaos_exit=True)
    if fault == "F3":
        return Plan(fault, "same" if n % 2 == 0 else "altered")
    if fault == "F5":
        return Plan(fault, ("order", "space", "enum")[n % 3])
    if fault == "F7":
        return Plan(fault, ("R", "P", "none")[n % 3])
    return Plan(fault, "")


# --- the scripted agent -------------------------------------------------------------------------------------


class ScriptedAgent:
    def __init__(self, plan, ctx):
        self.plan, self.ctx = plan, ctx
        self.result = agent.AgentResult()
        self.effect_done = False
        self.client = None
        self.tools = None

    async def run(self):
        ctx, slot = self.ctx, self.ctx.slot
        loop = asyncio.get_running_loop()
        watcher = None
        try:
            if self.plan.fault in ("F3",):
                await loop.run_in_executor(None, self._resend)
            if self.plan.fault == "F2" and self.plan.variant == "exit":
                watcher = asyncio.create_task(self._restart_when_dead())
            if self.plan.fault == "F8":
                watcher = asyncio.create_task(self._kill_after_numbers())
            async with agent.RawClient(ctx.task_url, slot.tokens["agent"]) as client:
                self.client = client
                self.tools = await client.list_tools()
                ids = [ctx.document_id]
                if self.plan.fault == "F4":
                    await asyncio.sleep(2.0)
                    second = await loop.run_in_executor(None, self._send_new_exchange)
                    ids.append(second)
                for doc_id in ids:
                    await self._handle(doc_id)
            if self.plan.fault == "F8":
                await loop.run_in_executor(None, self._send_acks_after_restart)
        except Exception as e:
            self.result.end_reason = "model_error"
            self.result.error = f"scripted agent: {type(e).__name__}: {e}"[:300]
        finally:
            if watcher is not None:
                watcher.cancel()
        return self.result

    # -- the call sequence of one document ---------------------------------------------------

    async def _handle(self, doc_id):
        ctx = self.ctx
        label = ctx.label
        model = fakes.FakeModel("honest", label, ctx.corpus.fixtures, doc_id, self.tools, ctx.poll_sleep)
        gen = model.plan
        try:
            step = next(gen)
            while True:
                name, args = step
                if name == "sleep":
                    await asyncio.sleep(ctx.poll_sleep)
                    answer = {}
                else:
                    answer = await self._call(name, args)
                step = gen.send(answer)
        except StopIteration:
            pass

    async def _call(self, name, args):
        """One call of the plan. The first call with an effect is made again and again as F1 and F5 say."""
        first_effect = name in EFFECT_TOOLS and not self.effect_done and self.plan.fault in ("F1", "F5")
        if name in EFFECT_TOOLS:
            self.effect_done = True
        if first_effect:
            await self._raw(name, args)                        # the answer is lost: dropped
            reps = 3 if self.plan.variant == "repeat3" else 1
            for i in range(reps):
                f5 = self.plan.fault == "F5"
                text = await self._raw(name, perturb(name, args, self.plan.variant) if f5 else args, spaced=f5 and self.plan.variant == "space")
            return fakes.parse_result(text)
        return fakes.parse_result(await self._raw(name, args))

    async def _raw(self, name, args, spaced=False):
        """A call with the retries of a client library. Returns the text of the answer."""
        loop = asyncio.get_running_loop()
        text = ""
        for attempt in range(6):
            self.result.steps += 1
            self.result.calls.append({"tool": name, "args": args})
            self.ctx.log.write(kind="call", tool=name, args=args, attempt=attempt)
            try:
                text, error = await self.client.call(name, args, spaced)
            except Exception as e:      # the process is down: wait for the restart, open a new session
                self.ctx.log.write(kind="result", tool=name, error=str(e)[:200])
                alive = await loop.run_in_executor(None, self._wait_alive)
                if not alive:
                    raise HarnessError("the gateway did not come back")
                await self.client.reconnect()
                continue
            self.ctx.log.write(kind="result", tool=name, error=error, text=text[:4000])
            if error and error["reason"] == "internal" and attempt < 2:
                continue
            if error and error["reason"] in agent.REFUSALS:
                self.result.refusals += 1
            return text
        return text

    # -- things that happen to the gateway -----------------------------------------------------

    def _wait_alive(self):
        slot = self.ctx.slot
        end = time.time() + ALIVE_WAIT
        while time.time() < end:
            if slot.alive():
                try:
                    slot.wait_healthy(5)
                    return True
                except HarnessError:
                    pass
            time.sleep(0.5)
        return False

    def _resend(self):
        """F3: the same exchange again, right after the first MDN."""
        ctx = self.ctx
        label = ctx.label
        raw = ctx.corpus.raw(label["doc_id"])
        if self.plan.variant == "altered":
            raw = alter_body(raw)
        status, body = ctx.slot.send_document(raw, label["partner_id"])
        ctx.log.write(kind="resend", variant=self.plan.variant, http=status, body=body[:200])

    def _send_new_exchange(self):
        """F4: the same order with new ISA13 and GS06. Returns the id of the new document in the inbox."""
        ctx = self.ctx
        label = ctx.label
        raw = renumber(ctx.corpus.raw(label["doc_id"]), doc_number(label) + 500)
        status, body = ctx.slot.send_document(raw, label["partner_id"])
        ctx.log.write(kind="new_exchange", http=status)
        text, error = asyncio.run(self._inbox_once())
        docs = json.loads(text).get("documents", []) if text else []
        ids = [d["id"] for d in docs if d["id"] != ctx.document_id]
        if not ids:
            raise HarnessError("the second exchange of F4 did not reach the inbox")
        return ids[0]

    async def _inbox_once(self):
        async with agent.RawClient(self.ctx.task_url, self.ctx.slot.tokens["agent"]) as c:
            return await c.call("edi.list_inbox", {})

    async def _restart_when_dead(self):
        """F2 with the kill: when the process has exited, wait 5 s and start it again without chaos."""
        slot = self.ctx.slot
        loop = asyncio.get_running_loop()
        while slot.alive():
            await asyncio.sleep(0.2)
        await asyncio.sleep(RESTART_AFTER)
        await loop.run_in_executor(None, slot.start, slot.mode)
        self.ctx.restarts += 1

    async def _kill_after_numbers(self):
        """F8: after the first outgoing document has its control numbers, kill -9 the process and start it again."""
        slot = self.ctx.slot
        loop = asyncio.get_running_loop()
        while True:
            n = await loop.run_in_executor(None, lambda: slot.slot_conn.execute("SELECT count(*) FROM outbound_docs").fetchone()[0])
            if n:
                break
            await asyncio.sleep(0.05)
        slot.kill9()
        self.ctx.log.write(kind="kill9", outbound_docs=n)
        await asyncio.sleep(2.0)
        await loop.run_in_executor(None, slot.start, slot.mode)
        self.ctx.restarts += 1

    def _send_acks_after_restart(self):
        """F8: after the restart the partner sends a 997 (accepted) for every outgoing document that has none."""
        ctx, slot = self.ctx, self.ctx.slot
        end = time.time() + 30
        while time.time() < end and slot.open_work()[0]:
            time.sleep(0.5)
        rows = slot.slot_conn.execute(
            "SELECT st01, gs06, st02, partner_id FROM outbound_docs WHERE ack_status IS NULL ORDER BY created_at").fetchall()
        for i, (st01, gs06, st02, partner) in enumerate(rows):
            fake_label = dict(ctx.label, ack_target=st01, partner_id=partner)
            raw = build_ack("A", fake_label, gs06, st02, number=900 + i)
            status, body = slot.send_document(raw, partner)
            ctx.log.write(kind="ack", http=status, st01=st01)
            time.sleep(0.6)


def perturb(name, args, variant):
    """F5: the arguments of the retry, changed in a way that does not change their meaning. Variants by the
    number of the document: `order` (the keys in the other order), `space` (white space in the JSON, sent by
    the client, the arguments are the same) and `enum` (an optional enum value written out: delivery_instruction
    "none", which is what an absent one means). Calls that have no optional member keep only the key order."""
    out = dict(args)
    if variant == "enum" and name == "erp.create_sales_order" and "delivery_instruction" not in out:
        out["delivery_instruction"] = "none"
    if variant == "order" or (variant == "enum" and out == args):
        out = {k: out[k] for k in reversed(list(out))}
    return out
