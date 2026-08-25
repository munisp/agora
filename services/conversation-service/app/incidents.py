"""Emergency-intent detection + Incident Data Packet (IDP) emission
(SPEC-W11 Part A).

Lexicon-first classifier (EN + Nigerian Pidgin/PCM) following the app/intel.py
lexicon pattern: weighted keyword/phrase hits produce an emergency score, a
severity tier, an incident_type and a hazard set. When a newly persisted USER
turn scores >= INCIDENT_MIN_SCORE, an IDP (canonical shape —
docs/schemas/incident-data-packet.json) is built and published as a CloudEvent
`com.opendesk.incidents.IDPCreated` to Kafka topic `opendesk.incidents` via the
service's existing Dapr pubsub producer pattern (same as the transcript event
in app/routes.py).

NOTE (SPEC-W11 Part C mirror): voice-agent-runtime app/emergency.py carries a
deliberate copy of this small lexicon for live-call detection. Keep the two in
sync when editing keywords/weights.

Emission contract:
- non-blocking: routes.py schedules emission as an asyncio task after the turn
  is persisted; failures are logged, never raised;
- idempotent per (conversation_id, turn_id): the dedupe key gates emission and
  incident_id is deterministic (uuid5 of the key), so downstream consumers can
  upsert on incident_id; the CloudEvent id IS the incident_id;
- DURABLE (SPEC-W43 Y-03): the dedupe gate is the `incident_emitted` table
  (PK (tenant_id, dedupe_key)), written in the SAME transaction as the
  `incident_counters` reference-number upsert. A Dapr publish failure leaves
  the row with published_at NULL and the IncidentRetryWorker republishes it
  — a failed emission is never silently dropped (the in-process
  `_emitted_keys` set is only a fast-path cache on top of the durable state);
- reference numbers are INC-{YYYY}-{seq:06d} per tenant per year from the
  Postgres `incident_counters` table (no Redis here). SPEC-W43 Y-06: the
  table is created by bootstrap DDL at service boot
  (Database.ensure_relay_tables, with fail-closed RLS + FORCE) — it is no
  longer lazily created inside the tenant-scoped transaction.
"""

from __future__ import annotations

import asyncio
import re
import uuid
from datetime import UTC, datetime
from typing import Any

from .config import Config
from .intel import normalize_text
from .logging import get_logger

log = get_logger(__name__)

SCHEMA_VERSION = "1.0"
EVENT_TYPE_IDP_CREATED = "com.opendesk.incidents.IDPCreated"
EVENT_SOURCE = "//conversation-service"

# ---------------------------------------------------------------------------
# Emergency lexicon (EN + PCM). Each entry:
#   (phrase, weight, severity, category)
# weight contributes to the emergency score (sum, capped at 1.0); severity is
# the tier the phrase asserts (the highest tier seen wins); category feeds
# incident_type (None = generic distress -> "other" unless a categorised
# phrase also matched).
# ---------------------------------------------------------------------------

_WEIGHT_CRITICAL = 1.0
_WEIGHT_HIGH = 0.75
_WEIGHT_MEDIUM = 0.5
_WEIGHT_LOW = 0.3

_SEVERITY_RANK = {"low": 1, "medium": 2, "high": 3, "critical": 4}

# (phrase, weight, severity, category|None)
EMERGENCY_LEXICON: tuple[tuple[str, float, str, str | None], ...] = (
    # --- critical: life-threatening right now -----------------------------
    ("dying", _WEIGHT_CRITICAL, "critical", "medical"),
    ("dey die", _WEIGHT_CRITICAL, "critical", "medical"),  # PCM: "e dey die"
    ("kidnap", _WEIGHT_CRITICAL, "critical", "crime"),
    ("kidnapped", _WEIGHT_CRITICAL, "critical", "crime"),
    ("armed robbery", _WEIGHT_CRITICAL, "critical", "crime"),
    ("fire dey burn", _WEIGHT_CRITICAL, "critical", "fire"),  # PCM
    ("blood everywhere", _WEIGHT_CRITICAL, "critical", "medical"),
    ("heart attack", _WEIGHT_CRITICAL, "critical", "medical"),
    ("murder", _WEIGHT_CRITICAL, "critical", "crime"),
    # --- high: emergency services needed ----------------------------------
    ("emergency", _WEIGHT_HIGH, "high", None),
    ("accident", _WEIGHT_HIGH, "high", "crash"),
    ("thief dey", _WEIGHT_HIGH, "high", "crime"),  # PCM: "thief dey my compound"
    ("collapse", _WEIGHT_HIGH, "high", "medical"),
    ("collapsed", _WEIGHT_HIGH, "high", "medical"),
    ("rape", _WEIGHT_HIGH, "high", "crime"),
    ("robbery", _WEIGHT_HIGH, "high", "crime"),
    ("shooting", _WEIGHT_HIGH, "high", "crime"),
    ("gunshot", _WEIGHT_HIGH, "high", "crime"),
    # --- medium: distress / hazard signals --------------------------------
    ("help me", _WEIGHT_MEDIUM, "medium", None),
    ("fire", _WEIGHT_MEDIUM, "medium", "fire"),
    ("blood", _WEIGHT_MEDIUM, "medium", "medical"),
    ("bleeding", _WEIGHT_MEDIUM, "medium", "medical"),
    ("thief", _WEIGHT_MEDIUM, "medium", "crime"),
    ("attack", _WEIGHT_MEDIUM, "medium", "crime"),
    ("injured", _WEIGHT_MEDIUM, "medium", "medical"),
    ("unconscious", _WEIGHT_MEDIUM, "medium", "medical"),
    ("gun", _WEIGHT_MEDIUM, "medium", "crime"),
    ("guns", _WEIGHT_MEDIUM, "medium", "crime"),
    ("knife", _WEIGHT_MEDIUM, "medium", "crime"),
    ("machete", _WEIGHT_MEDIUM, "medium", "crime"),
    ("ambulance", _WEIGHT_MEDIUM, "medium", "medical"),
    ("crash", _WEIGHT_MEDIUM, "medium", "crash"),
    ("smoke", _WEIGHT_MEDIUM, "medium", "fire"),
    ("gas leak", _WEIGHT_MEDIUM, "medium", "utility_fault"),
    ("intruder", _WEIGHT_MEDIUM, "medium", "security"),
    ("break in", _WEIGHT_MEDIUM, "medium", "security"),
    # --- low: urgency hints (never emit on their own at default threshold) -
    ("urgent", _WEIGHT_LOW, "low", None),
)

# Hazard extraction (IDP hazards enum: weapons|injuries|fire|gas|traffic).
HAZARD_LEXICON: dict[str, tuple[str, ...]] = {
    "weapons": (
        "gun", "guns", "gunshot", "shooting", "armed", "knife", "machete",
        "weapon", "pistol", "rifle",
    ),
    "injuries": (
        "blood", "blood everywhere", "bleeding", "injured", "injury",
        "unconscious", "wounded", "casualty",
    ),
    "fire": (
        "fire", "fire dey burn", "dey burn", "burning", "smoke", "flames",
        "explosion",
    ),
    "gas": ("gas leak", "gas", "lpg", "fumes"),
    "traffic": ("accident", "crash", "collision", "hit and run"),
}


def _phrase_re(phrase: str) -> re.Pattern[str]:
    return re.compile(r"\b" + re.escape(phrase) + r"\b")


_LEXICON_RES = tuple((p, _phrase_re(p), w, s, c) for p, w, s, c in EMERGENCY_LEXICON)
_HAZARD_RES = {
    hazard: tuple(_phrase_re(p) for p in phrases)
    for hazard, phrases in HAZARD_LEXICON.items()
}


def extract_hazards(text: str) -> list[str]:
    """Hazard tags present in the text (IDP `hazards` array order-stable)."""
    norm = normalize_text(text)
    return [h for h, res in _HAZARD_RES.items() if any(r.search(norm) for r in res)]


def classify_emergency(text: str) -> dict[str, Any]:
    """Score a user turn for emergency intent.

    Returns {"score": float in [0,1], "severity": critical|high|medium|low|None,
    "incident_type": <IDP enum>, "hazards": [...], "matches": [phrases]}.
    score == 0.0 (and severity None) means "no emergency signal".
    """
    norm = normalize_text(text)
    score = 0.0
    best_severity: str | None = None
    best_category: tuple[float, int, str] | None = None  # (weight, -index, category)
    matches: list[str] = []
    for index, (phrase, rx, weight, severity, category) in enumerate(_LEXICON_RES):
        if not rx.search(norm):
            continue
        matches.append(phrase)
        score += weight
        if best_severity is None or _SEVERITY_RANK[severity] > _SEVERITY_RANK[best_severity]:
            best_severity = severity
        if category is not None and (
            best_category is None or (weight, -index) > best_category[:2]
        ):
            best_category = (weight, -index, category)
    score = min(1.0, round(score, 4))
    if not matches:
        return {
            "score": 0.0,
            "severity": None,
            "incident_type": "other",
            "hazards": [],
            "matches": [],
        }
    return {
        "score": score,
        "severity": best_severity,
        "incident_type": best_category[2] if best_category else "other",
        "hazards": extract_hazards(text),
        "matches": matches,
    }


def is_emergency(text: str, min_score: float) -> tuple[bool, dict[str, Any]]:
    """Threshold gate: (emergency?, classification)."""
    result = classify_emergency(text)
    return result["score"] >= min_score, result


# ---------------------------------------------------------------------------
# IDP assembly
# ---------------------------------------------------------------------------

# conversation.channel (DB CHECK: voice|chat|phone|api|ussd — SPEC-W12)
# -> IDP channel enum.
_CHANNEL_MAP = {
    "voice": "voice",
    "phone": "voice",
    "chat": "web",
    "api": "webhook",
    # SPEC-W12 contract §2: the classifier treats ussd as web-like.
    "ussd": "web",
}

_NARRATIVE_MAX = 500


def incident_uuid(conversation_id: uuid.UUID, turn_id: uuid.UUID) -> uuid.UUID:
    """Deterministic incident id from the dedupe key conversation_id+turn_id.

    Retries/replays of the same turn therefore produce the SAME incident_id,
    and the CloudEvent id (= incident_id) dedupes downstream.
    """
    return uuid.uuid5(
        uuid.NAMESPACE_URL,
        f"opendesk:incident:{conversation_id}:{turn_id}",
    )


def map_channel(conversation_channel: str | None) -> str:
    return _CHANNEL_MAP.get(conversation_channel or "", "voice")


def build_idp(
    *,
    tenant_id: uuid.UUID,
    conversation_id: uuid.UUID,
    turn_id: uuid.UUID,
    text: str,
    channel: str,
    classification: dict[str, Any],
    reference_number: str,
    captured_at: datetime,
    callback_number: str | None = None,
    location: dict[str, Any] | None = None,
    contact_id: uuid.UUID | str | None = None,
) -> dict[str, Any]:
    """Assemble the canonical IDP (docs/schemas/incident-data-packet.json).

    location stays null here: conversation-service has no contact-location
    store (contact geo lives in booking-service, Wave-8); callers that have a
    location (e.g. widget client_location) may pass one in.
    """
    ts = captured_at
    if ts.tzinfo is None:
        ts = ts.replace(tzinfo=UTC)
    return {
        "incident_id": str(incident_uuid(conversation_id, turn_id)),
        "schema_version": SCHEMA_VERSION,
        "tenant_id": str(tenant_id),
        "captured_at": ts.isoformat(),
        "channel": map_channel(channel),
        "location": location,
        "callback_number": callback_number,
        "incident_type": classification["incident_type"],
        "severity": classification["severity"],
        "people_involved": 0,
        "hazards": classification["hazards"],
        "narrative_summary": text[:_NARRATIVE_MAX],
        "reference_number": reference_number,
        "contact_id": str(contact_id) if contact_id else None,
        "conversation_id": str(conversation_id),
    }


def idp_created_event(idp: dict[str, Any], *, subject: str = "") -> dict[str, Any]:
    """CloudEvent envelope; id = incident_id so brokers/consumers dedupe."""
    return {
        "specversion": "1.0",
        "id": idp["incident_id"],
        "source": EVENT_SOURCE,
        "type": EVENT_TYPE_IDP_CREATED,
        "subject": subject or idp["reference_number"],
        "time": datetime.now(UTC).isoformat(),
        "tenantid": idp["tenant_id"],
        "data": idp,
    }


# ---------------------------------------------------------------------------
# Reference number: INC-{YYYY}-{seq:06d} per tenant per year (Postgres counter;
# conversation-service has no Redis). SPEC-W43 Y-06: incident_counters is
# created by bootstrap DDL at service boot (Database.ensure_relay_tables)
# with fail-closed RLS + FORCE — NOT lazily inside the tenant-scoped tx.
# ---------------------------------------------------------------------------

_COUNTER_UPSERT = """
INSERT INTO incident_counters (tenant_id, year, seq)
VALUES ($1, $2, 1)
ON CONFLICT (tenant_id, year)
DO UPDATE SET seq = incident_counters.seq + 1
RETURNING seq
"""


async def next_reference_number(
    db: Any, tenant_id: uuid.UUID, *, now: datetime | None = None
) -> str:
    """Next tenant-facing reference number (atomic per tenant+year upsert)."""
    year = (now or datetime.now(UTC)).year
    async with db._tenant_tx(tenant_id) as conn:
        seq = await conn.fetchval(_COUNTER_UPSERT, tenant_id, year)
    return f"INC-{year}-{int(seq):06d}"


# ---------------------------------------------------------------------------
# Emission (non-blocking, DURABLY deduplicated, failures logged never raised)
#
# SPEC-W43 Y-03: the dedupe gate lives in Postgres (incident_emitted, PK
# (tenant_id, dedupe_key)) written in the SAME tenant transaction as the
# incident_counters reference upsert (Database.incident_emit_record). A Dapr
# publish failure leaves the row unpublished; the IncidentRetryWorker below
# republishes unsent rows — a crashed/failed publish is NEVER silent. The
# in-process _emitted_keys set is only a fast-path cache over the durable
# state (durable state wins on restart).
# ---------------------------------------------------------------------------

_emitted_keys: set[str] = set()


def _reset_dedupe() -> None:
    """Test hook: clear the in-process emission dedupe set."""
    _emitted_keys.clear()


async def emit_for_turn(
    *,
    cfg: Config,
    db: Any,
    dapr: Any,
    tenant_id: uuid.UUID,
    conversation_id: uuid.UUID,
    turn_id: uuid.UUID,
    text: str,
    channel: str,
    site_slug: str = "",
    contact_phone: str | None = None,
    captured_at: datetime | None = None,
) -> dict[str, Any] | None:
    """Classify a persisted user turn and, on threshold, build + emit the IDP.

    Returns the emitted IDP (or None when below threshold / already emitted /
    on failure). NEVER raises — incident detection must not break ingestion.
    """
    try:
        hit, classification = is_emergency(text, cfg.incident_min_score)
        if not hit:
            return None
        dedupe_key = f"{conversation_id}:{turn_id}"
        if dedupe_key in _emitted_keys:
            log.info(
                "incident already emitted for turn; skipping",
                conversation_id=str(conversation_id),
                turn_id=str(turn_id),
            )
            return None

        holder: dict[str, Any] = {}

        def _build(reference: str) -> dict[str, Any]:
            idp = build_idp(
                tenant_id=tenant_id,
                conversation_id=conversation_id,
                turn_id=turn_id,
                text=text,
                channel=channel,
                classification=classification,
                reference_number=reference,
                captured_at=captured_at or datetime.now(UTC),
                callback_number=contact_phone,
            )
            event = idp_created_event(idp, subject=site_slug)
            holder["event"] = event
            return event

        # Same-tx counter upsert + durable dedupe row (SPEC-W43 Y-03).
        event, state = await db.incident_emit_record(
            tenant_id, dedupe_key, _build
        )
        if state == "duplicate":
            _emitted_keys.add(dedupe_key)
            log.info(
                "incident already emitted for turn; skipping",
                conversation_id=str(conversation_id),
                turn_id=str(turn_id),
            )
            return None
        if state == "created":
            event = holder["event"]
        # state == "retry": republish the STORED event (reference number and
        # incident id are the original ones — no counter burn, no drift).

        # Publish failure propagates to the outer handler: the durable row
        # stays unpublished and the retry worker republishes it later.
        await dapr.publish_event(cfg.incidents_topic, event)
        await db.incident_mark_published(tenant_id, dedupe_key)
        _emitted_keys.add(dedupe_key)
        idp = event["data"]
        log.info(
            "incident IDP emitted",
            incident_id=idp["incident_id"],
            reference=idp["reference_number"],
            severity=idp["severity"],
            incident_type=idp["incident_type"],
            score=classification["score"],
            conversation_id=str(conversation_id),
            retry=(state == "retry"),
        )
        return idp
    except Exception as exc:  # noqa: BLE001 - never break turn ingestion
        log.error(
            "incident IDP emission failed (durable row kept for retry)",
            error=str(exc),
            conversation_id=str(conversation_id),
            turn_id=str(turn_id),
        )
        return None


class IncidentRetryWorker:
    """Background loop republishing incident_emitted rows whose Dapr publish
    failed (published_at IS NULL). SPEC-W43 Y-03: publish failure leaves
    durable state; this worker makes the retry loud and automatic instead of
    silently losing the incident.

    Mirrors RetentionSweeper: start()/stop() lifecycle, run_once() is the
    testable unit, the loop never dies on a cycle failure.
    """

    def __init__(self, cfg: Config, db: Any, dapr: Any) -> None:
        self._cfg = cfg
        self._db = db
        self._dapr = dapr
        self._task: asyncio.Task | None = None

    def start(self) -> None:
        self._task = asyncio.create_task(self._run(), name="incident-retry")
        log.info(
            "incident retry worker started",
            interval_seconds=self._cfg.incident_retry_seconds,
        )

    async def stop(self) -> None:
        if self._task is not None:
            self._task.cancel()
            try:
                await self._task
            except (asyncio.CancelledError, Exception):  # noqa: BLE001
                pass

    async def _run(self) -> None:
        while True:
            try:
                await self.run_once()
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # noqa: BLE001 — worker must not die
                log.error("incident retry sweep failed; will retry next cycle",
                          error=str(exc))
            await asyncio.sleep(self._cfg.incident_retry_seconds)

    async def run_once(self) -> int:
        """Republish every unsent incident row; returns the count published."""
        unsent = await self._db.incident_unsent()
        published = 0
        for tenant_id, dedupe_key, payload in unsent:
            try:
                await self._dapr.publish_event(self._cfg.incidents_topic, payload)
                await self._db.incident_mark_published(tenant_id, dedupe_key)
                published += 1
                log.info(
                    "incident IDP republished after earlier failure",
                    tenant_id=str(tenant_id),
                    dedupe_key=dedupe_key,
                )
            except Exception as exc:  # noqa: BLE001 — keep sweeping others
                log.error(
                    "incident IDP republish failed; row stays unsent",
                    tenant_id=str(tenant_id),
                    dedupe_key=dedupe_key,
                    error=str(exc),
                )
        return published
