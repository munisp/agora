"""Multilingual receptionist (Wave 5 #3, STRATEGY §3).

Three cooperating pieces, all driven by what the caller actually speaks:

1. Detection — whisper auto-detects the spoken language per utterance.
   faster-whisper exposes it on the transcription ``info.language`` and (for
   verbose results) as a ``language`` field on result segments; the LiveKit
   STT bridge (app/livekit_worker.py WhisperSTTNode) forwards it onto the
   session's :class:`MultilangState`.
2. LLM per-turn locale instruction — when the detected language changes, the
   system prompt gains ``locale_instruction(language)`` ("respond in
   {language}") so the next LLM turn answers in the caller's language. The
   tenant's identity locale (TenantContext.locale) sets the DEFAULT language
   used until whisper detects otherwise.
3. Piper voice map — ``PIPER_VOICE_MAP`` JSON ``{"en": "en_US-lessac-medium",
   "es": "es_ES-sharvard-medium"}`` picks a native voice per language with a
   graceful fallback to the default ``PIPER_VOICE`` when a language has no
   entry (the call stays up; it just speaks with the default accent).

Pack ``languages`` field: industry packs may declare ``languages: [en, es]``
(the languages the tenant's deployment supports). The identity service (Go)
passes pack fields through unvalidated, so validation lives HERE at the
voice runtime's pack consumption point (tenant_context._apply_pack ->
validate_pack_languages) — invalid entries are dropped with a warning, never
fatal. Declared languages bound the auto-switch set: detection outside the
pack's list falls back to the tenant default language.

Nigerian Pidgin (pcm) note: whisper has no distinct Pidgin detection —
Pidgin speech is transcribed and reported as English (``en``). Pidgin is
therefore handled at the PERSONA level via the industry pack
(industries/nigeria-sme.yaml declares ``languages: [en, pcm]`` and its
agentPersona carries the code-switching rules), NOT via a locale switch.
If ``pcm`` ever appears as a detection or pack entry, :func:`pidgin_proxy`
maps it to ``en`` so the locale instruction and Piper voice stay English.
Likewise there is no Pidgin Piper voice: PIPER_VOICE_MAP has no ``pcm``
entry, so Pidgin deployments speak with the configured English voice —
that is the honest, intended behaviour until a pcm TTS voice exists.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass, field
from typing import Any, Iterable

from .logging import get_logger

log = get_logger("multilang")

_LANG_RE = re.compile(r"^[a-z]{2,3}(-[a-zA-Z]{2,4})?$")

# English display names for the per-turn locale instruction (LLMs follow an
# explicit language name more reliably than a bare ISO code).
LANGUAGE_NAMES = {
    "en": "English",
    "es": "Spanish",
    "fr": "French",
    "de": "German",
    "it": "Italian",
    "pt": "Portuguese",
    "nl": "Dutch",
    "pl": "Polish",
    "sv": "Swedish",
    "ar": "Arabic",
    "zh": "Chinese",
    "ja": "Japanese",
    "ko": "Korean",
    "hi": "Hindi",
    "el": "Greek",
    "tr": "Turkish",
    # Nigerian Pidgin (ISO-639-3 pcm). Whisper never reports pcm — Pidgin
    # speech comes back as 'en' — so this name exists only for packs that
    # declare languages: [en, pcm]; see pidgin_proxy() below and the module
    # docstring. There is no Pidgin Piper voice (PIPER_VOICE_MAP falls back
    # to the default English voice — honest limitation, documented).
    "pcm": "Nigerian Pidgin",
}

# Whisper cannot detect Nigerian Pidgin distinctly: pcm audio is reported as
# English. Pidgin is therefore a PERSONA-level concern (the industry pack's
# agentPersona carries code-switching rules), not a locale-switch concern.
# Any pcm that does reach the language pipeline (pack declaration, manual
# override, a future STT engine) is proxied to English so the locale
# instruction and TTS voice selection behave sanely.
PIDGIN_CODES = frozenset({"pcm"})
PIDGIN_PROXY_LANGUAGE = "en"


def pidgin_proxy(code: str) -> str:
    """Map Nigerian Pidgin (``pcm``) to its runtime proxy language (``en``);
    every other code passes through unchanged. Expects an already-normalized
    primary subtag (see :func:`normalize_language`)."""
    return PIDGIN_PROXY_LANGUAGE if code in PIDGIN_CODES else code


def normalize_language(code: str | None) -> str:
    """Normalize a language/locale tag to the ISO-639 primary subtag in
    lowercase: ``es-ES`` -> ``es``, ``EN_us`` -> ``en``. Returns "" for
    unusable input."""
    if not code:
        return ""
    tag = str(code).strip().replace("_", "-")
    if not _LANG_RE.match(tag.lower()) and not _LANG_RE.match(tag):
        return ""
    return tag.split("-", 1)[0].lower()


def default_language_from_locale(locale: str | None) -> str:
    """Tenant identity locale (``en-US``) -> default language (``en``)."""
    return normalize_language(locale) or "en"


def validate_pack_languages(raw: Any) -> list[str]:
    """Validate a pack's optional ``languages: [en, es]`` field (voice-runtime
    side — identity passes packs through unchecked; see module docstring).
    Returns normalized primary subtags, dropping invalid entries."""
    if not isinstance(raw, list):
        return []
    out: list[str] = []
    for item in raw:
        lang = normalize_language(str(item) if item is not None else "")
        if not lang:
            log.warning("pack languages: dropping invalid entry", entry=str(item)[:40])
            continue
        if lang not in out:
            out.append(lang)
    return out


def _segment_language(segment: Any) -> str:
    if isinstance(segment, dict):
        return normalize_language(segment.get("language"))
    return normalize_language(getattr(segment, "language", None))


def detect_language_from_segments(segments: Iterable[Any]) -> str:
    """Detect the dominant language from whisper STT result segments (each
    carrying a ``language`` field, as attribute or dict key). Majority vote
    across segments; "" when no segment reports a language."""
    counts: dict[str, int] = {}
    for seg in segments or ():
        lang = _segment_language(seg)
        if lang:
            counts[lang] = counts.get(lang, 0) + 1
    if not counts:
        return ""
    return max(counts, key=counts.get)


def detect_language_from_info(info: Any) -> str:
    """faster-whisper ``TranscriptionInfo.language`` -> normalized code."""
    return normalize_language(getattr(info, "language", None))


def resolve_turn_language(
    detected: str,
    *,
    default: str,
    supported: list[str] | None = None,
) -> str:
    """Decide the language for the NEXT turn.

    ``supported`` (pack ``languages``) bounds the auto-switch set: detection
    outside the list falls back to the tenant default. An empty/None list
    means unconstrained (any detected language switches).

    Nigerian Pidgin (``pcm``) detections are proxied to English
    (:func:`pidgin_proxy`) BEFORE the supported-set check, so a pack that
    declares ``languages: [en, pcm]`` matches pcm input without a locale
    switch away from English."""
    lang = pidgin_proxy(normalize_language(detected))
    if not lang:
        return default
    if supported and lang not in supported:
        log.info(
            "detected language outside pack languages; keeping tenant default",
            detected=lang,
            default=default,
        )
        return default
    return lang


def locale_instruction(language: str) -> str:
    """Per-turn LLM system-prompt instruction ("respond in {language}").

    Pidgin (pcm) is proxied to English: Pidgin register is driven by the
    pack persona, not by a locale instruction (see module docstring)."""
    lang = pidgin_proxy(normalize_language(language)) or "en"
    name = LANGUAGE_NAMES.get(lang, lang)
    return (
        "\nLANGUAGE (this turn)\n"
        f"- The caller is speaking {name}. Respond in {name} for the rest of "
        "the conversation unless the caller switches languages; keep tool "
        "arguments (ids, dates, phone numbers) unchanged.\n"
    )


def parse_voice_map(raw: str | dict | None) -> dict[str, str]:
    """Parse ``PIPER_VOICE_MAP`` JSON ``{"en": "en_US-lessac-medium", ...}``.
    Tolerant: invalid JSON / non-string values drop out with a warning."""
    if not raw:
        return {}
    data: Any = raw
    if isinstance(raw, str):
        try:
            data = json.loads(raw)
        except json.JSONDecodeError:
            log.warning("PIPER_VOICE_MAP is not valid JSON; ignoring", raw=raw[:80])
            return {}
    if not isinstance(data, dict):
        log.warning("PIPER_VOICE_MAP must be a JSON object; ignoring")
        return {}
    out: dict[str, str] = {}
    for lang, voice in data.items():
        code = normalize_language(str(lang))
        if not code or not isinstance(voice, str) or not voice.strip():
            log.warning("PIPER_VOICE_MAP: dropping invalid entry", key=str(lang)[:40])
            continue
        out[code] = voice.strip()
    return out


def voice_for_language(
    language: str, voice_map: dict[str, str], default_voice: str
) -> str:
    """Pick the piper voice for a language, gracefully falling back to the
    default voice when the language has no mapping (or is empty).

    No Pidgin Piper voice exists: pcm is proxied to English here, so a pcm
    turn speaks with the configured English voice (or an explicit ``en``
    entry in the voice map)."""
    lang = pidgin_proxy(normalize_language(language))
    if lang and lang in voice_map:
        return voice_map[lang]
    return default_voice


# ---------------------------------------------------------------------------
# SPEC-W10 Part A: provider-qualified voice routing (TTS_VOICE_MAP).
#
# TTS_VOICE_MAP JSON maps language tags to ``"provider:voiceId"`` values:
#   {"pcm": "mms:pcm", "en-NG": "azure:en-NG-EzinneNeural", "yo": "mms:yor",
#    "ha": "mms:hau", "ig": "mms:ibo"}
# Unlike PIPER_VOICE_MAP, keys keep their region subtag ("en-NG" is distinct
# from "en") and ``pcm`` is NOT proxied to English — MMS/Spitch have real
# Pidgin/Yoruba/Hausa/Igbo voices, so the pidgin_proxy hack only applies to
# the piper fallback. When TTS_VOICE_MAP is unset, resolution is identical
# to voice_for_language() (byte-identical pre-W10 behavior).
# ---------------------------------------------------------------------------
def _normalize_voice_key(code: str | None) -> str:
    """Normalize a language tag for TTS_VOICE_MAP keys, KEEPING the region
    subtag: ``"en-NG"`` -> ``"en-ng"``, ``"EN_us"`` -> ``"en-us"``.
    Returns "" for unusable input."""
    if not code:
        return ""
    tag = str(code).strip().replace("_", "-")
    if not _LANG_RE.match(tag.lower()):
        return ""
    return tag.lower()


def parse_tts_voice_map(raw: str | dict | None) -> dict[str, str]:
    """Parse ``TTS_VOICE_MAP`` JSON ``{"pcm": "mms:pcm", ...}``.

    Values are provider-qualified (``"provider:voiceId"``) or bare voice ids
    (legacy piper semantics). Tolerant: invalid JSON, invalid keys and
    malformed values drop out with a warning."""
    if not raw:
        return {}
    data: Any = raw
    if isinstance(raw, str):
        try:
            data = json.loads(raw)
        except json.JSONDecodeError:
            log.warning("TTS_VOICE_MAP is not valid JSON; ignoring", raw=raw[:80])
            return {}
    if not isinstance(data, dict):
        log.warning("TTS_VOICE_MAP must be a JSON object; ignoring")
        return {}
    out: dict[str, str] = {}
    for lang, voice in data.items():
        key = _normalize_voice_key(str(lang))
        if not key or not isinstance(voice, str) or not voice.strip():
            log.warning("TTS_VOICE_MAP: dropping invalid entry", key=str(lang)[:40])
            continue
        value = voice.strip()
        if ":" in value:
            provider, _, voice_id = value.partition(":")
            if not re.match(r"^[a-z][a-z0-9]*$", provider.strip().lower()) or (
                not voice_id.strip()
            ):
                log.warning(
                    "TTS_VOICE_MAP: dropping malformed provider:voiceId", key=key
                )
                continue
            value = f"{provider.strip().lower()}:{voice_id.strip()}"
        out[key] = value
    return out


def resolve_tts_voice(
    language: str,
    tts_voice_map: dict[str, str],
    piper_voice_map: dict[str, str],
    default_voice: str,
    *,
    ctx: Any = None,
) -> str:
    """Resolve the voice spec for a language turn.

    Precedence:
    1. Per-tenant override ``ctx.tts_voice`` (defensive getattr; may itself
       be provider-qualified) — pack/identity-supplied tenant voice.
    2. ``TTS_VOICE_MAP`` (provider-qualified): full tag match ("en-ng")
       then primary subtag ("en"); pcm is matched literally, NOT proxied.
    3. Legacy ``PIPER_VOICE_MAP`` via :func:`voice_for_language` (pidgin
       proxy + default fallback) — byte-identical to pre-W10 behavior when
       TTS_VOICE_MAP is unset and no tenant override exists.
    """
    if ctx is not None:
        override = getattr(ctx, "tts_voice", None)
        if isinstance(override, str) and override.strip():
            return override.strip()
    key = _normalize_voice_key(language)
    if key and tts_voice_map:
        if key in tts_voice_map:
            return tts_voice_map[key]
        primary = key.split("-", 1)[0]
        if primary in tts_voice_map:
            return tts_voice_map[primary]
    return voice_for_language(language, piper_voice_map, default_voice)


@dataclass
class MultilangState:
    """Per-session language tracker.

    ``default_language`` comes from the tenant identity locale;
    ``supported`` from the pack ``languages`` field (empty = unconstrained).
    ``observe()`` applies a detection and reports whether the active language
    changed (the worker then swaps the piper voice + locale instruction).
    """

    default_language: str = "en"
    supported: list[str] = field(default_factory=list)
    active_language: str = ""

    def __post_init__(self) -> None:
        self.default_language = normalize_language(self.default_language) or "en"
        self.supported = validate_pack_languages(self.supported)
        if not self.active_language:
            self.active_language = self.default_language

    @classmethod
    def from_context(cls, ctx: Any) -> "MultilangState":
        """Build from a TenantContext (locale -> default, pack languages ->
        supported set)."""
        return cls(
            default_language=default_language_from_locale(getattr(ctx, "locale", "en-US")),
            supported=list(getattr(ctx, "languages", []) or []),
        )

    def observe(self, detected: str) -> bool:
        """Apply a whisper language detection; True when the language switched.
        An empty detection (no language reported) keeps the current language."""
        if not normalize_language(detected):
            return False
        new = resolve_turn_language(
            detected, default=self.default_language, supported=self.supported
        )
        if new != self.active_language:
            log.info(
                "language switch",
                previous=self.active_language,
                detected=normalize_language(detected),
                active=new,
            )
            self.active_language = new
            return True
        return False

    def instruction(self) -> str:
        """Locale instruction for the active language ("" when the caller
        speaks the tenant default — the base prompt already covers it)."""
        if self.active_language == self.default_language:
            return ""
        return locale_instruction(self.active_language)
