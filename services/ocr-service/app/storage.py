"""MinIO document storage (SPEC-W53 Scope A).

Uses the official `minio` pip package — pure-Python (~1 MB wheel + small
deps), so the slim-image budget (I5) is preserved without hand-rolling
SigV4 in Python (the Go hand-rolled precedent in notification-worker
s3.go exists because adding ANY Go module there was the heavier option).
Choice justified further in README.md.

Uploaded bytes are PII: keys are tenant-prefixed {tenant_id}/{uuid}.<ext>;
bytes are never logged.
"""

from __future__ import annotations

import asyncio
import io

from minio import Minio

from .logging import get_logger

log = get_logger(__name__)


class StorageError(RuntimeError):
    """Object-store write/read failure (maps to 502 upstream errors)."""


class DocumentStore:
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
            endpoint,
            access_key=access_key,
            secret_key=secret_key,
            secure=secure,
        )

    @property
    def bucket(self) -> str:
        return self._bucket

    def put_object_sync(self, key: str, data: bytes, mime: str) -> None:
        try:
            self._client.put_object(
                self._bucket,
                key,
                io.BytesIO(data),
                length=len(data),
                content_type=mime,
            )
        except Exception as exc:  # minio raises a family of S3Error types
            raise StorageError(f"minio put {self._bucket}/{key}: {exc}") from exc

    async def put(self, key: str, data: bytes, mime: str) -> None:
        """Async wrapper — the minio client is sync; offload to a thread."""
        await asyncio.to_thread(self.put_object_sync, key, data, mime)
        log.info("document.stored", bucket=self._bucket, key=key, bytes=len(data))
