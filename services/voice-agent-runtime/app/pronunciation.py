"""SPEC-W82 A4.5: per-tenant Naija name pronunciation lexicon.

TTS engines routinely butcher Nigerian names and terms ("Adaeze",
"Chukwuemeka", "Ogunleye"). A per-tenant lexicon maps a term to a plain
RESPELLING (any provider) and/or an IPA phoneme string (Azure SSML):

    {"Adaeze": {"respell": "ah-DAH-ay-zeh", "ipa": "ɑːdɑːˈɛzɛ"},
     "Ogunleye": "oh-goon-LEH-yeh"}

Config sources and precedence (documented in NOTES.md):
1. ``*`` (global) entries of the VOICE_PRONUNCIATION_LEXICON JSON env;
2. the tenant-slug-keyed entries of the same env JSON;
3. the pack ``voice.lexicon`` block (per-tenant, highest precedence —
   pack entries override env entries for the same term).

:func:`apply_pronunciation` runs pre-TTS: for the ``azure`` provider a term
with an IPA value is wrapped in an SSML ``<phoneme>`` element; every other
provider (mms/spitch/piper/xtts/...) gets the plain respell substitution.
Matching is longest-match-first, word-boundary safe and case-insensitive.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass
from typing import Any

from .logging import get_logger

log = get_logger("pronunciation")


@dataclass
class PronSpec:
    """One lexicon entry: plain respelling and/or IPA phoneme."""

    respell: str = ""
    ipa: str = ""


def parse_lexicon(raw: Any) -> dict[str, PronSpec]:
    """Parse ONE tenant's lexicon mapping {term: respell | {respell, ipa}}.

    Tolerant: non-dict input, empty terms and malformed values drop out with
    a warning; a bare string value is a respelling.
    """
    if not isinstance(raw, dict):
        return {}
    out: dict[str, PronSpec] = {}
    for term, value in raw.items():
        term_s = str(term).strip()
        if not term_s:
            continue
        if isinstance(value, str):
            spec = PronSpec(respell=value.strip())
        elif isinstance(value, dict):
            spec = PronSpec(
                respell=str(value.get("respell") or "").strip(),
                ipa=str(value.get("ipa") or "").strip(),
            )
        else:
            log.warning("pronunciation lexicon: dropping malformed entry", term=term_s[:40])
            continue
        if not spec.respell and not spec.ipa:
            log.warning("pronunciation lexicon: dropping empty entry", term=term_s[:40])
            continue
        out[term_s] = spec
    return out


def parse_tenant_lexicons(raw: str | dict | None) -> dict[str, dict[str, PronSpec]]:
    """Parse the VOICE_PRONUNCIATION_LEXICON env JSON keyed by tenant slug
    (``"*"`` = global entries applied to every tenant)."""
    if not raw:
        return {}
    data: Any = raw
    if isinstance(raw, str):
        try:
            data = json.loads(raw)
        except json.JSONDecodeError:
            log.warning("VOICE_PRONUNCIATION_LEXICON is not valid JSON; ignoring")
            return {}
    if not isinstance(data, dict):
        log.warning("VOICE_PRONUNCIATION_LEXICON must be a JSON object; ignoring")
        return {}
    out: dict[str, dict[str, PronSpec]] = {}
    for slug, lexicon in data.items():
        parsed = parse_lexicon(lexicon)
        if parsed:
            out[str(slug).strip()] = parsed
    return out


def merge_lexicons(
    env_lexicons: dict[str, dict[str, PronSpec]] | None,
    tenant_slug: str,
    pack_lexicon: dict[str, PronSpec] | None = None,
) -> dict[str, PronSpec]:
    """Effective lexicon for a tenant. Precedence (low -> high):
    env ``*`` global < env tenant-slug < pack ``voice.lexicon``.
    """
    merged: dict[str, PronSpec] = {}
    for source in (
        (env_lexicons or {}).get("*"),
        (env_lexicons or {}).get(tenant_slug),
        pack_lexicon,
    ):
        if source:
            # Case-insensitive term identity: later sources override earlier
            # ones for the same term regardless of casing.
            lowered = {k.lower(): k for k in merged}
            for term, spec in source.items():
                existing = lowered.get(term.lower())
                if existing is not None and existing != term:
                    del merged[existing]
                merged[term] = spec
    return merged


def _boundary_pattern(term: str) -> re.Pattern:
    """Word-boundary-safe pattern for a term (case-insensitive). Boundaries
    are lookarounds on word characters so multi-word terms and terms next to
    punctuation still match, while substrings of longer words never do."""
    return re.compile(r"(?<!\w)" + re.escape(term) + r"(?!\w)", re.IGNORECASE)


def apply_pronunciation(
    text: str,
    lexicon: dict[str, PronSpec] | None,
    *,
    provider: str = "",
) -> str:
    """Apply the lexicon to TTS-bound text.

    - ``provider == "azure"``: terms with an IPA value are wrapped in SSML
      ``<phoneme alphabet="ipa" ph="...">`` elements (Azure neural voices);
      terms with only a respelling get the respell substitution.
    - every other provider: plain respell substitution; IPA-only entries
      are skipped (no SSML support -> no change rather than broken markup).

    Longest-match-first so "Chukwuemeka" wins over a "Chukwu" entry;
    matching is word-boundary safe and case-insensitive.
    """
    if not text or not lexicon:
        return text
    out = text
    for term in sorted(lexicon, key=len, reverse=True):
        spec = lexicon[term]
        if provider == "azure" and spec.ipa:
            replacement = (
                f'<phoneme alphabet="ipa" ph="{spec.ipa}">\\g<0></phoneme>'
            )
        elif spec.respell:
            replacement = spec.respell.replace("\\", "\\\\")
        else:
            continue
        out = _boundary_pattern(term).sub(replacement, out)
    return out
