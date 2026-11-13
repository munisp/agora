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

# SPEC-W62 §bronze.receipt_documents (G1): one row per
# com.opendesk.ocr.DocumentExtracted on opendesk.ocr.events with
# kind='receipt' ONLY. kyc_* kinds (and every other non-receipt kind) are
# DROPPED ENTIRELY — a hash-only kyc row in the lake would still be a
# processing record with no TTL mechanism (RECON Q4). From extracted_fields
# EXACTLY three keys are whitelisted — amount_minor, currency, receipt_date;
# vendor/note/items/raw text NEVER land (PII posture: hash-only + scalars).
# Storage pointers (storage_bucket/storage_key) are intentionally NOT
# columns: the lake holds hashes + ids, not object locations.
RECEIPT_DOCUMENT_COLUMNS = (
    "event_id",
    "tenant_id",
    "document_id",
    "kind",
    "status",
    "page_count",
    "sha256",
    "phash",
    "model_version",
    "amount_minor",
    "currency",
    "receipt_date",
    "occurred_at",
)

# SPEC-W62 §bronze.receipt_recon_findings (G2): com.opendesk.booking.
# ReceiptReconFinding rides opendesk.booking.events (booking-service
# internal/recon/recon.go findingEventPayload) but is NOT a booking event —
# it gets its own bronze table with the full finding payload. booking_id is
# taken from data ONLY (never the CloudEvent subject: that subject is the
# TENANT SLUG — stuffing it into booking_id was the pre-existing G2 break).
RECEIPT_RECON_FINDING_COLUMNS = (
    "event_id",
    "tenant_id",
    "run_id",
    "finding_kind",
    "confidence",
    "capture_client_id",
    "booking_id",
    "attempt_tx_ref",
    "expected_amount_minor",
    "captured_amount_minor",
    "receipt_amount_major",
    "receipt_currency",
    "occurred_at",
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


_ISO3_RE = re.compile(r"^[A-Z]{3}$")
_RECEIPT_DATE_RE = re.compile(r"^\d{4}-\d{2}-\d{2}$")


def _as_iso3(value: Any) -> Optional[str]:
    """ISO-4217 alpha-3 currency code, normalised upper-case; else None."""
    if not isinstance(value, str):
        return None
    code = value.strip().upper()
    return code if _ISO3_RE.match(code) else None


def _as_receipt_date(value: Any) -> Optional[str]:
    """Strict YYYY-MM-DD calendar date string; anything else -> None."""
    if not isinstance(value, str):
        return None
    text = value.strip()
    if not _RECEIPT_DATE_RE.match(text):
        return None
    try:
        datetime.strptime(text, "%Y-%m-%d")
    except ValueError:
        return None
    return text


# SPEC-W56 / SPEC-W62 G2: ReceiptReconFinding events share the
# opendesk.booking.events topic but are NOT booking events.
RECEIPT_RECON_FINDING_TYPE = "ReceiptReconFinding"


def is_receipt_recon_finding(message: Mapping[str, Any]) -> bool:
    """True for ReceiptReconFinding-typed messages (CloudEvent ``type`` or a
    bare payload's event_type), matched case-insensitively on the short name."""
    envelope, data = split_envelope(message)
    raw = envelope.get("type") or _get(data, "event_type", "eventType")
    short = short_event_type(raw if isinstance(raw, str) else None)
    return (short or "").lower() == RECEIPT_RECON_FINDING_TYPE.lower()


def map_booking_event(message: Mapping[str, Any]) -> Optional[dict[str, Any]]:
    # SPEC-W62 G2: ReceiptReconFinding-typed messages never land in
    # bronze.booking_events (they used to — sparsely — breaking the dbt
    # accepted_values test on stg_booking_events.event_type and stuffing the
    # tenant-slug subject into booking_id). They are routed to
    # bronze.receipt_recon_findings by route_booking_event; here they are
    # SKIPPED entirely.
    if is_receipt_recon_finding(message):
        return None
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


def map_ocr_document(message: Mapping[str, Any]) -> Optional[dict[str, Any]]:
    """com.opendesk.ocr.DocumentExtracted (opendesk.ocr.events)
    -> bronze.receipt_documents.

    SPEC-W62 G1 (PII exclusion): ONLY kind='receipt' documents land. kyc_*
    kinds are DROPPED ENTIRELY (belt-and-braces — W58 R1 already emits
    extracted_fields={} for them, and a hash-only kyc row in the TTL-less
    lake is still a processing record); every other non-receipt kind
    (document, knowledge_scan, ...) is dropped too. Returns None for dropped
    messages (the consumer skips None).

    From ``extracted_fields`` EXACTLY three whitelisted scalars are read —
    amount_minor (int), currency (ISO-3), receipt_date (YYYY-MM-DD). No
    vendor, no note, no items, no free text, no other key is ever touched.
    """
    envelope, data = split_envelope(message)
    kind = _as_str(_get(data, "kind"))
    if kind != "receipt":
        return None
    fields = _get(data, "extracted_fields", "extractedFields")
    if not isinstance(fields, Mapping):
        fields = {}
    return {
        "event_id": _as_str(envelope.get("id") or _get(data, "event_id", "eventId")),
        "tenant_id": _as_str(
            envelope.get("tenantid") or _get(data, "tenant_id", "tenantId")
        ),
        "document_id": _as_str(
            _get(data, "document_id", "documentId") or envelope.get("subject")
        ),
        "kind": kind,
        "status": _as_str(_get(data, "status")),
        "page_count": _as_int(_get(data, "page_count", "pageCount")),
        "sha256": _as_str(_get(data, "sha256")),
        "phash": _as_str(_get(data, "phash")),
        "model_version": _as_str(_get(data, "model_version", "modelVersion")),
        # G1 whitelist — the ONLY keys read out of extracted_fields.
        "amount_minor": _as_int(_get(fields, "amount_minor", "amountMinor")),
        "currency": _as_iso3(_get(fields, "currency")),
        "receipt_date": _as_receipt_date(_get(fields, "receipt_date", "receiptDate")),
        "occurred_at": parse_ts(envelope.get("time"))
        or parse_ts(_get(data, "occurred_at", "occurredAt")),
    }


def map_receipt_recon_finding(message: Mapping[str, Any]) -> dict[str, Any]:
    """com.opendesk.booking.ReceiptReconFinding -> bronze.receipt_recon_findings
    (SPEC-W62 G2; full finding payload per SPEC §bronze.receipt_recon_findings).

    Emitted by booking-service internal/recon (recon.go findingEventPayload):
    data = {tenant_id, kind, confidence, capture_client_id?, booking_id?,
    attempt_tx_ref?, expected_amount_minor?, captured_amount_minor?,
    receipt_amount_major?, detail, finding_id}. ``run_id`` is on the finding
    row but not in today's event payload (mapped when present, else NULL);
    ``receipt_currency`` rides inside ``detail`` today (fallback lookup).
    booking_id comes from data ONLY — the CloudEvent subject is the tenant
    slug, never a booking id (the G2 stuffing bug).
    """
    envelope, data = split_envelope(message)
    detail = data.get("detail")
    if not isinstance(detail, Mapping):
        detail = {}
    return {
        "event_id": _as_str(
            envelope.get("id")
            or _get(data, "event_id", "eventId")
            or _get(data, "finding_id", "findingId")
        ),
        "tenant_id": _as_str(
            envelope.get("tenantid") or _get(data, "tenant_id", "tenantId")
        ),
        "run_id": _as_str(_get(data, "run_id", "runId")),
        "finding_kind": _as_str(_get(data, "kind", "finding_kind", "findingKind")),
        "confidence": _as_str(_get(data, "confidence")),
        "capture_client_id": _as_str(_get(data, "capture_client_id", "captureClientId")),
        # NO envelope-subject fallback: subject is the tenant slug (G2).
        "booking_id": _as_str(_get(data, "booking_id", "bookingId")),
        "attempt_tx_ref": _as_str(_get(data, "attempt_tx_ref", "attemptTxRef")),
        "expected_amount_minor": _as_int(
            _get(data, "expected_amount_minor", "expectedAmountMinor")
        ),
        "captured_amount_minor": _as_int(
            _get(data, "captured_amount_minor", "capturedAmountMinor")
        ),
        "receipt_amount_major": _as_float(
            _get(data, "receipt_amount_major", "receiptAmountMajor")
        ),
        "receipt_currency": _as_str(_get(data, "receipt_currency", "receiptCurrency"))
        or _as_str(_get(detail, "receipt_currency", "receiptCurrency")),
        "occurred_at": parse_ts(envelope.get("time"))
        or parse_ts(_get(data, "occurred_at", "occurredAt")),
    }


def route_booking_event(message: Mapping[str, Any]) -> Optional[tuple[str, dict[str, Any]]]:
    """Router for opendesk.booking.events (SPEC-W62 G2).

    Returns (bronze_table, row) or None to drop. ReceiptReconFinding-typed
    messages route to receipt_recon_findings; everything else maps as a
    booking event. (map_booking_event ALSO skips findings on its own, so a
    finding can never reach bronze.booking_events even if this router is
    bypassed.)
    """
    if is_receipt_recon_finding(message):
        return ("receipt_recon_findings", map_receipt_recon_finding(message))
    row = map_booking_event(message)
    if row is None:
        return None
    return ("booking_events", row)
