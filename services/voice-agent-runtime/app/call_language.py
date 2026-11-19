"""The language a caller chose for a voice call (call page picker -> POST /voice/session {language}).

The choice travels in the caller's LiveKit token metadata ({"language": "ig"}); the worker reads it when the caller
joins and, for a Nigerian language, hears the caller with MMS speech recognition and answers with the naija-speech
voices (services/naija-speech) instead of whisper/piper. Whisper's own per-utterance detection is NOT used for these
calls: it cannot tell Igbo or Pidgin at all, and would flip the call back to English.

Fulfulde (ful) is supported by the speech service but not offered by default (CALL_LANGUAGES): the receptionist's LLM
does not write usable Fulfulde yet (tested 2026-11-16), so a Fulfulde call could not be answered.
"""
from __future__ import annotations

import json
from typing import Any

# code -> (English name, name in the language itself)
NAMES: dict[str, tuple[str, str]] = {
    "en": ("English", "English"),
    "pcm": ("Nigerian Pidgin", "Pidgin"),
    "yo": ("Yoruba", "Yorùbá"),
    "ha": ("Hausa", "Hausa"),
    "ig": ("Igbo", "Igbo"),
    "ful": ("Fulfulde", "Fulfulde"),
}
NIGERIAN = frozenset({"pcm", "yo", "ha", "ig", "ful"})

_GREETINGS = {
    "pcm": "Welcome to {name}! How I fit help you today?",
    "yo": "Ẹ kú àbọ̀ sí {name}! Báwo ni mo ṣe lè ràn yín lọ́wọ́ lónìí?",
    "ha": "Barka da zuwa {name}! Yaya zan iya taimaka maka yau?",
    "ig": "Nnọọ na {name}! Kedu ka m ga-esi nyere gị aka taa?",
}


def allowed(setting: str) -> list[str]:
    """CALL_LANGUAGES -> known codes, English always first."""
    codes = [c.strip().lower() for c in (setting or "").split(",") if c.strip()]
    out = ["en"] + [c for c in codes if c in NAMES and c != "en"]
    return list(dict.fromkeys(out))


def normalize(code: str | None, setting: str) -> str:
    """A requested language if the deployment offers it, else "" (English / no choice)."""
    c = (code or "").strip().lower()
    return c if c in allowed(setting) and c != "en" else ""


def token_metadata(language: str) -> str:
    return json.dumps({"language": language}) if language else ""


def from_metadata(raw: Any) -> str:
    """The chosen language from a participant's metadata JSON ("" when none or unreadable)."""
    if not raw:
        return ""
    try:
        data = json.loads(raw) if isinstance(raw, str) else raw
    except ValueError:
        return ""
    lang = data.get("language") if isinstance(data, dict) else None
    return lang.strip().lower() if isinstance(lang, str) and lang.strip().lower() in NAMES else ""


# Said first on every call, before anything else (the business's English greeting carries its own notice).
RECORDING_NOTICES = {
    "pcm": "Dis call fit dey recorded.",
    "yo": "A lè ṣe ìgbàsílẹ̀ ìpè yìí.",
    "ha": "Ana iya nadar wannan kiran.",
    "ig": "Enwere ike ịdekọ oku a.",
}


def greeting(language: str, business: str, english_greeting: str) -> str:
    """The opening line in the chosen language, starting with the recording notice (the business's own English
    greeting otherwise, which starts with its own notice)."""
    tpl = _GREETINGS.get(language)
    if tpl and business:
        return f"{RECORDING_NOTICES.get(language, '')} {tpl.format(name=business)}".strip()
    return english_greeting


def prompt_instruction(language: str) -> str:
    """Appended to the system prompt for the whole call."""
    if not language or language == "en":
        return ""
    name = NAMES[language][0]
    return (
        f"\n\nLANGUAGE: The caller chose to speak {name} on this call. Their words reach you transcribed by a speech "
        f"recogniser, so spelling and tone marks may be imperfect — read for meaning. Reply ONLY in {name}, in short, "
        f"simple spoken sentences (no lists, no symbols, no emojis): your reply is read aloud by a {name} voice. Say "
        f"numbers, dates and times in words. Keep names of services, people and the business as they are. Callers "
        f"often give dates, names, phone numbers and emails in English: that is normal — keep replying in {name}. "
        f"Only answer in English if the caller asks to continue in English."
    )
