"""CloudEvents 1.0 envelopes for ocr-service (SPEC-W53 Scope A).

Two streams, both via Dapr pubsub (pubsub-kafka):
  * topic opendesk.usage.events — com.opendesk.usage.UsageRecord
    {tenant_id, metric, value, ts, meta} (bookingops/usage.go contract;
    metric "ocr_page", analytics-pipeline consumes with zero changes).
  * topic opendesk.ocr.events  — com.opendesk.ocr.DocumentExtracted.
  * topic opendesk.ops.alerts  — com.opendesk.ocr.AvScanAlert (SPEC-W59 P5:
    infected uploads / fail-open scan skips; severity critical/warning).
"""

from __future__ import annotations

import uuid
from datetime import UTC, datetime
from typing import Any

from .config import is_kyc_kind

SOURCE = "ocr-service"

TYPE_USAGE_RECORD = "com.opendesk.usage.UsageRecord"
TYPE_DOCUMENT_EXTRACTED = "com.opendesk.ocr.DocumentExtracted"

# SPEC-W58 P3: inbound topics consumed via Dapr programmatic subscriptions.
TYPE_CONSENT_ERASURE = "com.opendesk.consent.ErasureRequested"
TOPIC_CONSENT_ERASURE = "opendesk.consent.erasure.v1"
TYPE_TENANT_DELETED = "com.opendesk.identity.TenantDeleted"
TOPIC_IDENTITY_EVENTS = "opendesk.identity.events"

# SPEC-W59 P5/P2: outbound ops alerts (notification-worker opsalerts
# consumer persists every alert; NOT opendesk.usage.events, NOT
# opendesk.ocr.events — an infected upload consumed no extraction and no
# document exists).
TYPE_AV_ALERT = "com.opendesk.ocr.AvScanAlert"
TOPIC_OPS_ALERTS = "opendesk.ops.alerts"

# SPEC-W67 (G5/G6, PINNED A<->B contract): cross-tenant exact-duplicate
# match. Emitted on opendesk.ocr.events ONLY for the LATER tenant when its
# upload joins a registry key that already has >=1 OTHER tenant; earlier
# tenants are NEVER alerted. graph-sync projects seen_elsewhere_count onto
# the later tenant's OWN Document node; D9's tenant-bound sweep raises S4.
TYPE_CROSS_TENANT_MATCH = "com.opendesk.ocr.CrossTenantDocumentMatch"


def cloud_event(
    event_type: str,
    *,
    subject: str,
    tenant_id: str,
    data: dict[str, Any],
    event_time: datetime | str | None = None,
    event_id: uuid.UUID | str | None = None,
) -> dict[str, Any]:
    """CloudEvents 1.0 envelope per SPEC §4.

    SPEC-W66 WS1-A (additive): optional `event_time` / `event_id` exist ONLY
    for the backfill reconciler — `event_time` stamps the HISTORICAL
    document created_at (graph stub created_at = event time; time=now would
    corrupt gold day partitions) and `event_id` pins the deterministic
    uuid5 id (gold dedupes by event_id only). Both default to the legacy
    behavior, so every pre-W66 caller is byte-identical.
    """
    if event_time is None:
        ts = datetime.now(UTC).isoformat()
    elif isinstance(event_time, datetime):
        ts = event_time.isoformat()
    else:
        ts = str(event_time)
    return {
        "specversion": "1.0",
        "id": str(event_id) if event_id is not None else str(uuid.uuid4()),
        "source": SOURCE,
        "type": event_type,
        "subject": subject,
        "time": ts,
        "tenantid": tenant_id,
        "data": data,
    }


def usage_record_event(
    *,
    tenant_id: str,
    document_id: uuid.UUID,
    page_count: int,
    model_version: str,
    kind: str,
) -> dict[str, Any]:
    """ocr_page UsageRecord (meter every page — RECON §1-F/§4)."""
    return cloud_event(
        TYPE_USAGE_RECORD,
        subject=tenant_id,
        tenant_id=tenant_id,
        data={
            "tenant_id": tenant_id,
            "metric": "ocr_page",
            "value": page_count,
            "ts": datetime.now(UTC).isoformat(),
            "meta": {
                "document_id": str(document_id),
                "service": SOURCE,
                "model_version": model_version,
                "kind": kind,
            },
        },
    )


def document_extracted_event(
    *,
    tenant_id: str,
    document_id: uuid.UUID,
    kind: str,
    status: str,
    page_count: int,
    sha256: str,
    phash: str | None,
    model_version: str,
    storage_bucket: str,
    storage_key: str,
    extracted_fields: dict[str, Any],
    mime: str,
) -> dict[str, Any]:
    """com.opendesk.ocr.DocumentExtracted (opendesk.ocr.events).

    Carries hashes + storage pointers (fraud D9 duplicate-detection prep);
    NEVER carries document bytes or full extracted text.

    SPEC-W58 GATE R1 (P2): for any `kyc_*` kind, `extracted_fields` is
    emitted as {} — KYC PII (names, DOB, ID numbers) NEVER rides the bus;
    downstream consumers (graph-sync hash-only stub, fraud D9) only need
    the hashes/pointers, which are retained.

    SPEC-W64: `mime` rides the payload (the validated content type from
    validate_upload — e.g. image/png, application/pdf). mime is NOT PII
    (it describes the file format, never the subject), so the kyc
    redaction posture is unchanged; consumers that predate the key ignore
    it (graph-sync's OCRDocumentData silently ignores unknown JSON keys;
    the W62 bronze mapper's receipt_documents columns are untouched).
    """
    return cloud_event(
        TYPE_DOCUMENT_EXTRACTED,
        subject=str(document_id),
        tenant_id=tenant_id,
        data={
            "document_id": str(document_id),
            "tenant_id": tenant_id,
            "kind": kind,
            "status": status,
            "page_count": page_count,
            "sha256": sha256,
            "phash": phash,
            "model_version": model_version,
            "storage_bucket": storage_bucket,
            "storage_key": storage_key,
            "mime": mime,
            "extracted_fields": {} if is_kyc_kind(kind) else extracted_fields,
        },
    )


# SPEC-W66 WS1-A: the ONLY three receipt scalars a backfilled event may
# carry (bronze map_ocr_document whitelist, mapping.py:475-477). Stricter
# than the live path: EVERY non-receipt kind emits {} (kyc redaction
# included), and receipt rows emit exactly this subset — nothing else.
BACKFILL_RECEIPT_FIELD_WHITELIST = ("amount_minor", "currency", "receipt_date")


def backfill_event_id(document_id: uuid.UUID | str) -> uuid.UUID:
    """Deterministic backfill event id (SPEC-W66 WS1-A, HARD REQUIREMENT).

    uuid5(URL namespace, "ocr-backfill:{document_id}"). Idempotency anchor:
    gold.receipt_ingest_daily dedupes partitionBy("event_id") only
    (receipt_recon_gold.py:437-457) and the bronze sink dedupes event_id
    only within a batch — a re-run MUST re-emit the SAME id or gold
    double-counts documents/pages/receipts.
    """
    return uuid.uuid5(uuid.NAMESPACE_URL, f"ocr-backfill:{document_id}")


def document_backfill_event(
    *,
    tenant_id: str,
    document_id: uuid.UUID | str,
    kind: str,
    status: str,
    page_count: int,
    sha256: str,
    phash: str | None,
    model_version: str,
    created_at: datetime | str,
    extracted_fields: dict[str, Any] | str | None = None,
) -> dict[str, Any]:
    """com.opendesk.ocr.DocumentExtracted re-emission for the D9 backfill
    reconciler (SPEC-W66 WS1-A; published on opendesk.ocr.events so
    graph-sync projects the (:Document) stub and bronze maps receipts
    exactly like the live path).

    Hash-only pinned set (PII minimization, G2): document_id, tenant_id,
    kind, status, page_count, sha256, phash, model_version + the whitelisted
    receipt scalars. NO storage_bucket/storage_key/mime/extracted_text.
    `time` is the HISTORICAL document created_at and `id` is the
    deterministic backfill_event_id — see cloud_event() for why.
    """
    fields: dict[str, Any] = {}
    if isinstance(extracted_fields, str):
        # asyncpg returns jsonb as text when no codec is registered.
        import json as _json

        try:
            extracted_fields = _json.loads(extracted_fields)
        except ValueError:
            extracted_fields = None
    if kind == "receipt" and isinstance(extracted_fields, dict):
        fields = {
            k: extracted_fields[k]
            for k in BACKFILL_RECEIPT_FIELD_WHITELIST
            if k in extracted_fields
        }
    return cloud_event(
        TYPE_DOCUMENT_EXTRACTED,
        subject=str(document_id),
        tenant_id=tenant_id,
        event_id=backfill_event_id(document_id),
        event_time=created_at,
        data={
            "document_id": str(document_id),
            "tenant_id": tenant_id,
            "kind": kind,
            "status": status,
            "page_count": page_count,
            "sha256": sha256,
            "phash": phash,
            "model_version": model_version,
            "extracted_fields": fields,
        },
    )


def cross_tenant_match_event(
    *,
    tenant_id: str,
    document_id: uuid.UUID | str,
    kind: str,
    sha256: str,
    tenant_count: int,
) -> dict[str, Any]:
    """SPEC-W67 PINNED contract — com.opendesk.ocr.CrossTenantDocumentMatch
    (opendesk.ocr.events), emitted ONLY to the LATER tenant when its
    membership add joins a registry key with >=1 OTHER tenant.

    CloudEvents tenantid is the LATER tenant's id; subject is the LATER
    tenant's OWN document_id (document-event precedent — DocumentExtracted
    live :144 and backfill :223 use subject=str(document_id); only the
    tenant-scoped UsageRecord/AV events use subject=tenant_id). data is
    EXACTLY {document_id (the later tenant's OWN), sha256 (its OWN), kind,
    tenant_count} — own identifiers plus ONE coarse integer (count, never
    identity). NO other tenant ids, NO other document ids, NO other
    tenants' timestamps, NO perceptual hash (G2/G5 zero-cross-tenant-
    disclosure).
    """
    return cloud_event(
        TYPE_CROSS_TENANT_MATCH,
        subject=str(document_id),
        tenant_id=tenant_id,
        data={
            "document_id": str(document_id),
            "sha256": sha256,
            "kind": kind,
            "tenant_count": int(tenant_count),
        },
    )


def av_alert_event(
    *,
    tenant_id: str,
    kind: str,
    sha256: str,
    signature: str,
) -> dict[str, Any]:
    """SPEC-W59 P5: infected upload -> opendesk.ops.alerts, severity critical.

    Payload keys are EXACTLY severity/tenant_id/kind/sha256/signature —
    never document bytes, filename, or PII (PII logging ban, kyc
    documents.go idiom).
    """
    return cloud_event(
        TYPE_AV_ALERT,
        subject=tenant_id,
        tenant_id=tenant_id,
        data={
            "severity": "critical",
            "tenant_id": tenant_id,
            "kind": kind,
            "sha256": sha256,
            "signature": signature,
        },
    )


def av_scan_skipped_event(
    *,
    tenant_id: str,
    kind: str,
    sha256: str,
    reason: str,
) -> dict[str, Any]:
    """SPEC-W59 P2/R4: a fail-open skip (clamd down + CLAMAV_REQUIRED=false)
    is never silent — metric + this ops alert (warning severity; an
    infected scan is the critical one). Same no-PII payload discipline."""
    return cloud_event(
        TYPE_AV_ALERT,
        subject=tenant_id,
        tenant_id=tenant_id,
        data={
            "severity": "warning",
            "tenant_id": tenant_id,
            "kind": kind,
            "sha256": sha256,
            "reason": reason,
        },
    )
