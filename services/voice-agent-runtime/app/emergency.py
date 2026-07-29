"""Live-turn emergency detection (SPEC-W11 Part C).

A deliberately small lexicon-first classifier that runs on every committed
user turn of a live voice call. On the first hit the LiveKit worker latches
the per-session :class:`EmergencyState`, counts the session
(``voice_emergency_sessions_total``), triggers the EXISTING warm-handoff
escalation path with reason ``"emergency"`` and injects a location-first
prompt addendum (app/prompts.py).

MIRROR NOTE: the keyword/severity/hazard class list below intentionally
duplicates the Wave-11 Part A emergency-intent lexicon in
conversation-service ``app/incidents.py`` (same classes: critical
dying/kidnap/armed robbery/blood, high emergency/accident/thief dey/
collapse/fire, hazards weapons/injuries/fire/gas/traffic). The two services
must agree on what counts as an emergency, but the voice runtime cannot
import the conversation-service module, so the small lexicon is mirrored
here BY DESIGN — keep both in sync when extending it.

This module is deliberately free of livekit imports so it stays unit-testable
without the server SDK.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

# ---------------------------------------------------------------------------
# Lexicon (EN + Nigerian Pidgin). Matching is case-insensitive substring over
# the normalized transcript — phrases first so multi-word idioms ("thief dey",
# "armed robbery") win over their single-word parts.
# ---------------------------------------------------------------------------

# critical: life-threatening / violent crime in progress.
CRITICAL_KEYWORDS: tuple[str, ...] = (
    "dying",
    "kidnap",
    "kidnapped",
    "kidnapping",
    "armed robbery",
    "armed robber",
    "blood",
    "bleeding to death",
    "rape",
    "they are shooting",
    "gun shot",
    "gunshot",
    "he has a gun",
    "she has a gun",
    "don kill",  # PCM: "dem don kill person" (someone has been killed)
    "kill am",  # PCM: "they want to kill him/her"
    "machete",
)

# high: urgent but not necessarily life-threatening.
HIGH_KEYWORDS: tuple[str, ...] = (
    "emergency",
    "help me",
    "accident",
    "thief dey",  # PCM: "thief dey my house" (there is a thief)
    "thief don enter",  # PCM: a thief has broken in
    "collapse",
    "collapsed",
    "fire",
    "robbery",
    "robbers",
    "attack",
    "attacked",
    "unconscious",
    "not breathing",
    "can't breathe",
    "cannot breathe",
    "fire dey burn",  # PCM
    "e don happen",  # PCM: "it has happened" (something bad just occurred)
    "wahala",  # PCM: serious trouble
)

# Hazard extraction: keyword -> canonical hazard class (IDP `hazards` enum:
# weapons | injuries | fire | gas | traffic).
HAZARD_KEYWORDS: dict[str, tuple[str, ...]] = {
    "weapons": (
        "gun",
        "gunshot",
        "gun shot",
        "shooting",
        "knife",
        "machete",
        "weapon",
        "armed",
    ),
    "injuries": (
        "blood",
        "bleeding",
        "injured",
        "injury",
        "hurt",
        "wounded",
        "dying",
        "unconscious",
        "not breathing",
        "collapsed",
        "collapse",
        "broken bone",
    ),
    "fire": (
        "fire",
        "burning",
        "smoke",
        "explosion",
    ),
    "gas": (
        "gas leak",
        "gas dey leak",  # PCM
        "smell gas",
        "gas cylinder",
    ),
    "traffic": (
        "accident",
        "crash",
        "crashed",
        "collision",
        "hit and run",
        "hit-and-run",
        "knock down",  # PCM: vehicle knocked someone down
        "motor accident",  # PCM phrasing
    ),
}

SEVERITY_ORDER = ("low", "medium", "high", "critical")


def is_emergency(text: str) -> tuple[bool, str | None, list[str]]:
    """Classify one user turn.

    Returns ``(matched, severity, hazards)``: ``matched`` is True when any
    critical/high keyword hits; ``severity`` is ``"critical"`` when any
    critical keyword matched, else ``"high"`` when a high keyword matched,
    else None; ``hazards`` is the sorted list of hazard classes extracted
    from the text (may be non-empty even when ``matched`` is False — the
    caller only acts on matched turns).
    """
    if not text:
        return False, None, []
    lowered = " ".join(str(text).lower().split())

    critical = any(kw in lowered for kw in CRITICAL_KEYWORDS)
    high = critical or any(kw in lowered for kw in HIGH_KEYWORDS)
    severity = "critical" if critical else ("high" if high else None)

    hazards = sorted(
        hazard
        for hazard, keywords in HAZARD_KEYWORDS.items()
        if any(kw in lowered for kw in keywords)
    )
    return high, severity, hazards


def message_text(message: Any) -> str:
    """Defensively extract transcript text from a livekit ChatMessage (or a
    plain string). Returns "" when nothing usable is present — the emergency
    hook must never raise on an unexpected event payload shape."""
    if message is None:
        return ""
    if isinstance(message, str):
        return message
    content = getattr(message, "content", None)
    if isinstance(content, str):
        return content
    if isinstance(content, (list, tuple)):
        parts = [str(c) for c in content if isinstance(c, (str, int, float))]
        if parts:
            return " ".join(parts)
    text = getattr(message, "text", None)
    if isinstance(text, str):
        return text
    return ""


@dataclass
class EmergencyState:
    """Per-session emergency latch (SPEC-W11 Part C).

    One instance per voice session. `observe` runs the classifier on a user
    turn and returns ``(severity, hazards)`` ONLY on the first matched turn —
    the trigger (escalation, metric, addendum) fires exactly once per
    session, while the latched state remains readable afterwards. Severity
    can still escalate (high -> critical) on later turns without re-firing
    the trigger.
    """

    matched: bool = False
    severity: str | None = None
    hazards: list[str] = field(default_factory=list)

    def observe(self, text: str) -> tuple[str, list[str]] | None:
        matched, severity, hazards = is_emergency(text)
        if not matched:
            return None
        first = not self.matched
        self.matched = True
        # Latch the highest severity seen and the union of hazards.
        if self.severity is None or SEVERITY_ORDER.index(
            severity or "low"
        ) > SEVERITY_ORDER.index(self.severity):
            self.severity = severity
        for hazard in hazards:
            if hazard not in self.hazards:
                self.hazards.append(hazard)
        if first:
            return self.severity or "high", list(self.hazards)
        return None
