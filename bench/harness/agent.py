"""The agent loop (protocol section 4): a model, the tools of the gateway, a limit of 20 tool calls and 300 s.

Input: the URL and token of the agent, a model factory, the system prompt, the task text, the document set.
Output: an `AgentResult` with the end reason (`finished`, `step_limit`, `timeout`, `model_error`), the number of
steps, the tokens and cost, the calls the agent made, and how many answers were refusals of the gateway.

The tools come from `tools/list` of the running mode, cut by the task (configs.offered_tools), and are given to the
model as functions. A call of the model becomes `tools/call` on the gateway. Every message of the model and every call
of MCP is written as a line of JSON to the harness log of the episode (`harness_transport.jsonl`).
One MCP session serves the episode; `McpClient.reconnect` opens a new one after the gateway process was restarted.
"""
import asyncio
import json
import re
import time
from dataclasses import dataclass, field

import httpx
from mcp import ClientSession, MCPError
from mcp.client.streamable_http import CONNECTION_CLOSED, create_mcp_http_client, streamable_http_client

from . import configs

STEP_LIMIT = 20
EPISODE_TIMEOUT = 300.0
# Reasons that mean the gateway refused the call (used for FPR and for the 'blocked' count).
REFUSALS = {"policy_denied", "tainted_sink", "rate_limited", "forbidden", "conflicting_duplicate", "quarantine_invalid"}


class JsonlLog:
    """Appends one JSON object per line. Safe for one writer per episode."""

    def __init__(self, path):
        self.f = open(path, "a", encoding="utf-8")

    def write(self, **kw):
        self.f.write(json.dumps({"t": round(time.time(), 3), **kw}, ensure_ascii=False, default=str) + "\n")
        self.f.flush()

    def close(self):
        self.f.close()


@dataclass
class AgentResult:
    end_reason: str = "finished"
    steps: int = 0
    prompt_tokens: int = 0
    completion_tokens: int = 0
    cost_usd: float = 0.0
    calls: list = field(default_factory=list)   # [{tool, args}] as the agent sent them
    refusals: int = 0
    error: str = ""


class McpClient:
    """One MCP session to the gateway, with the token of the agent. Used as an async context manager."""

    def __init__(self, url, token):
        self.url, self.token = url, token
        self.stack = None
        self.session = None

    async def __aenter__(self):
        await self.reconnect()
        return self

    async def __aexit__(self, *exc):
        await self.close()

    async def reconnect(self):
        await self.close()
        from contextlib import AsyncExitStack
        stack = AsyncExitStack()
        http = create_mcp_http_client(headers={"Authorization": f"Bearer {self.token}"})
        await stack.enter_async_context(http)
        read, write, *_ = await stack.enter_async_context(streamable_http_client(self.url, http_client=http))
        session = await stack.enter_async_context(ClientSession(read, write))
        await session.initialize()
        self.stack, self.session = stack, session

    async def close(self):
        if self.stack is not None:
            try:
                await self.stack.aclose()
            except (Exception, asyncio.CancelledError):   # a broken session may fail on close; nothing to keep
                pass
        self.stack = self.session = None

    async def list_tools(self):
        res = await self.session.list_tools()
        return [{"name": t.name, "description": t.description or "", "input_schema": t.input_schema} for t in res.tools]

    async def call(self, name, args):
        """Calls a tool. Returns (text, error): text is the JSON text of the answer; error is {code, reason, message} or None."""
        try:
            res = await self.session.call_tool(name, args)
        except asyncio.CancelledError:
            # The session cancels its own call when the connection breaks (the gateway process was killed).
            # A cancellation of our own task must go on; the other one is a lost connection.
            if asyncio.current_task().cancelling():
                raise
            raise ConnectionError("the MCP session was closed") from None
        except MCPError as e:
            if e.code == CONNECTION_CLOSED:   # the process is gone: the caller decides (the scripted agent reconnects)
                raise
            reason = ""
            if isinstance(e.data, dict):
                reason = e.data.get("reason", "")
            return json.dumps({"error": {"code": e.code, "reason": reason, "message": e.message,
                                         "explain": (e.data or {}).get("explain") if isinstance(e.data, dict) else None}}), \
                {"code": e.code, "reason": reason, "message": e.message}
        text = "".join(c.text for c in res.content if getattr(c, "text", None) is not None)
        if res.is_error:
            return json.dumps({"error": {"message": text}}), {"code": 0, "reason": "tool_error", "message": text}
        return text, None


class RawClient:
    """The same four methods as McpClient over plain JSON-RPC POSTs (the gateway is stateless: no session).

    The scripted agent of the fault episodes uses it: it kills and restarts the gateway process, and the session
    object of the `mcp` package cancels its own task when the connection breaks, which cannot be recovered from
    cleanly. A closed connection here is an ordinary httpx exception that the caller retries."""

    def __init__(self, url, token):
        self.url = url
        self.headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json",
                        "Accept": "application/json, text/event-stream"}
        self.http = None
        self.n = 0

    async def __aenter__(self):
        await self.reconnect()
        return self

    async def __aexit__(self, *exc):
        await self.close()

    async def reconnect(self):
        """A new HTTP client and the MCP handshake. A proxy such as agentgateway keeps a session (Mcp-Session-Id);
        the gateway itself keeps none, and the header is then absent."""
        await self.close()
        self.http = httpx.AsyncClient(headers=self.headers, timeout=60)
        resp = await self._post({"jsonrpc": "2.0", "id": 0, "method": "initialize",
                                 "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "bench", "version": "1"}}})
        if resp.headers.get("mcp-session-id"):
            self.http.headers["Mcp-Session-Id"] = resp.headers["mcp-session-id"]
        await self._post({"jsonrpc": "2.0", "method": "notifications/initialized"})

    async def close(self):
        if self.http is not None:
            await self.http.aclose()
        self.http = None

    async def _post(self, message, spaced=False):
        body = json.dumps(message, indent=2 if spaced else None)
        return await self.http.post(self.url, content=body.encode("utf-8"))

    async def _rpc(self, method, params, spaced=False):
        self.n += 1
        resp = await self._post({"jsonrpc": "2.0", "id": self.n, "method": method, "params": params}, spaced)
        text = resp.text
        if "text/event-stream" in resp.headers.get("content-type", ""):
            data = [l[5:].strip() for l in text.splitlines() if l.startswith("data:")]
            text = data[-1] if data else ""
        try:
            return json.loads(text)
        except ValueError:
            raise ConnectionError(f"HTTP {resp.status_code} with a body that is not JSON-RPC")

    async def list_tools(self):
        msg = await self._rpc("tools/list", {})
        return [{"name": t["name"], "description": t.get("description", ""), "input_schema": t.get("inputSchema", {})}
                for t in msg["result"]["tools"]]

    async def call(self, name, args, spaced=False):
        """Same contract as McpClient.call: (text, error). `spaced` sends the JSON with white space (fault F5)."""
        msg = await self._rpc("tools/call", {"name": name, "arguments": args}, spaced)
        if "error" in msg:
            e = msg["error"]
            data = e.get("data") if isinstance(e.get("data"), dict) else {}
            reason = data.get("reason", "")
            return json.dumps({"error": {"code": e.get("code"), "reason": reason, "message": e.get("message"),
                                         "explain": data.get("explain")}}), \
                {"code": e.get("code"), "reason": reason, "message": e.get("message")}
        res = msg["result"]
        text = "".join(c.get("text", "") for c in res.get("content", []))
        if res.get("isError"):
            return json.dumps({"error": {"message": text}}), {"code": 0, "reason": "tool_error", "message": text}
        return text, None


def safe_name(name):
    """Chat APIs do not take dots in function names: edi.read_document becomes edi_read_document."""
    return re.sub(r"[^A-Za-z0-9_-]", "_", name)


def to_openai_tools(tools):
    return [{"type": "function", "function": {"name": safe_name(t["name"]), "description": t["description"],
                                              "parameters": t["input_schema"]}} for t in tools]


async def run_agent(url, token, make_model, system, task, set_id, log, step_limit=STEP_LIMIT, timeout=EPISODE_TIMEOUT):
    """Runs one episode of the agent. `make_model(tools)` builds the model once the tool list is known.
    Never raises for a failure of the model or the gateway: it returns the result with the end reason."""
    result = AgentResult()
    try:
        await asyncio.wait_for(_loop(url, token, make_model, system, task, set_id, log, step_limit, result), timeout)
    except asyncio.TimeoutError:
        result.end_reason = "timeout"
    except Exception as e:  # the model or the transport broke: the state is still read by the runner
        result.end_reason = "model_error"
        result.error = f"{type(e).__name__}: {e}"[:300]
    log.write(kind="end", end_reason=result.end_reason, steps=result.steps, error=result.error)
    return result


async def _loop(url, token, make_model, system, task, set_id, log, step_limit, result):
    async with McpClient(url, token) as client:
        listed = await client.list_tools()
        names = configs.offered_tools(set_id, {t["name"] for t in listed})
        tools = [t for t in listed if t["name"] in names]
        tools.sort(key=lambda t: names.index(t["name"]))
        model = make_model(tools)
        try:
            await _converse(client, model, tools, names, system, task, log, step_limit, result)
        finally:
            if hasattr(model, "aclose"):
                await model.aclose()


async def _converse(client, model, tools, names, system, task, log, step_limit, result):
    """The conversation: the model answers, its tool calls go to the gateway, until it stops or a limit is hit."""
    by_safe = {safe_name(t["name"]): t["name"] for t in tools}
    openai_tools = to_openai_tools(tools)
    messages = [{"role": "system", "content": system}, {"role": "user", "content": task}]
    log.write(kind="start", tools=names)
    while True:
        reply = await model.complete(messages, openai_tools)
        result.prompt_tokens += reply.prompt_tokens
        result.completion_tokens += reply.completion_tokens
        result.cost_usd += reply.cost_usd
        msg = reply.message
        messages.append(msg)
        log.write(kind="model", message=msg, prompt_tokens=reply.prompt_tokens, completion_tokens=reply.completion_tokens)
        calls = msg.get("tool_calls") or []
        if not calls:
            result.end_reason = "finished"
            return
        for call in calls:
            if result.steps >= step_limit:
                result.end_reason = "step_limit"
                return
            name = by_safe.get(call["function"]["name"], call["function"]["name"])
            try:
                args = json.loads(call["function"]["arguments"] or "{}")
            except ValueError:
                args = {}
            result.steps += 1
            result.calls.append({"tool": name, "args": args})
            log.write(kind="call", tool=name, args=args)
            text, error = await client.call(name, args)
            if error and error["reason"] in REFUSALS:
                result.refusals += 1
            log.write(kind="result", tool=name, error=error, text=text[:4000])
            messages.append({"role": "tool", "tool_call_id": call["id"], "content": text})
        if result.steps >= step_limit:
            result.end_reason = "step_limit"
            return
