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


def cloud_event(
    event_type: str,
    *,
    subject: str,
    tenant_id: str,
    data: dict[str, Any],
) -> dict[str, Any]:
    """CloudEvents 1.0 envelope per SPEC §4."""
    return {
        "specversion": "1.0",
        "id": str(uuid.uuid4()),
        "source": SOURCE,
        "type": event_type,
        "subject": subject,
        "time": datetime.now(UTC).isoformat(),
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
