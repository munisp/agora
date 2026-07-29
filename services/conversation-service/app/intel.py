"""Call intelligence (SPEC-W3 §4, innovation 3): per-turn enrichment.

- Lexicon sentiment: curated positive/negative word lists with negation
  handling (a negator within a 3-word window flips the polarity of the
  sentiment word). No external dependency — fast enough for every turn.
- Optional LLM NER (env INTEL_LLM=on): one OpenAI-compatible chat call
  (same LLM_BASE_URL/LLM_MODEL/LLM_API_KEY env family as the voice runtime,
  so it works with the local Ollama qwen3:8b out of the box) extracting
  {"intent": ..., "entities": {...}} as JSON with a strict 3s timeout.
  Any failure (timeout, bad JSON, unreachable) degrades to lexicon-only.
"""

from __future__ import annotations

import json
import re
from typing import Any

import httpx

from .config import Config
from .logging import get_logger

log = get_logger(__name__)

# Curated service-domain lexicons (English; extend per deployment).
POSITIVE_WORDS = frozenset(
    """
    amazing awesome best better brilliant calm comfortable delighted easy
    excellent fantastic friendly glad good great happy helpful impressed
    kind lovely nice perfect pleased polite quick recommend satisfied smooth
    thanks thank thrilled wonderful
    """.split()
)

NEGATIVE_WORDS = frozenset(
    """
    angry annoyed awful bad broken complaint confused disappointing
    frustrated horrible late long lost noisy overpriced pain problem rude
    sad slow sorry terrible unacceptable unhappy upset waiting worse
    worst wrong
    """.split()
)

NEGATIONS = frozenset(
    "no not never neither nor cannot can't couldn't don't doesn't didn't "
    "isn't aren't wasn't weren't won't wouldn't shouldn't hardly barely "
    "without none nothing nobody nowhere".split()
)

_WORD_RE = re.compile(r"[a-z']+")

_NEGATION_WINDOW = 3


def normalize_text(text: str) -> str:
    """Lowercased, punctuation-stripped, single-spaced token string.

    Shared phrase-matching normalization: app/incidents.py (SPEC-W11
    emergency classifier) matches multi-word EN/PCM lexicon phrases against
    this form. Sentiment tokenization above is unchanged.
    """
    return " ".join(_WORD_RE.findall(text.lower()))


def analyze_sentiment(text: str) -> dict[str, Any]:
    """Lexicon sentiment with negation handling.

    Returns {"score": float in [-1, 1], "label": positive|negative|neutral}.
    Score = (pos - neg) / (pos + neg), so a single polarised word saturates
    and balanced texts land near 0.
    """
    tokens = _WORD_RE.findall(text.lower())
    pos = neg = 0
    for i, tok in enumerate(tokens):
        polarity = 0
        if tok in POSITIVE_WORDS:
            polarity = 1
        elif tok in NEGATIVE_WORDS:
            polarity = -1
        if polarity == 0:
            continue
        window = tokens[max(0, i - _NEGATION_WINDOW):i]
        if any(w in NEGATIONS for w in window):
            polarity = -polarity
        if polarity > 0:
            pos += 1
        else:
            neg += 1
    total = pos + neg
    score = (pos - neg) / total if total else 0.0
    if score > 0.15:
        label = "positive"
    elif score < -0.15:
        label = "negative"
    else:
        label = "neutral"
    return {"score": round(score, 4), "label": label}


_NER_SYSTEM = (
    "You extract structured call metadata. Answer with ONLY a JSON object, "
    "no prose, of the form "
    '{"intent": "<short snake_case caller intent>", "entities": {"<key>": "<value>"}}. '
    "Entities capture concrete details mentioned by the caller (names, dates, "
    "services, reference numbers). Use an empty object when none."
)


def _extract_json(text: str) -> dict[str, Any] | None:
    """Parse the first JSON object in an LLM response (tolerant of fences)."""
    text = text.strip()
    start = text.find("{")
    end = text.rfind("}")
    if start == -1 or end <= start:
        return None
    try:
        parsed = json.loads(text[start:end + 1])
    except json.JSONDecodeError:
        return None
    if not isinstance(parsed, dict):
        return None
    intent = parsed.get("intent")
    entities = parsed.get("entities")
    return {
        "intent": str(intent) if isinstance(intent, (str, int, float)) else None,
        "entities": entities if isinstance(entities, dict) else {},
    }


async def llm_extract(
    text: str,
    cfg: Config,
    *,
    client: httpx.AsyncClient | None = None,
) -> dict[str, Any] | None:
    """Optional LLM NER: {"intent": str|None, "entities": dict} or None on failure."""
    payload = {
        "model": cfg.intel_llm_model,
        "messages": [
            {"role": "system", "content": _NER_SYSTEM},
            {"role": "user", "content": text[:2000]},
        ],
        "temperature": 0.0,
    }
    headers = {}
    if cfg.intel_llm_api_key:
        headers["authorization"] = f"Bearer {cfg.intel_llm_api_key}"
    owns = client is None
    client = client or httpx.AsyncClient(
        timeout=httpx.Timeout(cfg.intel_llm_timeout_s)
    )
    try:
        resp = await client.post(
            f"{cfg.intel_llm_base_url.rstrip('/')}/chat/completions",
            json=payload,
            headers=headers,
        )
        if resp.status_code >= 300:
            log.warning("intel llm http error", status=resp.status_code)
            return None
        body = resp.json()
        content = (
            (body.get("choices") or [{}])[0].get("message", {}).get("content") or ""
        )
        return _extract_json(content)
    except Exception as exc:  # noqa: BLE001 - degrade to lexicon-only
        log.warning("intel llm extraction failed", error=str(exc))
        return None
    finally:
        if owns:
            await client.aclose()


async def enrich_turn(
    text: str,
    cfg: Config,
    *,
    client: httpx.AsyncClient | None = None,
) -> dict[str, Any]:
    """Full per-turn enrichment payload.

    Always includes lexicon sentiment; intent/entities are populated only
    when INTEL_LLM=on and the LLM call succeeds (else None — lexicon-only).
    """
    sentiment = analyze_sentiment(text)
    result: dict[str, Any] = {
        "sentiment": sentiment["score"],
        "sentiment_label": sentiment["label"],
        "intent": None,
        "entities": None,
    }
    if cfg.intel_llm:
        ner = await llm_extract(text, cfg, client=client)
        if ner is not None:
            result["intent"] = ner.get("intent")
            result["entities"] = ner.get("entities") or {}
    return result
