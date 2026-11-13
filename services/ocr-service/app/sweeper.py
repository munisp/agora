"""SPEC-W58 GATE R3 (P4): TTL retention sweeper + MinIO object deletion.

storage.py is intentionally upload-only (W53: no DELETE endpoint exists);
this module owns the delete path. The sweeper is kind-GENERIC (any row with
expires_at set) but today only kyc_* rows get expires_at
(OCR_KYC_TTL_HOURS, default 72h), so the effective scope is KYC PII.

Cadence: OCR_KYC_SWEEP_MINUTES (default 15). 'erased' rows are skipped by
the query — their objects are already deleted and the blanked row is the
hash-only audit stub retained per P3.

MinIO bucket lifecycle rules on `documents/` are the belt-and-braces
complement (docs only — cannot be provisioned from here).
"""

from __future__ import annotations

import asyncio
from typing import Any, Protocol

from minio import Minio

from .logging import get_logger
from .storage import StorageError

log = get_logger(__name__)


class ObjectDeleter(Protocol):
    """Anything that can delete one object by key (DocumentStore fakes in
    tests satisfy this without a MinIO client)."""

    async def delete(self, key: str) -> None: ...


class MinioObjectDeleter:
    """MinIO remove_object wrapper (sync client offloaded to a thread)."""

    def __init__(
        self,
        *,
        endpoint: str,
        access_key: str,
        secret_key: str,
        bucket: str,
        secure: bool = False,
    ):
        self._bucket = bucket
        self._client = Minio(
            endpoint, access_key=access_key, secret_key=secret_key, secure=secure
        )

    def delete_object_sync(self, key: str) -> None:
        try:
            # S3 DELETE on a missing key is a success — idempotent retries.
            self._client.remove_object(self._bucket, key)
        except Exception as exc:
            raise StorageError(f"minio delete {self._bucket}/{key}: {exc}") from exc

    async def delete(self, key: str) -> None:
        await asyncio.to_thread(self.delete_object_sync, key)


async def sweep_expired_once(db: Any, deleter: ObjectDeleter, *, limit: int = 500) -> int:
    """One sweep pass: delete expired rows + their MinIO objects.

    Object deleted FIRST, then the row; a row whose object delete fails is
    kept so the next pass retries (never a row without its object). Returns
    the number of rows deleted. Never raises.
    """
    try:
        async with db.sweep_conn() as conn:
            rows = await db.list_expired(conn, limit=limit)
    except Exception as exc:
        log.error("kyc_ttl.sweep_query_failed", error=str(exc))
        return 0
    deleted = 0
    for row in rows:
        try:
            await deleter.delete(row["storage_key"])
        except Exception as exc:
            log.error(
                "kyc_ttl.object_delete_failed",
                document_id=str(row["id"]),
                error=str(exc),
            )
            continue
        try:
            async with db.sweep_conn() as conn:
                await db.delete_document(conn, row["id"])
        except Exception as exc:
            log.error(
                "kyc_ttl.row_delete_failed",
                document_id=str(row["id"]),
                error=str(exc),
            )
            continue
        deleted += 1
    if rows:
        log.info("kyc_ttl.sweep_complete", expired=len(rows), deleted=deleted)
    return deleted


async def sweeper_loop(db: Any, deleter: ObjectDeleter, *, interval_minutes: int) -> None:
    """Periodic sweep until cancelled (lifespan-managed asyncio task)."""
    interval_s = max(interval_minutes, 1) * 60
    log.info("kyc_ttl.sweeper_started", interval_minutes=interval_minutes)
    while True:
        await asyncio.sleep(interval_s)
        try:
            await sweep_expired_once(db, deleter)
        except Exception as exc:  # sweep_expired_once never raises; belt
            log.error("kyc_ttl.sweep_failed", error=str(exc))
