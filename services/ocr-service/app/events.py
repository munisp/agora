"""CloudEvents 1.0 envelopes for ocr-service (SPEC-W53 Scope A).

Two streams, both via Dapr pubsub (pubsub-kafka):
  * topic opendesk.usage.events — com.opendesk.usage.UsageRecord
    {tenant_id, metric, value, ts, meta} (bookingops/usage.go contract;
    metric "ocr_page", analytics-pipeline consumes with zero changes).
  * topic opendesk.ocr.events  — com.opendesk.ocr.DocumentExtracted.
"""

from __future__ import annotations

import uuid
from datetime import UTC, datetime
from typing import Any

SOURCE = "ocr-service"

TYPE_USAGE_RECORD = "com.opendesk.usage.UsageRecord"
TYPE_DOCUMENT_EXTRACTED = "com.opendesk.ocr.DocumentExtracted"


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
) -> dict[str, Any]:
    """com.opendesk.ocr.DocumentExtracted (opendesk.ocr.events).

    Carries hashes + storage pointers (fraud D9 duplicate-detection prep);
    NEVER carries document bytes or full extracted text.
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
            "extracted_fields": extracted_fields,
        },
    )
