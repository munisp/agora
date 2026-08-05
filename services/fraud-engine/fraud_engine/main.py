"""fraud-engine HTTP service (SPEC-W30 §1, §4 WS-B).

Endpoints:
  GET  /healthz
  POST /v1/detect/run  {tenant_id?, detector?}   (manual run)
  GET  /v1/detect/status

Cadence (SPEC-W30 §3): event-driven triggers on Kafka where natural (D3 on
capture events, D5 on consent/messaging events) + a full sweep every
FRAUD_SWEEP_MINUTES (15).

The app is a factory: tests inject a fake GraphClient + InMemoryPublisher and
never touch FalkorDB/Kafka.
"""

from __future__ import annotations

import asyncio
import json
import logging
import threading
from contextlib import asynccontextmanager
from datetime import UTC, datetime
from typing import Any

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel

from .config import Settings, load_settings
from .detectors import ALL_DETECTORS, DetectionRunner
from .events import EventPublisher, publisher_from_settings
from .graph import GraphClient, client_from_settings

log = logging.getLogger("fraud_engine")

# Event-type substring -> detectors to trigger (SPEC-W30 §3: D3 on capture
# events, D5 on consent/messaging events).
EVENT_TRIGGERS: tuple[tuple[tuple[str, ...], tuple[str, ...]], ...] = (
    (("capture", "lead"), ("d3_capture_velocity",)),
    (("consent", "messag"), ("d5_consent_backdating",)),
)


def detectors_for_event(event_type: str) -> list[str]:
    """Pure routing helper (unit-tested). CloudEvent type -> detector names."""
    et = event_type.lower()
    out: list[str] = []
    for needles, dets in EVENT_TRIGGERS:
        if any(n in et for n in needles):
            out.extend(dets)
    return sorted(set(out))


class RunRequest(BaseModel):
    tenant_id: str | None = None
    detector: str | None = None


def create_app(
    client: GraphClient | None = None,
    publisher: EventPublisher | None = None,
    settings: Settings | None = None,
) -> FastAPI:
    settings = settings or load_settings()
    state: dict[str, Any] = {
        "settings": settings,
        "client": client,
        "publisher": publisher,
        "last_run": None,
        "sweep_task": None,
        "kafka_thread": None,
        "stop": threading.Event(),
    }

    def get_client() -> GraphClient:
        if state["client"] is None:
            state["client"] = client_from_settings(settings)
        return state["client"]

    def get_publisher() -> EventPublisher:
        if state["publisher"] is None:
            state["publisher"] = publisher_from_settings(settings)
        return state["publisher"]

    def get_runner() -> DetectionRunner:
        return DetectionRunner(get_client(), get_publisher(), settings)

    def run_and_record(tenant_id: str | None = None, detector: str | None = None):
        report = get_runner().run(tenant_id=tenant_id, detector=detector)
        state["last_run"] = report.as_dict()
        return report

    async def sweep_loop() -> None:
        # Full sweep every FRAUD_SWEEP_MINUTES (SPEC-W30 §3).
        await asyncio.sleep(5)  # let the graph warm up after boot
        while not state["stop"].is_set():
            try:
                await asyncio.to_thread(run_and_record)
            except Exception as exc:  # noqa: BLE001
                log.error("sweep failed: %s", exc)
            await asyncio.sleep(settings.sweep_minutes * 60)

    def kafka_loop() -> None:
        """Event-driven triggers (D3/D5). confluent-kafka, lazy import."""
        try:
            from confluent_kafka import Consumer
        except ImportError:
            log.error("KAFKA_ENABLED=true but confluent-kafka is not installed")
            return
        consumer = Consumer(
            {
                "bootstrap.servers": settings.kafka_bootstrap_servers,
                "group.id": settings.kafka_group_id,
                "auto.offset.reset": "latest",
            }
        )
        topics = [t.strip() for t in settings.kafka_trigger_topics.split(",") if t.strip()]
        consumer.subscribe(topics)
        log.info("kafka triggers subscribed: %s", topics)
        try:
            while not state["stop"].is_set():
                msg = consumer.poll(1.0)
                if msg is None or msg.error():
                    continue
                try:
                    event = json.loads(msg.value().decode())
                    tenant = event.get("tenantid") or (event.get("data") or {}).get("tenantId")
                    etype = str(event.get("type") or "")
                    if not tenant:
                        continue
                    for det in detectors_for_event(etype):
                        run_and_record(tenant_id=str(tenant), detector=det)
                except Exception as exc:  # noqa: BLE001 — never kill the loop
                    log.error("trigger handling failed: %s", exc)
        finally:
            consumer.close()

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        if settings.sweep_enabled:
            state["sweep_task"] = asyncio.create_task(sweep_loop())
        if settings.kafka_enabled:
            state["kafka_thread"] = threading.Thread(target=kafka_loop, daemon=True)
            state["kafka_thread"].start()
        yield
        state["stop"].set()
        if state["sweep_task"]:
            state["sweep_task"].cancel()
        if state["client"] is not None:
            state["client"].close()

    app = FastAPI(title="fraud-engine", version="0.1.0", lifespan=lifespan)
    app.state.fraud = state

    @app.get("/healthz")
    def healthz() -> dict[str, Any]:
        graph_ok = False
        try:
            graph_ok = get_client().ping()
        except Exception:  # noqa: BLE001
            graph_ok = False
        return {
            "status": "ok",
            "service": "fraud-engine",
            "graph": "up" if graph_ok else "unavailable",
            "time": datetime.now(UTC).isoformat(),
        }

    @app.post("/v1/detect/run")
    def detect_run(req: RunRequest) -> dict[str, Any]:
        try:
            report = run_and_record(tenant_id=req.tenant_id, detector=req.detector)
        except KeyError as exc:
            raise HTTPException(
                status_code=400,
                detail={"error": str(exc), "known_detectors": [d.name for d in ALL_DETECTORS]},
            ) from exc
        return report.as_dict()

    @app.get("/v1/detect/status")
    def detect_status() -> dict[str, Any]:
        return {
            "service": "fraud-engine",
            "sweep_minutes": settings.sweep_minutes,
            "sweep_enabled": settings.sweep_enabled,
            "kafka_enabled": settings.kafka_enabled,
            "alerts_topic": settings.alerts_topic,
            "detectors": [d.name for d in ALL_DETECTORS],
            "thresholds": {
                "SYBIL_WINDOW_MIN": settings.sybil_window_min,
                "SYBIL_SIM_THRESHOLD": settings.sybil_sim_threshold,
                "SYBIL_HIGH_SIZE": settings.sybil_high_size,
                "CAPTURE_VELOCITY_MAX": settings.capture_velocity_max,
                "CAPTURE_WINDOW_MIN": settings.capture_window_min,
                "CAPTURE_SUSTAINED_WINDOWS": settings.capture_sustained_windows,
                "MAX_TRAVEL_KMH": settings.max_travel_kmh,
                "GHOST_MIN": settings.ghost_min,
                "GHOST_WINDOW_MIN": settings.ghost_window_min,
                "ANOMALY_ALERT_THRESHOLD": settings.anomaly_alert_threshold,
                "FRAUD_SWEEP_MINUTES": settings.sweep_minutes,
            },
            "last_run": state["last_run"],
        }

    return app


app = create_app()


def main() -> None:  # pragma: no cover
    import uvicorn

    settings = load_settings()
    uvicorn.run("fraud_engine.main:app", host=settings.host, port=settings.port)


if __name__ == "__main__":  # pragma: no cover
    main()
