"""Models for the agent: Ollama and OpenRouter through the `openai` client, and the fake models.

Input: a model id from the command line: `fake-honest`, `fake-gullible`, `ollama:<name>` or `openrouter:<name>`.
Output: an object with `async complete(messages, tools)` that returns a `Completion` (fakes.py): the assistant
message in chat format, the tokens from `usage`, and `cost_usd` (from the answer for OpenRouter, 0 for local models).

Decoding is fixed by the protocol (section 4): temperature 0.7, the seed passed to the API, an answer of at most
2048 tokens, reasoning switched off where the API allows it. OpenRouter needs OPENROUTER_API_KEY; without it
`check_ready` raises an error that names the key (that is not a failure of a smoke run). Ollama is
read from OLLAMA_BASE_URL (default http://127.0.0.1:11434) at /v1.
"""
import os

from openai import AsyncOpenAI

from .fakes import Completion, FakeModel

TEMPERATURE = 0.7
MAX_TOKENS = 2048
REQUEST_TIMEOUT = 280.0


def is_fake(model_id):
    return model_id.startswith("fake-")


def check_ready(model_id):
    """Raises RuntimeError with the reason if the model cannot run here."""
    if is_fake(model_id):
        if model_id not in ("fake-honest", "fake-gullible"):
            raise RuntimeError(f"unknown fake model {model_id!r}")
    elif model_id.startswith("openrouter:"):
        if not os.environ.get("OPENROUTER_API_KEY"):
            raise RuntimeError("OPENROUTER_API_KEY is not set: models of OpenRouter are not available")
    elif not model_id.startswith("ollama:"):
        raise RuntimeError(f"unknown model {model_id!r} (fake-honest, fake-gullible, ollama:<name>, openrouter:<name>)")


def make_fake(model_id, label, fixtures, document_id, tools, poll_sleep=1.0):
    return FakeModel(model_id.removeprefix("fake-"), label, fixtures, document_id, tools, poll_sleep)


class ApiModel:
    """A model behind an OpenAI-compatible chat API."""

    def __init__(self, model_id, seed):
        self.seed = seed
        if model_id.startswith("openrouter:"):
            self.name = model_id.removeprefix("openrouter:")
            self.client = AsyncOpenAI(base_url="https://openrouter.ai/api/v1", api_key=os.environ["OPENROUTER_API_KEY"],
                                      timeout=REQUEST_TIMEOUT)
            # cost comes back in usage; reasoning off where the model allows it
            self.extra = {"usage": {"include": True}, "reasoning": {"enabled": False}}
        else:
            self.name = model_id.removeprefix("ollama:")
            base = os.environ.get("OLLAMA_BASE_URL", "http://127.0.0.1:11434").rstrip("/") + "/v1"
            self.client = AsyncOpenAI(base_url=base, api_key="ollama", timeout=REQUEST_TIMEOUT)
            self.extra = {"think": False, "reasoning_effort": "none"}

    async def aclose(self):
        """Closes the HTTP client; the agent loop calls it when the episode is over."""
        await self.client.close()

    async def complete(self, messages, tools):
        resp = await self.client.chat.completions.create(
            model=self.name, messages=messages, tools=tools or None, temperature=TEMPERATURE, seed=self.seed,
            max_tokens=MAX_TOKENS, extra_body=self.extra)
        msg = resp.choices[0].message
        out = {"role": "assistant", "content": msg.content}
        if msg.tool_calls:
            out["tool_calls"] = [{"id": c.id, "type": "function",
                                  "function": {"name": c.function.name, "arguments": c.function.arguments}} for c in msg.tool_calls]
        usage = resp.usage
        extra = (usage.model_extra or {}) if usage else {}
        return Completion(out, usage.prompt_tokens if usage else 0, usage.completion_tokens if usage else 0,
                          float(extra.get("cost") or 0.0))
