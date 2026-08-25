"""conversation_outbox relay (SPEC-W43 Y-08).

Every created turn writes a conversation_outbox row in the SAME Postgres
transaction as the turn insert (Database.add_turn(outbox=...)), so a crash
between commit and Dapr publish can never lose the transcript event
(booking/billing transactional-outbox idiom, Python flavor — consistent
with the incident_emitted pattern in app/incidents.py).

The request path publishes inline and marks the row sent; this background
relay republishes rows still unsent (publish failure, crash) with
exponential backoff (attempts + next_attempt_at), marking them sent on
success. Unsent rows are NEVER dropped silently.
"""

from __future__ import annotations

import asyncio
from typing import Any

from .config import Config
from .logging import get_logger

log = get_logger(__name__)

_BACKOFF_BASE_SECONDS = 2.0
_BACKOFF_MAX_SECONDS = 300.0


def backoff_seconds(attempts: int) -> float:
    """Exponential backoff after ``attempts`` failed publishes (capped)."""
    return min(_BACKOFF_MAX_SECONDS, _BACKOFF_BASE_SECONDS * (2 ** max(0, attempts)))


class OutboxRelay:
    """Background loop publishing unsent conversation_outbox rows via Dapr.

    Mirrors RetentionSweeper: start()/stop() lifecycle, run_once() is the
    testable unit, the loop never dies on a cycle failure.
    """

    def __init__(self, cfg: Config, db: Any, dapr: Any) -> None:
        self._cfg = cfg
        self._db = db
        self._dapr = dapr
        self._task: asyncio.Task | None = None

    def start(self) -> None:
        self._task = asyncio.create_task(self._run(), name="conversation-outbox-relay")
        log.info(
            "conversation outbox relay started",
            interval_seconds=self._cfg.outbox_relay_seconds,
            batch=self._cfg.outbox_relay_batch,
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
            except Exception as exc:  # noqa: BLE001 — relay must not die
                log.error("outbox relay sweep failed; will retry next cycle",
                          error=str(exc))
            await asyncio.sleep(self._cfg.outbox_relay_seconds)

    async def run_once(self) -> int:
        """Publish every due unsent outbox row; returns the count sent."""
        rows = await self._db.outbox_unsent(limit=self._cfg.outbox_relay_batch)
        sent = 0
        for row in rows:
            try:
                await self._dapr.publish_event(row["topic"], row["payload"])
                await self._db.outbox_mark_sent(row["id"], row["tenant_id"])
                sent += 1
            except Exception as exc:  # noqa: BLE001 — keep relaying others
                delay = backoff_seconds(int(row.get("attempts") or 0))
                log.error(
                    "outbox publish failed; backing off",
                    outbox_id=str(row["id"]),
                    topic=row["topic"],
                    attempts=int(row.get("attempts") or 0) + 1,
                    retry_in_seconds=delay,
                    error=str(exc),
                )
                try:
                    await self._db.outbox_mark_failed(
                        row["id"], row["tenant_id"], delay
                    )
                except Exception as exc2:  # noqa: BLE001
                    log.error("outbox backoff mark failed",
                              outbox_id=str(row["id"]), error=str(exc2))
        return sent
