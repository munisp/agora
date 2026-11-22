"""LLM-compressed channel answers (SPEC-W110).

USSD grounding (W108) answers with raw KB snippet text. When USSD_KB_LLM is
on, the raw snippets are composed into a short natural answer by an
OpenAI-compatible LLM (default qwen3:8b via Ollama — same pattern as
intel.py llm_extract). The composer NEVER raises: any failure returns None
and the caller keeps the raw grounded text (fallback chain: LLM answer ->
raw snippet -> static ack).
"""

from __future__ import annotations

from typing import Any

import httpx

from .config import Config
from .logging import get_logger
from .ussd import grounded_reply

log = get_logger(__name__)

COMPOSE_SYSTEM = (
    "You answer customer questions for a business over USSD (a plain-text "
    "phone menu channel). Use ONLY the provided facts. Keep the answer "
    "short and plain: no markdown, no lists, no pleasantries. If the facts "
    "do not answer the question, say so in one short sentence."
)


async def compose_answer(
    question: str,
    snippets: list[str],
    cfg: Config,
    *,
    client: httpx.AsyncClient | None = None,
) -> str | None:
    """Compose a USSD-sized answer from KB snippets; None on any failure."""
    facts = [s for s in (str(x).strip() for x in snippets) if s]
    if not question.strip() or not facts:
        return None
    payload = {
        "model": cfg.ussd_kb_llm_model,
        "messages": [
            {"role": "system", "content": COMPOSE_SYSTEM},
            {
                "role": "user",
                "content": f"Question: {question[:500]}\n\nFacts:\n"
                + "\n".join(f"- {f[:500]}" for f in facts[:4]),
            },
        ],
        "temperature": 0.0,
        "max_tokens": cfg.ussd_kb_llm_max_tokens,
    }
    headers = {}
    if cfg.ussd_kb_llm_api_key:
        headers["authorization"] = f"Bearer {cfg.ussd_kb_llm_api_key}"
    owns = client is None
    client = client or httpx.AsyncClient(
        timeout=httpx.Timeout(cfg.ussd_kb_llm_timeout_s)
    )
    try:
        resp = await client.post(
            f"{cfg.ussd_kb_llm_base_url.rstrip('/')}/chat/completions",
            json=payload,
            headers=headers,
        )
        if resp.status_code >= 300:
            log.warning("kb compose http error", status=resp.status_code)
            return None
        body: dict[str, Any] = resp.json()
        content = (
            (body.get("choices") or [{}])[0].get("message", {}).get("content") or ""
        ).strip()
        if not content:
            return None
        # Enforce the USSD char budget with the same word-boundary
        # truncation as raw grounded replies.
        return grounded_reply([content], max_snippets=1, max_chars=cfg.ussd_kb_max_reply_chars)
    except Exception as exc:  # noqa: BLE001 - degrade to raw snippets
        log.warning("kb compose failed", error=str(exc)[:200])
        return None
    finally:
        if owns:
            await client.aclose()
