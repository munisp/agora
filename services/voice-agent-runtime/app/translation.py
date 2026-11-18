"""SPEC-W82 A1-A3: Nigerian-language translation layer.

Greenfield module (no translation infrastructure existed pre-W82). Three
cooperating pieces:

1. :func:`detect_language` — PURE STDLIB heuristic detector over
   {en, pcm, yo, ig, ha}. Signals: Yoruba/Igbo diacritics (e-dot, o-dot,
   i-dot, u-dot, n-tilde — matched in precomposed AND NFD combining forms),
   Hausa hooked letters (b-hook, d-hook, k-hook, y-hook), a Nigerian-Pidgin
   lexicon (wetin, dey, wahala, abeg, una, how far, no dey, na so, ...) plus
   small function-word lexicons for Yoruba/Igbo/Hausa so short texts with no
   special characters still resolve. English is the fallback when no signal
   fires. Deterministic, < 1 ms on 500 chars, no dependencies, no network.
2. Providers behind :class:`TranslatorInterface`:
   - :class:`HTTPTranslator` — POSTs {text, source_lang, target_lang} to
     TRANSLATE_URL with an optional Bearer key and a hard timeout
     (TRANSLATE_TIMEOUT_S, default 3.0s). FAIL-OPEN on ANY error (timeout,
     connection failure, 5xx, bad JSON, missing key): the original text is
     returned and ``voice_translation_errors_total{provider="http"}`` is
     incremented — a turn is NEVER blocked or failed by translation.
   - :class:`MockTranslator` — echoes the text, provider="mock", any lang
     pair. Default via TRANSLATE_MOCK=1 (the sanctioned XTTS_MOCK /
     COSYVOICE_MOCK / MMS_MOCK dev pattern).
3. Turn helpers :func:`process_inbound_text` / :func:`process_outbound_text`
   — the fail-open seams consumed by the chat path (app/chat.py
   ChatService._prepare_turn / reply path) and the voice path
   (app/livekit_worker.py WhisperSTTNode post-STT hook + PiperTTSNode
   pre-TTS transform).

Only {pcm, yo, ig, ha} are translated inbound -> en and outbound en -> lang
(:data:`TRANSLATABLE_LANGUAGES`); every other detection passes through
untouched. Code-switched pcm/en turns (app/codeswitch.py) are NOT
force-translated wholesale — they pass through with a code-switch locale
instruction instead.
"""

from __future__ import annotations

import re
import unicodedata
from dataclasses import dataclass, field
from typing import Any, Protocol

from . import metrics
from .logging import get_logger
from .multilang import normalize_language

log = get_logger("translation")

#: Languages the W82 translation layer handles (Nigerian Pidgin + the three
#: major Nigerian languages). Everything else passes through.
TRANSLATABLE_LANGUAGES = frozenset({"pcm", "yo", "ig", "ha"})


@dataclass
class TranslationResult:
    text: str
    source_lang: str
    target_lang: str
    provider: str
    cached: bool = False


class TranslatorInterface(Protocol):
    async def translate(
        self, text: str, source_lang: str, target_lang: str
    ) -> TranslationResult:
        """Translate ``text`` from ``source_lang`` to ``target_lang``."""
        ...


# ---------------------------------------------------------------------------
# A1: pure-stdlib heuristic language detector (en / pcm / yo / ig / ha)
# ---------------------------------------------------------------------------
# Nigerian Pidgin lexicon. Strong words are unambiguous Pidgin markers
# (weight 2); weak words are common-but-nameable (weight 1). English words
# that also exist in Pidgin (chop, dash, yarn, don) are deliberately absent —
# false positives on English text are worse than a missed detection.
PIDGIN_WORDS_STRONG = frozenset(
    {
        "wetin",
        "dey",
        "wahala",
        "abeg",
        "una",
        "pikin",
        "sabi",
        "oya",
        "shebi",
        "naija",
        "waka",
        "kuku",
        "yarn",
    }
)
PIDGIN_WORDS_WEAK = frozenset({"abi"})
PIDGIN_WORDS = PIDGIN_WORDS_STRONG | PIDGIN_WORDS_WEAK

#: Multi-word Pidgin markers (weight 3 when present as a substring phrase).
PIDGIN_PHRASES = (
    "how far",
    "no dey",
    "na so",
    "i dey",
    "e don",
    "dey go",
    "no wahala",
    "una dey",
)

# Small function-word lexicons so short texts without diacritics/hooked
# letters still resolve ("Bawo ni?", "Kedu?", "Sannu!"). Weight 2 each.
YORUBA_WORDS = frozenset(
    {
        "bawo",
        "kini",
        "jowo",
        "ekaaro",
        "ekasan",
        "ekale",
        "nlo",
        "wipe",
        "odabo",
        "dabo",
        "nko",
        "kosi",
        "rere",
        "sugbon",
        "nitori",
        "emi",
    }
)
IGBO_WORDS = frozenset(
    {
        "kedu",
        "ndewo",
        "biko",
        "dalu",
        "imela",
        "kwenu",
        "gini",
        "ole",
        "kwanu",
        "udo",
        "nnoo",
        "olee",
    }
)
HAUSA_WORDS = frozenset(
    {
        "sannu",
        "lafiya",
        "nagode",
        "yaya",
        "gobe",
        "yau",
        "kudin",
        "kudi",
        "barka",
        "gida",
        "aboki",
        "yarinya",
    }
)
HAUSA_PHRASES = ("don allah", "ina kwana", "yau da gobe", "sai gobe")

# Deterministic tie-break order when two languages score identically
# (documented: Pidgin first, then Yoruba, Igbo, Hausa — Nigerian Pidgin is
# the most common mixed register on Naija telephony audio/text).
_DETECTOR_PRIORITY = ("pcm", "yo", "ig", "ha")

_WORD_RE = re.compile(r"\w+")


def detect_language(text: str) -> str:
    """Heuristically detect en/pcm/yo/ig/ha for a caller utterance.

    Pure stdlib, deterministic, no network. Scoring:
    - Yoruba diacritics (e/s + dot below) weight 2; o-dot weight 1 for BOTH
      Yoruba and Igbo (shared letter); Igbo letters (i/u/n + dot below,
      n-tilde) weight 2; Hausa hooked letters (b/d/k/y-hook) weight 2.
      Precomposed and NFD combining forms are both matched.
    - Lexicon hits weight 2 (weak Pidgin markers weight 1), multi-word
      phrases weight 3.
    - Highest score wins; ties break by _DETECTOR_PRIORITY; zero score is
      English (the detector NEVER invents a language for unmarked text).
    """
    raw = (text or "").strip()
    if not raw:
        return "en"
    lower = raw.lower()
    scores = {"pcm": 0, "yo": 0, "ig": 0, "ha": 0}

    # Signal 1+2: diacritics and hooked letters (precomposed form).
    for ch in lower:
        if ch in "ẹṣ":
            scores["yo"] += 2
        elif ch == "ọ":
            scores["yo"] += 1
            scores["ig"] += 1
        elif ch in "ịụñ":
            scores["ig"] += 2
        elif ch in "ɓɗƙƴ":
            scores["ha"] += 2
    # Same signals in NFD (decomposed) form: base letter + U+0323 dot below.
    nfd = unicodedata.normalize("NFD", lower)
    prev = ""
    for ch in nfd:
        if ch == "\u0323":  # combining dot below (e.g. o + U+0323 = o-dot)
            if prev in "es":
                scores["yo"] += 2
            elif prev == "o":
                scores["yo"] += 1
                scores["ig"] += 1
            elif prev in "iun":
                scores["ig"] += 2
        prev = ch

    # Signal 3: lexicons.
    words = _WORD_RE.findall(lower)
    for word in words:
        if word in PIDGIN_WORDS_STRONG:
            scores["pcm"] += 2
        elif word in PIDGIN_WORDS_WEAK:
            scores["pcm"] += 1
        if word in YORUBA_WORDS:
            scores["yo"] += 2
        if word in IGBO_WORDS:
            scores["ig"] += 2
        if word in HAUSA_WORDS:
            scores["ha"] += 2
    for phrase in PIDGIN_PHRASES:
        if phrase in lower:
            scores["pcm"] += 3
    for phrase in HAUSA_PHRASES:
        if phrase in lower:
            scores["ha"] += 3

    best = "en"
    best_score = 0
    for lang in _DETECTOR_PRIORITY:
        if scores[lang] > best_score:
            best = lang
            best_score = scores[lang]
    return best


# ---------------------------------------------------------------------------
# A1: providers
# ---------------------------------------------------------------------------
class MockTranslator:
    """Dev default (TRANSLATE_MOCK=1): echoes the text for any lang pair."""

    provider = "mock"

    async def translate(
        self, text: str, source_lang: str, target_lang: str
    ) -> TranslationResult:
        return TranslationResult(
            text=text,
            source_lang=source_lang,
            target_lang=target_lang,
            provider=self.provider,
        )


class HTTPTranslator:
    """POST {text, source_lang, target_lang} to a translation HTTP endpoint.

    FAIL-OPEN on any error/timeout/5xx/bad JSON/missing key: returns the
    original text, increments ``voice_translation_errors_total`` and logs a
    single warning per call. The response body is JSON; the translated string
    is read from ``text`` (falling back to ``translated_text`` /
    ``translation`` keys).
    """

    provider = "http"

    def __init__(
        self,
        url: str,
        *,
        api_key: str = "",
        timeout_s: float = 3.0,
        client: Any = None,
    ) -> None:
        self._url = (url or "").strip()
        self._api_key = api_key or ""
        self._timeout_s = timeout_s
        self._client = client  # injected in tests; lazily created otherwise
        if not self._url:
            log.warning("HTTPTranslator configured without TRANSLATE_URL; fail-open")

    async def _get_client(self) -> Any:
        if self._client is None:
            import httpx

            self._client = httpx.AsyncClient(
                timeout=httpx.Timeout(self._timeout_s or 3.0)
            )
        return self._client

    async def translate(
        self, text: str, source_lang: str, target_lang: str
    ) -> TranslationResult:
        try:
            if not self._url:
                raise RuntimeError("TRANSLATE_URL not configured")
            headers = (
                {"Authorization": f"Bearer {self._api_key}"} if self._api_key else None
            )
            client = await self._get_client()
            resp = await client.post(
                self._url,
                json={
                    "text": text,
                    "source_lang": source_lang,
                    "target_lang": target_lang,
                },
                headers=headers,
            )
            if resp.status_code != 200:
                raise RuntimeError(f"translator HTTP {resp.status_code}")
            data = resp.json()
            translated = (
                data.get("text") or data.get("translated_text") or data.get("translation")
                if isinstance(data, dict)
                else None
            )
            if not isinstance(translated, str) or not translated.strip():
                raise RuntimeError("translator returned no text")
            return TranslationResult(
                text=translated,
                source_lang=source_lang,
                target_lang=target_lang,
                provider=self.provider,
            )
        except Exception as exc:  # noqa: BLE001 - fail-open by contract
            metrics.translation_error(self.provider)
            log.warning(
                "translation failed; passing through original text",
                provider=self.provider,
                source_lang=source_lang,
                target_lang=target_lang,
                error=str(exc)[:200],
            )
            return TranslationResult(
                text=text,
                source_lang=source_lang,
                target_lang=target_lang,
                provider=self.provider,
            )

    async def aclose(self) -> None:
        if self._client is not None:
            try:
                await self._client.aclose()
            except Exception:  # noqa: BLE001 - shutdown must not raise
                pass


def _truthy(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    return str(value).strip().lower() in ("1", "true", "yes", "on")


def build_translator(settings: Any) -> TranslatorInterface:
    """Provider selection, mirroring the existing chain idioms.

    TRANSLATE_MOCK=1 (default) forces the MockTranslator regardless of
    TRANSLATE_PROVIDER — the sanctioned mock-default dev pattern
    (XTTS_MOCK/COSYVOICE_MOCK/MMS_MOCK/WHATSAPP_MOCK all default "1").
    """
    if _truthy(getattr(settings, "translate_mock", True)):
        return MockTranslator()
    provider = (getattr(settings, "translate_provider", "mock") or "mock").strip().lower()
    if provider == "http":
        return HTTPTranslator(
            getattr(settings, "translate_url", ""),
            api_key=getattr(settings, "translate_api_key", ""),
            timeout_s=getattr(settings, "translate_timeout_s", 3.0),
        )
    if provider != "mock":
        log.warning(
            "unknown TRANSLATE_PROVIDER; falling back to mock translator",
            provider=provider,
        )
    return MockTranslator()


# ---------------------------------------------------------------------------
# A2/A3: fail-open turn helpers
# ---------------------------------------------------------------------------
def _provider_name(translator: Any) -> str:
    return str(getattr(translator, "provider", "unknown"))


async def _safe_translate(
    translator: TranslatorInterface,
    text: str,
    source_lang: str,
    target_lang: str,
    *,
    direction: str,
) -> TranslationResult:
    """One guarded translation call: metrics on every attempt, fail-open to
    the original text on ANY error (a turn never blocks on translation)."""
    provider = _provider_name(translator)
    metrics.translation(
        direction=direction,
        source_lang=source_lang,
        target_lang=target_lang,
        provider=provider,
    )
    try:
        return await translator.translate(text, source_lang, target_lang)
    except Exception as exc:  # noqa: BLE001 - belt-and-braces guard; the
        # built-in providers already fail open internally.
        metrics.translation_error(provider)
        log.warning(
            "translator raised; passing through original text",
            provider=provider,
            direction=direction,
            error=str(exc)[:200],
        )
        return TranslationResult(
            text=text,
            source_lang=source_lang,
            target_lang=target_lang,
            provider=provider,
        )


@dataclass
class InboundResult:
    """Outcome of the inbound (caller-language -> English) turn seam."""

    text: str  # effective text for routing/LLM (translated, or original)
    detected_language: str = ""
    original_text: str = ""
    translated: bool = False
    code_switch: bool = False
    segments: list[dict[str, str]] = field(default_factory=list)


async def process_inbound_text(
    text: str,
    translator: TranslatorInterface,
    *,
    enabled: bool = True,
    path: str = "chat",
) -> InboundResult:
    """Inbound seam (A2): detect the caller language, translate pure
    pcm/yo/ig/ha turns to English for routing/LLM.

    Code-switched pcm/en turns (A4.1) are NOT force-translated wholesale —
    they pass through annotated (``code_switch=True`` + segments); only pure
    non-English turns go through full translation. Fail-open: detection and
    translation errors never alter or block the turn.
    """
    original = text or ""
    if not enabled or not original.strip():
        return InboundResult(text=original, original_text=original)
    try:
        detected = detect_language(original)
    except Exception as exc:  # noqa: BLE001 - detection must never break a turn
        log.warning("language detection failed; passing through", error=str(exc)[:200])
        return InboundResult(text=original, original_text=original)
    metrics.language_detected(detected, path)
    if detected not in TRANSLATABLE_LANGUAGES:
        return InboundResult(
            text=original, detected_language=detected, original_text=original
        )
    # A4.1: pcm/en code-switch guard (mixed turns keep their mixed text;
    # the locale instruction tells the LLM to answer in kind).
    if detected == "pcm":
        from .codeswitch import analyze

        try:
            switch = analyze(original)
        except Exception as exc:  # noqa: BLE001 - annotation must never break a turn
            log.warning("code-switch analysis failed; treating as pure pcm", error=str(exc)[:200])
            switch = None
        if switch is not None and switch.code_switch:
            return InboundResult(
                text=original,
                detected_language=detected,
                original_text=original,
                code_switch=True,
                segments=[
                    {"text": s.text, "lang": s.lang} for s in switch.segments
                ],
            )
    result = await _safe_translate(
        translator, original, detected, "en", direction="inbound"
    )
    return InboundResult(
        text=result.text,
        detected_language=detected,
        original_text=original,
        translated=result.text != original,
    )


async def process_outbound_text(
    text: str,
    target_lang: str | None,
    translator: TranslatorInterface,
    *,
    enabled: bool = True,
) -> str:
    """Outbound seam (A3): English reply -> caller language before TTS (voice)
    or before the chat reply payload.

    Fail-open to the English reply on any error. Skipped when the reply is
    already substantially non-English (detector sees the caller's language
    or a code-switch rather than English — the A4.1 pass-through).
    """
    if not enabled or not text or not text.strip():
        return text
    lang = normalize_language(target_lang)
    if not lang or lang == "en" or lang not in TRANSLATABLE_LANGUAGES:
        return text
    try:
        if detect_language(text) != "en":
            return text  # already (substantially) non-English
    except Exception:  # noqa: BLE001 - guard only; translate below fails open
        pass
    result = await _safe_translate(
        translator, text, "en", lang, direction="outbound"
    )
    return result.text
