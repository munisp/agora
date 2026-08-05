"""NL -> Cypher GraphRAG via Ollama (SPEC-W28 §4 WS-B: POST /v1/graph/ask).

Design (compliance gate 5 aligned): the LLM NEVER produces executable
Cypher. It answers a schema-prompted question by selecting one of the
allowlisted READ-ONLY query shapes (templates.py) plus params, as strict
JSON. The service then renders the canonical parameterized Cypher itself —
which is how the tenant filter is "injected post-generation" and why no
prompt injection can reach the graph store. The generated Cypher is returned
with the rows (capped at ASK_ROW_CAP, default 100).

Degradation: when Ollama is unreachable the endpoint answers 503 with a
reason; when the model output cannot be mapped to an allowlisted shape it
answers 422 (never a silent fallback to raw Cypher).
"""

from __future__ import annotations

import json
import re
from typing import Any, Protocol

from .templates import ASK_ALLOWED, TEMPLATES


class AskUnavailable(Exception):
    """Ollama unreachable / timed out (mapped to 503 with reason)."""


class AskUnanswerable(Exception):
    """Model output could not be mapped onto the read-only allowlist (422)."""


class AskLLM(Protocol):
    async def complete(self, messages: list[dict[str, str]]) -> str: ...


class OllamaAskLLM:
    """OpenAI-compatible chat client (mirrors voice-agent-runtime's
    OpenAICompatibleLLM seam): Ollama by default, any compatible endpoint
    via OLLAMA_BASE_URL."""

    def __init__(self, base_url: str, model: str, api_key: str, timeout_s: float) -> None:
        from openai import AsyncOpenAI  # lazy: not needed for tests

        self._client = AsyncOpenAI(base_url=base_url, api_key=api_key, timeout=timeout_s)
        self.model = model

    async def complete(self, messages: list[dict[str, str]]) -> str:
        try:
            resp = await self._client.chat.completions.create(
                model=self.model,
                messages=messages,  # type: ignore[arg-type]
                temperature=0.0,
            )
        except Exception as exc:  # noqa: BLE001 — connection/timeout/5xx
            raise AskUnavailable(f"ollama unavailable: {type(exc).__name__}: {exc}") from exc
        return (resp.choices[0].message.content or "").strip()


def build_prompt(question: str) -> list[dict[str, str]]:
    catalog = [
        {"template": name, "description": TEMPLATES[name].description, "params": TEMPLATES[name].params_doc}
        for name in sorted(ASK_ALLOWED)
    ]
    schema = (
        "Graph schema (per-tenant; every node carries tenant_id):\n"
        "Nodes: Person{person_id,name,phone_hash,channels,consent_summary,quarantine}, "
        "Contact{lead_id,channel_of_first_touch,source,captured_at}, "
        "Consent{consent_id,purpose,granted_at,revoked_at}, "
        "Booking{booking_id,status,created_at,showed}, Offering{offering_id,name}, "
        "Location{lga,ward}, Campaign{campaign_id,kind}.\n"
        "Edges: (Person)-[:CONSENTED]->(Consent), (Person)-[:HAS_CONTACT]->(Contact), "
        "(Contact)-[:CAPTURED_AT]->(Location), (Person)-[:BOOKED]->(Booking), "
        "(Booking)-[:FOR]->(Offering), (Person)-[:REFERRED]->(Person), "
        "(Person)-[:MESSAGED]->(Campaign)."
    )
    system = (
        "You translate analytics questions about a tenant knowledge graph into one of "
        "a fixed allowlist of READ-ONLY query templates. You MUST answer with a single "
        "JSON object and nothing else:\n"
        '{"template": "<one of the allowlisted template names>", "params": {...}}\n'
        "Never invent templates, never write Cypher, never add prose. If the question "
        "cannot be answered with the allowlist, answer "
        '{"template": "unanswerable", "params": {}}.\n\n'
        f"{schema}\n\nAllowlisted templates:\n{json.dumps(catalog, indent=2)}"
    )
    return [
        {"role": "system", "content": system},
        {"role": "user", "content": question},
    ]


_JSON_BLOCK_RE = re.compile(r"\{.*\}", re.DOTALL)


def parse_answer(text: str) -> dict[str, Any]:
    """Extract the JSON object from the model's reply (tolerates code fences
    and leading/trailing prose). Raises AskUnanswerable."""
    cleaned = text.strip()
    if cleaned.startswith("```"):
        cleaned = re.sub(r"^```(?:json)?\s*", "", cleaned)
        cleaned = re.sub(r"\s*```$", "", cleaned)
    match = _JSON_BLOCK_RE.search(cleaned)
    if not match:
        raise AskUnanswerable("model did not return a JSON template selection")
    try:
        payload = json.loads(match.group(0))
    except json.JSONDecodeError as exc:
        raise AskUnanswerable(f"model returned invalid JSON: {exc}") from exc
    if not isinstance(payload, dict) or "template" not in payload:
        raise AskUnanswerable("model JSON lacks a 'template' field")
    return payload


def validate_selection(payload: dict[str, Any]) -> tuple[str, dict[str, Any]]:
    """Allowlist enforcement: only ASK_ALLOWED read-only shapes may proceed."""
    name = payload.get("template")
    if not isinstance(name, str) or name not in ASK_ALLOWED:
        raise AskUnanswerable(
            f"question not answerable with the read-only template allowlist "
            f"(got {name!r}); allowed: {sorted(ASK_ALLOWED)}"
        )
    params = payload.get("params") or {}
    if not isinstance(params, dict):
        raise AskUnanswerable("'params' must be an object")
    return name, params
