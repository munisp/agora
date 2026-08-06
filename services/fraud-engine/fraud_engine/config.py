"""Environment-driven settings (stdlib dataclass, mirroring
services/graph-service/app/config.py conventions).

Every detector threshold from SPEC-W30 §3 is an env override so ops can
tune without a deploy; defaults are the SPEC values.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field


def _int(name: str, default: int) -> int:
    raw = os.getenv(name)
    if raw is None or raw == "":
        return default
    return int(raw)


def _float(name: str, default: float) -> float:
    raw = os.getenv(name)
    if raw is None or raw == "":
        return default
    return float(raw)


def _bool(name: str, default: bool) -> bool:
    raw = os.getenv(name)
    if raw is None or raw == "":
        return default
    return raw.strip().lower() in {"1", "true", "yes", "on"}


@dataclass(frozen=True)
class Settings:
    # HTTP server (SPEC-W30 §1: fraud-engine on :7017, internal only).
    port: int = field(default_factory=lambda: _int("PORT", 7017))
    host: str = field(default_factory=lambda: os.getenv("HOST", "0.0.0.0"))

    # FalkorDB (Redis protocol). GRAPH_BACKEND=memory is not shipped here —
    # tests inject a fake GraphClient; production uses RedisGraphClient.
    falkordb_host: str = field(default_factory=lambda: os.getenv("FALKORDB_HOST", "localhost"))
    falkordb_port: int = field(default_factory=lambda: _int("FALKORDB_PORT", 6379))
    falkordb_graph: str = field(default_factory=lambda: os.getenv("FALKORDB_GRAPH", "agora_tenants"))
    falkordb_username: str = field(default_factory=lambda: os.getenv("FALKORDB_USERNAME", ""))
    falkordb_password: str = field(default_factory=lambda: os.getenv("FALKORDB_PASSWORD", ""))

    # Kafka / CloudEvents (SPEC-W30 §3: com.opendesk.fraud.AlertRaised).
    kafka_bootstrap_servers: str = field(
        default_factory=lambda: os.getenv("KAFKA_BOOTSTRAP_SERVERS", "localhost:9092")
    )
    kafka_enabled: bool = field(default_factory=lambda: _bool("KAFKA_ENABLED", False))
    kafka_group_id: str = field(default_factory=lambda: os.getenv("FRAUD_KAFKA_GROUP", "fraud-engine"))
    # Event-driven triggers (D3 on capture events, D5 on consent/messaging
    # events) consume the same topics as graph-sync.
    kafka_trigger_topics: str = field(
        default_factory=lambda: os.getenv(
            "FRAUD_KAFKA_TOPICS",
            "opendesk.cac.events,opendesk.identity.events,opendesk.civic.events.v1",
        )
    )
    alerts_topic: str = field(
        default_factory=lambda: os.getenv("FRAUD_ALERTS_TOPIC", "opendesk.fraud.alerts.v1")
    )

    # Sweep cadence (SPEC-W30 §3: full sweep every FRAUD_SWEEP_MINUTES = 15).
    sweep_minutes: int = field(default_factory=lambda: _int("FRAUD_SWEEP_MINUTES", 15))
    sweep_enabled: bool = field(default_factory=lambda: _bool("FRAUD_SWEEP_ENABLED", True))

    # D1 referral_cycle.
    referral_cycle_min_hops: int = field(default_factory=lambda: _int("REFERRAL_CYCLE_MIN_HOPS", 2))
    referral_cycle_max_hops: int = field(default_factory=lambda: _int("REFERRAL_CYCLE_MAX_HOPS", 4))

    # D2 sybil_cluster (SPEC-W30 §3: window 60 min, cosine >= 0.98, >=5 => high).
    sybil_window_min: int = field(default_factory=lambda: _int("SYBIL_WINDOW_MIN", 60))
    sybil_sim_threshold: float = field(
        default_factory=lambda: _float("SYBIL_SIM_THRESHOLD", 0.98)
    )
    sybil_high_size: int = field(default_factory=lambda: _int("SYBIL_HIGH_SIZE", 5))
    sybil_lookback_hours: int = field(default_factory=lambda: _int("SYBIL_LOOKBACK_HOURS", 24))

    # D3 capture_velocity (SPEC-W30 §3: >30 leads in any rolling 60 min).
    capture_velocity_max: int = field(default_factory=lambda: _int("CAPTURE_VELOCITY_MAX", 30))
    capture_window_min: int = field(default_factory=lambda: _int("CAPTURE_WINDOW_MIN", 60))
    capture_sustained_windows: int = field(
        default_factory=lambda: _int("CAPTURE_SUSTAINED_WINDOWS", 3)
    )
    capture_lookback_hours: int = field(default_factory=lambda: _int("CAPTURE_LOOKBACK_HOURS", 24))

    # D4 geo_impossibility (SPEC-W30 §3: implied speed > 120 km/h).
    max_travel_kmh: float = field(default_factory=lambda: _float("MAX_TRAVEL_KMH", 120.0))
    geo_lookback_hours: int = field(default_factory=lambda: _int("GEO_LOOKBACK_HOURS", 24))

    # D6 ghost_booking (SPEC-W30 §3: >=3 create+cancel within 10 min, same staff, same day).
    ghost_min: int = field(default_factory=lambda: _int("GHOST_MIN", 3))
    ghost_window_min: int = field(default_factory=lambda: _int("GHOST_WINDOW_MIN", 10))
    ghost_lookback_days: int = field(default_factory=lambda: _int("GHOST_LOOKBACK_DAYS", 7))

    # D8 report_spam (SPEC-W32 §3 WS-D). Severity is ALWAYS medium and D8
    # NEVER auto-quarantines — citizens are never banned from reporting;
    # alerts inform operator triage only.
    # (a) one reporter opening > CIVIC_REPORT_MAX_PER_DAY cases per UTC day.
    civic_report_max_per_day: int = field(
        default_factory=lambda: _int("CIVIC_REPORT_MAX_PER_DAY", 5)
    )
    civic_report_lookback_days: int = field(
        default_factory=lambda: _int("CIVIC_REPORT_LOOKBACK_DAYS", 7)
    )
    # (b) coordinated spam: > CIVIC_COORD_CASE_THRESHOLD open cases of the
    # same category within CIVIC_COORD_RADIUS_M metres AND
    # CIVIC_COORD_WINDOW_HOURS hours across DIFFERENT reporters.
    civic_coord_case_threshold: int = field(
        default_factory=lambda: _int("CIVIC_COORD_CASE_THRESHOLD", 3)
    )
    civic_coord_radius_m: float = field(
        default_factory=lambda: _float("CIVIC_COORD_RADIUS_M", 500.0)
    )
    civic_coord_window_hours: int = field(
        default_factory=lambda: _int("CIVIC_COORD_WINDOW_HOURS", 24)
    )

    # D7 gnn_anomaly (SPEC-W30 §3: consume risk_score >= 0.9 from the W29 sweep).
    anomaly_alert_threshold: float = field(
        default_factory=lambda: _float("ANOMALY_ALERT_THRESHOLD", 0.9)
    )
    # severity: >= this => medium, else low (SPEC: "low/medium").
    anomaly_medium_threshold: float = field(
        default_factory=lambda: _float("ANOMALY_MEDIUM_THRESHOLD", 0.97)
    )

    # W33-B learned scorer (SPEC-W33 §3 B1). Default UNSET = OFF: detection
    # output is then byte-equal to the pre-W33-B pure-rule pipeline (GB3).
    # When set to a registry dir, each finding's person is blended with the
    # fraud-ae+fraud-clf score and the alert evidence gains an
    # "ml_blend ae=<x> clf=<y>" reason; severities may be raised UPWARD only
    # (low -> medium; the high band and auto-quarantine stay rule-only), so a
    # rule verdict is never weakened (I1 UNION).
    ml_registry_dir: str = field(
        default_factory=lambda: os.getenv("FRAUD_ML_REGISTRY_DIR", "")
    )
    ml_score_threshold: float = field(
        default_factory=lambda: _float("FRAUD_ML_SCORE_THRESHOLD", 0.9)
    )

    log_level: str = field(default_factory=lambda: os.getenv("LOG_LEVEL", "INFO"))


def load_settings() -> Settings:
    return Settings()
