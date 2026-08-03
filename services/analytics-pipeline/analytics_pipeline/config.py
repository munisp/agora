"""Environment-driven settings (no pydantic — stdlib dataclass).

Every knob is documented in the README env table; defaults target the dev compose
network (`opendesk`) from infra/docker-compose.lakehouse.yml + the core compose.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field


def _int(name: str, default: int) -> int:
    raw = os.getenv(name)
    if raw is None or raw == "":
        return default
    return int(raw)


def _bool(name: str, default: bool) -> bool:
    raw = os.getenv(name)
    if raw is None or raw == "":
        return default
    return raw.strip().lower() in {"1", "true", "yes", "on"}


@dataclass(frozen=True)
class Settings:
    # Kafka (SPEC §4 topics)
    kafka_bootstrap_servers: str = field(
        default_factory=lambda: os.getenv("KAFKA_BOOTSTRAP_SERVERS", "kafka:9092")
    )
    kafka_group_id: str = field(
        default_factory=lambda: os.getenv("KAFKA_GROUP_ID", "analytics-pipeline")
    )
    topic_booking_events: str = field(
        default_factory=lambda: os.getenv("TOPIC_BOOKING_EVENTS", "opendesk.booking.events")
    )
    topic_payment_events: str = field(
        default_factory=lambda: os.getenv("TOPIC_PAYMENT_EVENTS", "opendesk.payments.events")
    )
    topic_transcripts: str = field(
        default_factory=lambda: os.getenv("TOPIC_TRANSCRIPTS", "opendesk.conversation.transcripts")
    )
    topic_usage_events: str = field(
        default_factory=lambda: os.getenv("TOPIC_USAGE_EVENTS", "opendesk.usage.events")
    )

    # Micro-batching
    batch_size: int = field(default_factory=lambda: _int("BATCH_SIZE", 500))
    flush_interval_seconds: float = field(
        default_factory=lambda: float(os.getenv("FLUSH_INTERVAL", "15"))
    )

    # Iceberg REST catalog + MinIO warehouse (SPEC §13)
    iceberg_rest_uri: str = field(
        default_factory=lambda: os.getenv("ICEBERG_REST_URI", "http://iceberg-rest:8181")
    )
    iceberg_warehouse: str = field(
        default_factory=lambda: os.getenv("ICEBERG_WAREHOUSE", "s3://lake/warehouse")
    )
    aws_access_key_id: str = field(
        default_factory=lambda: os.getenv("AWS_ACCESS_KEY_ID", "minioadmin")
    )
    aws_secret_access_key: str = field(
        default_factory=lambda: os.getenv("AWS_SECRET_ACCESS_KEY", "minioadmin")
    )
    aws_endpoint_url: str = field(
        default_factory=lambda: os.getenv("AWS_ENDPOINT_URL", "http://minio:9000")
    )
    aws_region: str = field(default_factory=lambda: os.getenv("AWS_REGION", "us-east-1"))
    auto_create_tables: bool = field(
        default_factory=lambda: _bool("AUTO_CREATE_TABLES", True)
    )

    # Sidecar HTTP server
    port: int = field(default_factory=lambda: _int("PORT", 7009))
    host: str = field(default_factory=lambda: os.getenv("HOST", "0.0.0.0"))

    # --- SPEC-W13: CAC rollups (cac.events consumer + /v1/cac/summary) ------
    # Contract §7 env names: CAC_EVENTS_TOPIC / CAC_EVENTS_GROUP.
    cac_events_topic: str = field(
        default_factory=lambda: os.getenv("CAC_EVENTS_TOPIC", "cac.events")
    )
    cac_events_group: str = field(
        default_factory=lambda: os.getenv("CAC_EVENTS_GROUP", "analytics-cac")
    )
    # Postgres for the realtime rollup tables (one DB per service, SPEC §7:
    # analytics_meta). PG_DSN is the base DSN (optionally without a database);
    # PG_DATABASE is appended/overrides — same convention as
    # conversation-service.
    pg_dsn: str = field(
        default_factory=lambda: os.getenv("PG_DSN", "postgres://opendesk:opendesk@postgres:5432")
    )
    pg_database: str = field(
        default_factory=lambda: os.getenv("PG_DATABASE", "analytics_meta")
    )
    pg_min_size: int = field(default_factory=lambda: _int("PG_MIN_SIZE", 1))
    pg_max_size: int = field(default_factory=lambda: _int("PG_MAX_SIZE", 4))
    # Dapr sidecar (spend join to booking-service + tenant slug resolution
    # against identity-service).
    dapr_host: str = field(
        default_factory=lambda: os.getenv("DAPR_HOST", "daprd-analytics")
    )
    dapr_http_port: int = field(default_factory=lambda: _int("DAPR_HTTP_PORT", 3500))
    booking_app_id: str = field(
        default_factory=lambda: os.getenv("BOOKING_APP_ID", "booking")
    )
    identity_app_id: str = field(
        default_factory=lambda: os.getenv("IDENTITY_APP_ID", "identity")
    )
    tenant_cache_ttl_seconds: float = field(
        default_factory=lambda: float(os.getenv("TENANT_CACHE_TTL_SECONDS", "300"))
    )
    # Set CAC_CONSUMER_ENABLED=false to run the bronze sink only (e.g. while
    # Postgres is unavailable in a dev tier); /v1/cac/summary then answers 503.
    cac_consumer_enabled: bool = field(
        default_factory=lambda: _bool("CAC_CONSUMER_ENABLED", True)
    )

    # Startup resilience: catalog/kafka may still be booting in compose.
    startup_retry_seconds: float = field(
        default_factory=lambda: float(os.getenv("STARTUP_RETRY_SECONDS", "5"))
    )
    startup_max_attempts: int = field(default_factory=lambda: _int("STARTUP_MAX_ATTEMPTS", 60))


def load_settings() -> Settings:
    return Settings()
