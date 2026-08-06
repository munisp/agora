"""Pure-python flattening of Kafka payloads into bronze table rows.

Column lists below are the CONTRACT with the dbt project
(infra/lakehouse/dbt/models/silver/schema.yml, sources.bronze) — do not rename
or reorder without changing both sides.

Input payloads (SPEC §4):
- CloudEvents 1.0 envelope everywhere: {specversion, id, source, type, subject,
  time, tenantid (ext), data}. ``type`` may be a short name ("BookingCreated")
  or a reverse-DNS name ("com.opendesk.booking.BookingCreated") — we keep the
  last segment so dbt's lower(event_type) comparisons match.
- ``opendesk.conversation.transcripts`` carries ConversationTurn
  {conversationId, tenantId, role, text, ts, audioUrl?} as the CE ``data``
  payload; bare (non-enveloped) turn messages are tolerated for the raw
  Fluvio-fed path.

All timestamps are normalised to *naive UTC* datetimes (Iceberg ``timestamp``
without timezone — consistent with the Spark silver jobs, which run with
``spark.sql.iceberg.handle-timestamp-without-timezone=true``).
"""

from __future__ import annotations

import json
import re
from datetime import datetime, timezone
from typing import Any, Mapping, Optional

# --- canonical bronze columns (order = pyiceberg/pyarrow schema order) --------
BOOKING_EVENT_COLUMNS = (
    "event_id",
    "event_type",
    "tenant_id",
    "booking_id",
    "status",
    "source",
    "price_cents",
    "currency",
    "starts_at",
    "occurred_at",
    # SPEC-W3 §3 innovation 9: revenue intelligence needs per-offering
    # granularity; appended at the END to keep pyiceberg field ids stable.
    "offering_id",
)

PAYMENT_EVENT_COLUMNS = (
    "event_id",
    "event_type",
    "tenant_id",
    "booking_id",
    "amount_cents",
    "currency",
    "transfer_code",
    "ledger_ref",
    "occurred_at",
)

TRANSCRIPT_COLUMNS = (
    "conversation_id",
    "tenant_id",
    "role",
    "text",
    "ts",
    "audio_url",
)

# STRATEGY §3, Wave 5 #9: one row per usage record emitted on
# opendesk.usage.events ({tenant_id, metric, value, ts, meta}). `value` is a
# double (call-minutes are fractional); `meta` is the raw JSON of the free-form
# meta object. occurred_at comes from the payload `ts` (fallback: CE time).
USAGE_EVENT_COLUMNS = (
    "event_id",
    "tenant_id",
    "metric",
    "value",
    "occurred_at",
    "meta",
)

# SPEC-W33 §2 A2: one row per CAC FunnelEvent emitted on ``cac.events``
# (CloudEvent type ``com.opendesk.cac.FunnelEvent``, SPEC-W13 §2) with the
# ``data`` payload flattened — the input contract documented in
# infra/lakehouse/spark/jobs/cac_analytics.py (bronze.cac_events). The bronze
# sink is deliberately tolerant (it keeps the raw copy; strict contract
# enforcement — event_name enum, mandatory fields — lives in the CAC rollup
# consumer, cac_events.py, and the Spark gold job). amount_ngn is a double
# and lga_id a long, matching the Spark contract. event_ts falls back to the
# CloudEvent ``time`` like the other mappers.
CAC_EVENT_COLUMNS = (
    "event_id",
    "tenant_id",
    "entity_type",
    "entity_id",
    "event_name",
    "event_ts",
    "channel",
    "campaign_id",
    "lga_id",
    "amount_ngn",
    "idempotency_key",
)

_CAMEL_RE = re.compile(r"(?<!^)(?=[A-Z])")


def _snake(name: str) -> str:
    return _CAMEL_RE.sub("_", name).lower()


def _get(data: Mapping[str, Any], *keys: str) -> Optional[Any]:
    """Fetch the first present key, tolerating camelCase/snake_case variants."""
    for key in keys:
        for variant in {key, _snake(key)}:
            if variant in data and data[variant] is not None:
                return data[variant]
    return None


def split_envelope(message: Mapping[str, Any]) -> tuple[Mapping[str, Any], Mapping[str, Any]]:
    """Return (envelope, data). Bare payloads yield an empty envelope."""
    if "specversion" in message:
        data = message.get("data")
        return message, data if isinstance(data, Mapping) else {}
    return {}, message


def short_event_type(raw_type: Optional[str]) -> Optional[str]:
    """'com.opendesk.booking.BookingCreated' -> 'BookingCreated'."""
    if not raw_type:
        return None
    return raw_type.rsplit(".", 1)[-1]


def parse_ts(value: Any) -> Optional[datetime]:
    """Parse ISO-8601 strings, epoch seconds, or epoch millis -> naive UTC."""
    if value is None:
        return None
    if isinstance(value, datetime):
        dt = value
    elif isinstance(value, (int, float)):
        # Heuristic: values beyond ~ year 3366 in seconds are millis.
        seconds = value / 1000.0 if value > 1e11 else float(value)
        dt = datetime.fromtimestamp(seconds, tz=timezone.utc)
    elif isinstance(value, str):
        text = value.strip()
        if text.endswith("Z"):
            text = text[:-1] + "+00:00"
        try:
            dt = datetime.fromisoformat(text)
        except ValueError:
            return None
    else:
        return None
    if dt.tzinfo is not None:
        dt = dt.astimezone(timezone.utc).replace(tzinfo=None)
    return dt.replace(tzinfo=None)


def _as_int(value: Any) -> Optional[int]:
    if value is None or isinstance(value, bool):
        return None
    try:
        return int(value)
    except (TypeError, ValueError):
        return None


def _as_str(value: Any) -> Optional[str]:
    if value is None:
        return None
    return value if isinstance(value, str) else str(value)


def _as_float(value: Any) -> Optional[float]:
    if value is None or isinstance(value, bool):
        return None
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def map_booking_event(message: Mapping[str, Any]) -> dict[str, Any]:
    envelope, data = split_envelope(message)
    return {
        "event_id": _as_str(envelope.get("id") or _get(data, "event_id", "eventId")),
        "event_type": _as_str(
            short_event_type(envelope.get("type")) or _get(data, "event_type", "eventType")
        ),
        "tenant_id": _as_str(
            envelope.get("tenantid") or _get(data, "tenant_id", "tenantId")
        ),
        "booking_id": _as_str(
            _get(data, "booking_id", "bookingId") or envelope.get("subject")
        ),
        "status": _as_str(_get(data, "status")),
        "source": _as_str(_get(data, "source", "channel")),
        "price_cents": _as_int(_get(data, "price_cents", "priceCents")),
        "currency": _as_str(_get(data, "currency")),
        "starts_at": parse_ts(_get(data, "starts_at", "startsAt")),
        "occurred_at": parse_ts(envelope.get("time"))
        or parse_ts(_get(data, "occurred_at", "occurredAt")),
        "offering_id": _as_str(_get(data, "offering_id", "offeringId")),
    }


def map_payment_event(message: Mapping[str, Any]) -> dict[str, Any]:
    envelope, data = split_envelope(message)
    return {
        "event_id": _as_str(envelope.get("id") or _get(data, "event_id", "eventId")),
        "event_type": _as_str(
            short_event_type(envelope.get("type")) or _get(data, "event_type", "eventType")
        ),
        "tenant_id": _as_str(
            envelope.get("tenantid") or _get(data, "tenant_id", "tenantId")
        ),
        "booking_id": _as_str(
            _get(data, "booking_id", "bookingId") or envelope.get("subject")
        ),
        "amount_cents": _as_int(_get(data, "amount_cents", "amountCents")),
        "currency": _as_str(_get(data, "currency")),
        "transfer_code": _as_int(_get(data, "transfer_code", "transferCode", "code")),
        "ledger_ref": _as_str(_get(data, "ledger_ref", "ledgerRef")),
        "occurred_at": parse_ts(envelope.get("time"))
        or parse_ts(_get(data, "occurred_at", "occurredAt")),
    }


def map_transcript(message: Mapping[str, Any]) -> dict[str, Any]:
    envelope, data = split_envelope(message)
    return {
        "conversation_id": _as_str(
            _get(data, "conversation_id", "conversationId") or envelope.get("subject")
        ),
        "tenant_id": _as_str(
            envelope.get("tenantid") or _get(data, "tenant_id", "tenantId")
        ),
        "role": _as_str(_get(data, "role")),
        "text": _as_str(_get(data, "text")),
        "ts": parse_ts(_get(data, "ts", "timestamp")) or parse_ts(envelope.get("time")),
        "audio_url": _as_str(_get(data, "audio_url", "audioUrl")),
    }


def map_usage_event(message: Mapping[str, Any]) -> dict[str, Any]:
    """Usage metering record (Wave 5 #9). Bare payloads of the shape
    {tenant_id, metric, value, ts, meta} are the contract; CloudEvent
    envelopes are tolerated. Sparse data is normal (v1 emits bookings only) —
    every field except tenant/metric/value may be null."""
    envelope, data = split_envelope(message)
    meta = _get(data, "meta", "metadata")
    return {
        "event_id": _as_str(envelope.get("id") or _get(data, "event_id", "eventId")),
        "tenant_id": _as_str(
            envelope.get("tenantid") or _get(data, "tenant_id", "tenantId")
        ),
        "metric": _as_str(_get(data, "metric")),
        "value": _as_float(_get(data, "value")),
        "occurred_at": parse_ts(_get(data, "ts", "timestamp", "occurred_at", "occurredAt"))
        or parse_ts(envelope.get("time")),
        "meta": json.dumps(meta, sort_keys=True) if meta is not None else None,
    }


def map_cac_event(message: Mapping[str, Any]) -> dict[str, Any]:
    """CAC FunnelEvent (SPEC-W13 §2 / SPEC-W33 §2 A2) -> bronze.cac_events.

    Bare payloads of the data shape are the contract; CloudEvent envelopes
    (type ``com.opendesk.cac.FunnelEvent``) are unwrapped via split_envelope
    like every other bronze mapper. All fields are nullable at this layer —
    bronze keeps the raw copy and the Spark gold job dedupes/filters
    (idempotency_key, event_name enum) downstream."""
    envelope, data = split_envelope(message)
    return {
        "event_id": _as_str(envelope.get("id") or _get(data, "event_id", "eventId")),
        "tenant_id": _as_str(
            envelope.get("tenantid") or _get(data, "tenant_id", "tenantId")
        ),
        "entity_type": _as_str(_get(data, "entity_type", "entityType")),
        "entity_id": _as_str(_get(data, "entity_id", "entityId")),
        "event_name": _as_str(_get(data, "event_name", "eventName")),
        "event_ts": parse_ts(_get(data, "event_ts", "eventTs"))
        or parse_ts(envelope.get("time")),
        "channel": _as_str(_get(data, "channel")),
        "campaign_id": _as_str(_get(data, "campaign_id", "campaignId")),
        "lga_id": _as_int(_get(data, "lga_id", "lgaId")),
        "amount_ngn": _as_float(_get(data, "amount_ngn", "amountNgn")),
        "idempotency_key": _as_str(_get(data, "idempotency_key", "idempotencyKey")),
    }
