"""SPEC-W58 GATE R2 (P3): Dapr consumers for consent erasure + tenant delete.

Subscribed via the programmatic /dapr/subscribe route registered in
main.py:

  * com.opendesk.consent.ErasureRequested on opendesk.consent.erasure.v1
    (identity consent registry payload: {tenant_id, data_subject_id,
    purpose, erased_records, erasure_ts, synthetic}) — for matching kyc_*
    rows: blank extracted_fields/extracted_text, status='erased', DELETE
    the MinIO object; sha256/phash are RETAINED as the hash-only audit
    stub. Subject matching (SPEC-W65 G2, implemented in
    store.list_kyc_rows_matching_subject): the subject_phone COLUMN match
    is primary (data_subject_id IS the E.164 phone); rows ingested before
    the column existed (subject_phone IS NULL) fall back to the legacy
    exact top-level extracted_fields string match, and the 72h TTL
    sweeper bounds that fallback window. mark_erased also NULLs
    subject_phone — the tombstone must not retain the PII locator.

  * com.opendesk.identity.TenantDeleted on opendesk.identity.events
    (payload: {tenant_slug, tenant_id, deleted_at, actor}) — HARD-DELETE
    ALL of the tenant's ocr_documents rows (all kinds) + their MinIO
    objects.

Handler semantics mirror the Dapr pubsub contract: handlers raise -> the
route returns 500 and Dapr redelivers (handlers are idempotent, so
redelivery converges); malformed/unmatched events are DROPped (200).
"""

from __future__ import annotations

from typing import Any

from .logging import get_logger
from .sweeper import ObjectDeleter

log = get_logger(__name__)


def _event_data(event: Any) -> dict[str, Any] | None:
    if not isinstance(event, dict):
        return None
    data = event.get("data")
    return data if isinstance(data, dict) else None


async def handle_erasure_event(db: Any, deleter: ObjectDeleter, event: Any) -> dict[str, int]:
    """Consent ErasureRequested: blank matching kyc_* rows + delete objects.

    Returns {"matched": n, "erased": m}. Raises on transient failures so
    the Dapr route can 500 and redeliver; object-delete failures are logged
    and still tombstone the row (the object either is gone already or the
    TTL sweeper will reap it — the row MUST NOT keep the PII).
    """
    data = _event_data(event)
    if not data:
        log.warning("consent_erasure.malformed_event")
        return {"matched": 0, "erased": 0}
    tenant_id = data.get("tenant_id")
    subject = data.get("data_subject_id")
    if not isinstance(tenant_id, str) or not tenant_id or not isinstance(subject, str) or not subject:
        log.warning("consent_erasure.missing_fields")
        return {"matched": 0, "erased": 0}
    async with db.tenant_conn(tenant_id) as conn:
        rows = await db.list_kyc_rows_matching_subject(conn, subject)
    erased = 0
    for row in rows:
        try:
            await deleter.delete(row["storage_key"])
        except Exception as exc:
            log.error(
                "consent_erasure.object_delete_failed",
                document_id=str(row["id"]),
                error=str(exc),
            )
        async with db.tenant_conn(tenant_id) as conn:
            await db.mark_erased(conn, row["id"])
        erased += 1
    log.info(
        "consent_erasure.applied",
        tenant_id=tenant_id,
        matched=len(rows),
        erased=erased,
    )
    return {"matched": len(rows), "erased": erased}


async def handle_tenant_deleted(db: Any, deleter: ObjectDeleter, event: Any) -> dict[str, int]:
    """TenantDeleted: HARD-DELETE every row + object of the tenant (all kinds)."""
    data = _event_data(event)
    if not data:
        log.warning("tenant_deleted.malformed_event")
        return {"objects": 0, "rows": 0}
    tenant_id = data.get("tenant_id")
    if not isinstance(tenant_id, str) or not tenant_id:
        log.warning("tenant_deleted.missing_tenant_id")
        return {"objects": 0, "rows": 0}
    async with db.tenant_conn(tenant_id) as conn:
        rows = await db.list_tenant_documents(conn)
    objects = 0
    for row in rows:
        try:
            await deleter.delete(row["storage_key"])
            objects += 1
        except Exception as exc:
            log.error(
                "tenant_deleted.object_delete_failed",
                document_id=str(row["id"]),
                error=str(exc),
            )
    async with db.tenant_conn(tenant_id) as conn:
        deleted = await db.delete_tenant_documents(conn)
    log.info(
        "tenant_deleted.applied",
        tenant_id=tenant_id,
        objects_deleted=objects,
        rows_deleted=deleted,
    )
    return {"objects": objects, "rows": deleted}
