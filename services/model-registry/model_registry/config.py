"""Environment-driven settings (SPEC-W33 §4). No secrets beyond the PG DSN."""

from __future__ import annotations

import os
from dataclasses import dataclass


@dataclass(frozen=True)
class Settings:
    port: int = 7019
    # psycopg (v3) sync DSN for the `platform` DB, app role (RLS subject).
    pg_dsn: str = (
        "postgresql://app_model_registry_login:app_model_registry_dev_password"
        "@localhost:5432/platform"
    )
    kafka_enabled: bool = True
    kafka_bootstrap_servers: str = "kafka:9092"
    alerts_topic: str = "ops.alerts"
    drift_interval_minutes: int = 15
    drift_psi_threshold: float = 0.25
    # Directory of training-snapshot reference manifests (C2); one JSON file
    # per family. See drift.py / README for the manifest schema.
    drift_manifest_dir: str = "/data/manifests"
    # Nightly continuous-training cron (C5). Disabled unless trainers are
    # plugged in (family trainers live in sibling services).
    train_cron_hour: int = 2
    train_enabled: bool = False
    train_brier_max: float = 0.20
    train_aucpr_tolerance: float = 0.02
    # I2 provenance: git sha of the running service build, where knowable.
    git_sha: str = "unknown"


def _bool(name: str, default: bool) -> bool:
    raw = os.getenv(name)
    if raw is None:
        return default
    return raw.strip().lower() in ("1", "true", "yes", "on")


def load_settings() -> Settings:
    return Settings(
        port=int(os.getenv("PORT", "7019")),
        pg_dsn=os.getenv("MODEL_REGISTRY_PG_DSN", Settings.pg_dsn),
        kafka_enabled=_bool("KAFKA_ENABLED", True),
        kafka_bootstrap_servers=os.getenv("KAFKA_BOOTSTRAP_SERVERS", "kafka:9092"),
        alerts_topic=os.getenv("ALERTS_TOPIC", "ops.alerts"),
        drift_interval_minutes=int(os.getenv("DRIFT_INTERVAL_MINUTES", "15")),
        drift_psi_threshold=float(os.getenv("DRIFT_PSI_THRESHOLD", "0.25")),
        drift_manifest_dir=os.getenv("DRIFT_MANIFEST_DIR", "/data/manifests"),
        train_cron_hour=int(os.getenv("TRAIN_CRON_HOUR", "2")),
        train_enabled=_bool("TRAIN_ENABLED", False),
        train_brier_max=float(os.getenv("TRAIN_BRIER_MAX", "0.20")),
        train_aucpr_tolerance=float(os.getenv("TRAIN_AUCPR_TOLERANCE", "0.02")),
        git_sha=os.getenv("GIT_SHA", "unknown"),
    )
