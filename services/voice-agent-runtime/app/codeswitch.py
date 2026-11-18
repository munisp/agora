"""SPEC-W82 A4.1: code-switch-aware turns (mixed Nigerian Pidgin / English).

Naija callers routinely mix Pidgin and English inside ONE utterance
("Wetin be the price for the braids?"). Wholesale force-translating such a
turn would mangle the English half and lose the caller's register, so this
module provides a SEGMENT-LEVEL heuristic splitter instead:

- :func:`analyze` classifies every token as pcm/en (token-class scoring
  reusing the translation.py Pidgin lexicon + phrase markers), groups
  consecutive same-class tokens into segments, and reports whether the
  utterance is genuinely code-switched.
- Code-switched turns are annotated on the turn context
  (``code_switch=True`` + segment list) and get the
  :func:`instruction` locale addendum so the LLM answers in the same
  natural mixed register. They are NOT force-translated — only pure
  non-English turns go through full translation (app/translation.py
  process_inbound_text).

Pure functions, no dependencies, unit-tested.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field

from .translation import PIDGIN_PHRASES, PIDGIN_WORDS

_WORD_RE = re.compile(r"\w+|\S")


@dataclass
class Segment:
    """One maximal run of same-class tokens in an utterance."""

    text: str
    lang: str  # "pcm" | "en"


@dataclass
class CodeSwitchResult:
    code_switch: bool
    segments: list[Segment] = field(default_factory=list)
    pcm_tokens: int = 0
    en_tokens: int = 0


def _token_classes(text: str) -> list[tuple[str, str]]:
    """Classify each whitespace/punctuation token as pcm/en/``""`` (punct).

    Word tokens score against the Pidgin lexicon; multi-word Pidgin phrase
    markers ("how far", "no dey", ...) tag every word they cover. Any word
    without a Pidgin signal is treated as English (the analysis only runs
    on utterances the detector already flagged as pcm/English-mixed).
    """
    words = re.findall(r"\w+", text.lower())
    flags = ["en"] * len(words)
    for i, word in enumerate(words):
        if word in PIDGIN_WORDS:
            flags[i] = "pcm"
    for phrase in PIDGIN_PHRASES:
        parts = phrase.split()
        n = len(parts)
        for i in range(len(words) - n + 1):
            if words[i : i + n] == parts:
                for j in range(i, i + n):
                    flags[j] = "pcm"
    # Rebuild the token stream WITH punctuation/spacing for readable segments.
    out: list[tuple[str, str]] = []
    word_iter = iter(zip(words, flags))
    current = next(word_iter, None)
    for token in _WORD_RE.findall(text):
        if re.fullmatch(r"\w+", token):
            if current is not None and token.lower() == current[0]:
                out.append((token, current[1]))
                current = next(word_iter, None)
            else:  # defensive: case fold mismatch — treat as English
                out.append((token, "en"))
        else:
            out.append((token, ""))
    return out


def analyze(text: str) -> CodeSwitchResult:
    """Segment an utterance into pcm/en runs and report code-switching.

    A turn counts as code-switched when it carries at least one Pidgin
    token AND at least two English word tokens — a lone Pidgin marker
    inside English ("Abeg, one moment") is borderline by design; the
    two-English-token floor keeps single-word English borrowings inside
    Pidgin speech ("Wetin be price?") from being misread as a switch.
    Pure-en and pure-pcm utterances pass through as one segment with
    ``code_switch=False``.
    """
    raw = (text or "").strip()
    if not raw:
        return CodeSwitchResult(code_switch=False)
    classified = _token_classes(raw)
    pcm_tokens = sum(1 for _, cls in classified if cls == "pcm")
    en_tokens = sum(1 for _, cls in classified if cls == "en")

    segments: list[Segment] = []
    for token, cls in classified:
        lang = cls or (segments[-1].lang if segments else "en")
        if segments and segments[-1].lang == lang:
            sep = "" if not cls else " "
            segments[-1].text = f"{segments[-1].text}{sep}{token}"
        else:
            segments.append(Segment(text=token, lang=lang))

    return CodeSwitchResult(
        code_switch=pcm_tokens >= 1 and en_tokens >= 2,
        segments=segments,
        pcm_tokens=pcm_tokens,
        en_tokens=en_tokens,
    )


def instruction() -> str:
    """Locale-instruction addendum for a code-switched pcm/en turn.

    Tells the LLM the caller mixes Pidgin and English and to answer in the
    same natural register — instead of force-translating the mixed turn.
    """
    return (
        "\nLANGUAGE (this turn)\n"
        "- The caller is code-switching: they mix Nigerian Pidgin and English "
        "in the same turn. Respond naturally in the same mixed Pidgin-English "
        "register; keep tool arguments (ids, dates, phone numbers) unchanged.\n"
    )
