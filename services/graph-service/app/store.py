"""Postgres-free segment/audience persistence (SPEC-W28 §4 WS-B).

Choice (documented in README): JSON file store — one file per record under
``SEGMENT_STORE_DIR`` with an in-memory index rebuilt at startup. Rationale:
segments are small, low-churn metadata; a file store keeps the service
Postgres-free per the spec while staying durable across restarts and easy to
back up alongside FalkorDB RDB snapshots. FalkorDB-persistence was rejected:
segment definitions are operational metadata, not graph relationships, and
storing them in the graph would couple admin metadata to tenant data nodes.

Writes are atomic (tmp file + rename). Records carry tenant_id; every lookup
is tenant-scoped (cross-tenant access returns None -> 404).
"""

from __future__ import annotations

import json
import os
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


class SegmentStore:
    def __init__(self, root: str | Path) -> None:
        self._segments_dir = Path(root) / "segments"
        self._audiences_dir = Path(root) / "audiences"
        self._segments: dict[str, dict[str, Any]] = {}
        self._audiences: dict[str, dict[str, Any]] = {}
        self._load()

    # --- persistence helpers -------------------------------------------------
    def _load(self) -> None:
        for directory, index in (
            (self._segments_dir, self._segments),
            (self._audiences_dir, self._audiences),
        ):
            directory.mkdir(parents=True, exist_ok=True)
            for path in sorted(directory.glob("*.json")):
                try:
                    record = json.loads(path.read_text(encoding="utf-8"))
                except (ValueError, OSError):
                    continue
                if isinstance(record, dict) and record.get("id"):
                    index[record["id"]] = record

    @staticmethod
    def _write(directory: Path, record: dict[str, Any]) -> None:
        directory.mkdir(parents=True, exist_ok=True)
        tmp = directory / f".{record['id']}.tmp"
        tmp.write_text(json.dumps(record, indent=2, sort_keys=True), encoding="utf-8")
        os.replace(tmp, directory / f"{record['id']}.json")

    # --- segments -------------------------------------------------------------
    def create_segment(
        self, tenant_id: str, payload: dict[str, Any], compiled_cypher: str
    ) -> dict[str, Any]:
        record = {
            "id": uuid.uuid4().hex,
            "tenant_id": tenant_id,
            "created_at": datetime.now(timezone.utc).isoformat(),
            "compiled_cypher": compiled_cypher,
            **payload,
        }
        self._segments[record["id"]] = record
        self._write(self._segments_dir, record)
        return record

    def list_segments(self, tenant_id: str) -> list[dict[str, Any]]:
        rows = [r for r in self._segments.values() if r.get("tenant_id") == tenant_id]
        rows.sort(key=lambda r: r.get("created_at") or "")
        return rows

    def get_segment(self, tenant_id: str, segment_id: str) -> dict[str, Any] | None:
        record = self._segments.get(segment_id)
        if record is None or record.get("tenant_id") != tenant_id:
            return None
        return record

    # --- audiences --------------------------------------------------------------
    def create_audience(
        self,
        tenant_id: str,
        segment_id: str,
        campaign_id: str | None,
        members: list[dict[str, Any]],
    ) -> dict[str, Any]:
        record = {
            "id": uuid.uuid4().hex,
            "tenant_id": tenant_id,
            "segment_id": segment_id,
            "campaign_id": campaign_id,
            "created_at": datetime.now(timezone.utc).isoformat(),
            "member_count": len(members),
            "members": members,
        }
        self._audiences[record["id"]] = record
        self._write(self._audiences_dir, record)
        return record

    def get_audience(self, tenant_id: str, audience_id: str) -> dict[str, Any] | None:
        record = self._audiences.get(audience_id)
        if record is None or record.get("tenant_id") != tenant_id:
            return None
        return record
