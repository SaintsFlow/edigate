"""The stand of the benchmark: slots, clean state before each episode, and the knobs of the mocks.

A slot (protocol section 6) is one database made with `edigatectl db create`, one process of
`bin/edigate` on the host with its own port, and one tenant `bench-s<N>`. The mocks and the
partner emulator are shared and keep the rows of the slots apart by tenant: mock-1c and mock-wms by the tenant id,
the partner emulator by the interchange id the gateway sends under (AS2-From).

Every slot has its own receiver id `ZZ:ZZTESTGW<N>` in place of the `ZZ:ZZTESTGATEWAY` of the corpus.
`address_document` rewrites ISA08 and GS03 of a document to it (the bytes stay valid X12; nothing else
changes), so the partner emulator can tell the transmissions of two slots apart.

Input: the stand must be up (`make up`: PostgreSQL 55432, mock-1c 18081, mock-wms 18082, partner emulator
18083) and `make build` done. Output: a `Slot` that the runner drives: `reset`, `start`, `kill9`,
`send_document`, `state`.
"""
import json
import os
import re
import signal
import socket
import subprocess
import time
from pathlib import Path

import httpx
import psycopg

from metrics import snapshot

from . import configs

EDIGATE = Path(__file__).resolve().parents[2]
BIN = EDIGATE / "bin"
ADMIN_URL = os.environ.get("BENCH_ADMIN_DB_URL") or "postgres://edigate:edigate@127.0.0.1:55432/edigate?sslmode=disable"
ONEC, WMS, PARTNER = "http://127.0.0.1:18081", "http://127.0.0.1:18082", "http://127.0.0.1:18083"
PARTNER_TOKENS = EDIGATE / "deploy/compose/stand/partner_tokens.json"
BASE_PORT = 18200
AG_BASE_PORT = 18300
TIME_SCALE = "720"
CORPUS_GATEWAY = b"ZZTESTGATEWAY"


class HarnessError(Exception):
    """A failure of the harness itself (not of the gateway or the model); it goes to harness_error."""


def slot_db_url(name):
    return re.sub(r"/edigate\?", f"/{name}?", ADMIN_URL)


def run_ctl(*args, db=None, stdin=None):
    """Runs edigatectl (on the database of a slot) and returns stdout; raises HarnessError on a nonzero code."""
    env = dict(os.environ, EDIGATE_DB_URL=db or ADMIN_URL)
    p = subprocess.run([str(BIN / "edigatectl"), *args], env=env, capture_output=True, text=True, timeout=60, input=stdin)
    if p.returncode != 0:
        raise HarnessError(f"edigatectl {' '.join(args[:3])} failed ({p.returncode}): {p.stderr.strip()[:300]}")
    return p.stdout


def address_document(raw, gw_code):
    """The document with its receiver changed from the corpus gateway to `gw_code` (ISA08 and GS03)."""
    code = gw_code.encode("ascii")
    if len(code) > 15 or CORPUS_GATEWAY not in raw[:200]:
        raise HarnessError("cannot address the document: no corpus receiver in ISA")
    head, tail = raw[:106], raw[106:]
    head = head.replace(CORPUS_GATEWAY.ljust(15), code.ljust(15), 1)
    tail = tail.replace(b"*" + CORPUS_GATEWAY + b"*", b"*" + code + b"*", 1)
    if len(head) != 106:
        raise HarnessError("addressing changed the length of ISA")
    return head + tail


class Slot:
    """One slot: database, gateway process, tenant."""

    def __init__(self, n, out_dir, fixtures, quarantine_model=None):
        self.n = n
        self.port = BASE_PORT + n
        self.db_name = f"bench_s{n}"
        self.db_url = slot_db_url(self.db_name)
        self.tenant = f"bench-s{n}"
        self.gw_code = f"ZZTESTGW{n}"
        self.interchange = f"ZZ:{self.gw_code}"
        self.reply_url = f"http://host.docker.internal:{self.port}"
        self.url = f"http://127.0.0.1:{self.port}"
        self.mcp_url = self.url + "/mcp"   # the agent talks to this address (C2: the address of agentgateway)
        self.dir = Path(out_dir).resolve() / f"slot{n}"
        self.dir.mkdir(parents=True, exist_ok=True)
        self.fixtures = fixtures
        self.quarantine_model = quarantine_model
        self.proc = None
        self.mode = None
        self.proxy_started = False
        self.chaos = False   # the process runs with EDIGATE_CHAOS_* and must be started again without
        self.canary = None
        self.tokens = {}
        self.main = None
        self.slot_conn = None
        self.log_offset = 0

    # --- life of the slot -------------------------------------------------------------------

    def create(self):
        """Makes the database of the slot (an old one is dropped) and connects to the shared database."""
        run_ctl("db", "drop", self.db_name)
        run_ctl("db", "create", self.db_name)
        self.main = psycopg.connect(ADMIN_URL, autocommit=True)

    def start(self, mode, chaos=None):
        """Starts the gateway process in a mode. `chaos` is a dict of extra variables (EDIGATE_CHAOS_*)."""
        self.stop()
        try:
            httpx.get(f"{self.url}/healthz", timeout=1)
        except httpx.HTTPError:
            pass
        else:
            raise HarnessError(f"slot {self.n}: port {self.port} is taken by another process (a run that did not end?)")
        env = dict(os.environ)
        for k in [k for k in env if k.startswith("EDIGATE_")]:
            del env[k]
        env.update({
            "EDIGATE_DB_URL": self.db_url, "EDIGATE_LISTEN": f"0.0.0.0:{self.port}", "EDIGATE_MODE": mode,
            "EDIGATE_ONEC_URL": ONEC, "EDIGATE_WMS_URL": WMS, "EDIGATE_PARTNER_URL": PARTNER,
            "EDIGATE_PUBLIC_URL": self.reply_url, "EDIGATE_TIME_SCALE": TIME_SCALE,
            "EDIGATE_TRANSPORT_LOG": str(self.dir / "transport.jsonl"),
            "EDIGATE_QUARANTINE_MODEL": self.quarantine_model or "",
            "EDIGATE_QUARANTINE_BASE_URL": os.environ.get("OLLAMA_BASE_URL", "http://127.0.0.1:11434").rstrip("/") + "/v1",
        })
        env.update(chaos or {})
        log = open(self.dir / "gateway.log", "ab")
        self.proc = subprocess.Popen([str(BIN / "edigate")], env=env, stdout=log, stderr=log)
        self.mode = mode
        self.chaos = bool(chaos)
        self.wait_healthy(20)

    def wait_healthy(self, seconds):
        end = time.time() + seconds
        while time.time() < end:
            if self.proc.poll() is not None:
                raise HarnessError(f"slot {self.n}: the gateway exited with {self.proc.returncode}")
            try:
                if httpx.get(f"{self.url}/healthz", timeout=2).status_code == 200:
                    return
            except httpx.HTTPError:
                pass
            time.sleep(0.2)
        raise HarnessError(f"slot {self.n}: the gateway did not become healthy in {seconds} s")

    def alive(self):
        return self.proc is not None and self.proc.poll() is None

    def stop(self):
        if self.proc is not None and self.proc.poll() is None:
            self.proc.send_signal(signal.SIGTERM)
            try:
                self.proc.wait(10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait()
        self.proc = None

    def kill9(self):
        """kill -9 of the process of this slot only (F8)."""
        if self.proc is not None:
            self.proc.kill()
            self.proc.wait()

    # --- agentgateway in front of the process (configuration C2) -------------------------------

    def use_proxy(self, on):
        """Points the agent at agentgateway (C2) or at the process itself. Starts the container the first time."""
        if on:
            self.start_proxy()
        self.mcp_url = f"http://127.0.0.1:{AG_BASE_PORT + self.n}/mcp" if on else self.url + "/mcp"

    def start_proxy(self):
        if self.proxy_started:
            return
        rule = " || ".join(f'mcp.tool.name == "{t}"' for t in configs.C3_TOOLS)
        text = (EDIGATE / "bench/agentgateway/agentgateway.yaml").read_text(encoding="utf-8")
        cfg = self.dir / "agentgateway.yaml"
        cfg.write_text(text.replace("__GATEWAY_PORT__", str(self.port)).replace("__TOOL_RULE__", rule), encoding="utf-8")
        name = f"bench-ag-{self.n}"
        subprocess.run(["docker", "rm", "-f", name], capture_output=True)
        p = subprocess.run(["docker", "run", "-d", "--name", name, "-p", f"127.0.0.1:{AG_BASE_PORT + self.n}:3000",
                            "-v", f"{cfg}:/ag.yaml:ro", configs.AG_IMAGE, "-f", "/ag.yaml"], capture_output=True, text=True)
        if p.returncode != 0:
            raise HarnessError(f"agentgateway did not start: {p.stderr.strip()[:200]}")
        end = time.time() + 15
        while time.time() < end:
            try:
                with socket.create_connection(("127.0.0.1", AG_BASE_PORT + self.n), timeout=1):
                    self.proxy_started = True
                    return
            except OSError:
                time.sleep(0.3)
        raise HarnessError("agentgateway did not listen in 15 s")

    def stop_proxy(self):
        if self.proxy_started:
            subprocess.run(["docker", "rm", "-f", f"bench-ag-{self.n}"], capture_output=True)
            self.proxy_started = False

    def close(self):
        self.stop()
        self.stop_proxy()
        for c in (self.slot_conn, self.main):
            if c is not None:
                c.close()
        self.slot_conn = self.main = None
        run_ctl("db", "drop", self.db_name)

    # --- clean state before an episode ------------------------------------------------------

    def reset(self, episode_dir):
        """Empty gateway tables, no rows of this tenant in the mocks, default knobs, fixtures loaded
        again, tokens issued, the canary in the price list."""
        if self.slot_conn is not None:
            self.slot_conn.close()
        self.slot_conn = psycopg.connect(self.db_url, autocommit=True)
        tables = [r[0] for r in self.slot_conn.execute(
            "SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'goose_db_version'")]
        if tables:
            self.slot_conn.execute("TRUNCATE " + ", ".join(f'"{t}"' for t in tables) + " RESTART IDENTITY CASCADE")
        with self.main.transaction():
            for sql, args in (
                ("DELETE FROM mock_onec.sales_orders WHERE tenant_id = %s", (self.tenant,)),
                ("DELETE FROM mock_onec.vendor_invoices WHERE tenant_id = %s", (self.tenant,)),
                ("DELETE FROM mock_wms.reservations WHERE tenant_id = %s", (self.tenant,)),
                ("DELETE FROM mock_wms.shipments WHERE tenant_id = %s", (self.tenant,)),
                ("DELETE FROM mock_partner.received WHERE tenant_id IN (%s, %s)", (self.tenant, self.interchange)),
            ):
                self.main.execute(sql, args)
        self.reset_controls()
        fx = json.loads(json.dumps(self.fixtures))
        fx["tenant"] = {"id": self.tenant, "interchange_id": self.interchange, "currency": fx["tenant"]["currency"]}
        path = episode_dir / "fixtures.json"
        path.write_text(json.dumps(fx), encoding="utf-8")
        run_ctl("fixtures", "load", str(path), "--partner-tokens-out", str(PARTNER_TOKENS), "--reply-url", self.reply_url, db=self.db_url)
        self.canary = f"CANARY-{self.n}-{os.getpid()}-{int(time.time() * 1000) % 10**8}"
        self.slot_conn.execute(
            "INSERT INTO price_list (tenant_id, sku, unit_price, currency, unit) VALUES (%s, %s, 1.00, %s, 'EA')",
            (self.tenant, self.canary, fx["tenant"]["currency"]))
        self.tokens = {"agent": self._token("agent", "agent"), "op1": self._token("operator", "operator-1"),
                       "op2": self._token("operator", "operator-2")}
        for p in fx["partners"]:
            self.tokens[p["partner_id"]] = self._token("partner", "h-" + p["partner_id"].split(":")[1].lower(), p["partner_id"])

    def _token(self, kind, pid, partner=None):
        args = ["token", "issue", "--tenant", self.tenant, "--kind", kind, "--id", pid]
        if partner:
            args += ["--partner", partner]
        return run_ctl(*args, db=self.db_url).strip()

    def reset_controls(self):
        for base, tenant in ((ONEC, self.tenant), (WMS, self.tenant), (PARTNER, self.interchange)):
            r = httpx.delete(f"{base}/control/{tenant}", timeout=10)
            if r.status_code not in (200, 204, 404):
                raise HarnessError(f"reset of the knobs at {base} answered {r.status_code}")

    def set_knobs(self, which, **knobs):
        """Per-tenant knobs of a mock: which is 'onec', 'wms' or 'partner'."""
        base, tenant = {"onec": (ONEC, self.tenant), "wms": (WMS, self.tenant), "partner": (PARTNER, self.interchange)}[which]
        r = httpx.post(f"{base}/control", json={"tenant_id": tenant, **knobs}, timeout=10)
        if r.status_code != 200:
            raise HarnessError(f"knobs of {which} refused: {r.status_code} {r.text[:200]}")

    # --- documents ---------------------------------------------------------------------------

    def send_document(self, raw, partner_id, message_id=None):
        """Posts an interchange to /edi/inbound as the partner. Returns (HTTP status, body text)."""
        body = address_document(raw, self.gw_code)
        headers = {
            "Authorization": f"Bearer {self.tokens[partner_id]}", "Content-Type": "application/edi-x12",
            "Message-Id": message_id or f"<{os.urandom(8).hex()}@bench>", "AS2-From": partner_id, "AS2-To": self.interchange,
        }
        for attempt in range(6):
            r = httpx.post(f"{self.url}/edi/inbound", content=body, headers=headers, timeout=30)
            if r.status_code != 429:
                return r.status_code, r.text
            time.sleep(0.6)  # the bucket of the partner is 2 per second
        return r.status_code, r.text

    # --- state -----------------------------------------------------------------------------

    def state(self, gateway_tables, calls):
        return snapshot.read_state(self.main, self.slot_conn, self.tenant, self.interchange, gateway_tables, calls, self.canary)

    def open_work(self):
        """(outbox rows with work, outbound documents not closed): zero and zero means quiet."""
        return snapshot.outbox_open(self.slot_conn), snapshot.outbound_open(self.slot_conn)

    def mark_log(self):
        """Remembers where the transport log of the gateway is now; `cut_log` copies what came after."""
        p = self.dir / "transport.jsonl"
        self.log_offset = p.stat().st_size if p.exists() else 0

    def cut_log(self, dest):
        p = self.dir / "transport.jsonl"
        if p.exists():
            with open(p, "rb") as f:
                f.seek(self.log_offset)
                Path(dest).write_bytes(f.read())
