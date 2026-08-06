"""Environment-driven settings (no pydantic-settings — stdlib dataclass,
mirroring services/analytics-pipeline/analytics_pipeline/config.py)."""

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
    # HTTP server (SPEC-W28 §4 WS-B: port 7014).
    port: int = field(default_factory=lambda: _int("PORT", 7014))
    host: str = field(default_factory=lambda: os.getenv("HOST", "0.0.0.0"))

    # FalkorDB. GRAPH_BACKEND=memory swaps in the in-memory backend
    # (dev/test; the pytest suite never needs a live graph DB).
    graph_backend: str = field(default_factory=lambda: os.getenv("GRAPH_BACKEND", "falkordb"))
    falkordb_host: str = field(default_factory=lambda: os.getenv("FALKORDB_HOST", "localhost"))
    falkordb_port: int = field(default_factory=lambda: _int("FALKORDB_PORT", 6379))
    falkordb_graph: str = field(
        default_factory=lambda: os.getenv("FALKORDB_GRAPH", "agora_tenants")
    )
    falkordb_username: str = field(
        default_factory=lambda: os.getenv("FALKORDB_USERNAME", "")
    )
    falkordb_password: str = field(
        default_factory=lambda: os.getenv("FALKORDB_PASSWORD", "")
    )

    # Workforce auth seam: tenant identity comes from the JWT `sub` claim.
    # JWT_PUBLIC_KEY set  -> Bearer JWT required (signature verified).
    # JWT_PUBLIC_KEY unset -> dev mode: X-Tenant-Id header is accepted
    # (mirrors the analytics-pipeline sidecar conventions for dev compose).
    jwt_public_key: str = field(default_factory=lambda: os.getenv("JWT_PUBLIC_KEY", ""))
    jwt_algorithm: str = field(default_factory=lambda: os.getenv("JWT_ALGORITHM", "RS256"))

    # Internal service-to-service write-back API (SPEC-W29 §3 WS-B): the
    # X-Internal-Token header must equal this value (constant-time compare).
    # Empty -> every internal call answers 401 (fail-closed).
    internal_token: str = field(default_factory=lambda: os.getenv("INTERNAL_TOKEN", ""))

    # SPEC-W33 §2 A2 graph export: salt for the W28 sha256(salt|tenant|id)
    # person-identifier hash — SAME env var and scheme as graph-sync
    # (PHONE_HASH_SALT, SPEC-W28 §3). Person node ids never leave the
    # internal export endpoints unhashed (I6).
    phone_hash_salt: str = field(default_factory=lambda: os.getenv("PHONE_HASH_SALT", ""))

    # Dev/e2e fixture seeder (SPEC-W30 WS-C addendum): the fixtures router is
    # mounted ONLY when E2E_FIXTURES=1; production images leave it unset.
    e2e_fixtures: bool = field(default_factory=lambda: _bool("E2E_FIXTURES", False))

    # Fraud alert audit events (SPEC-W30 §4 WS-C): CloudEvents published to
    # Kafka when configured; otherwise a no-op logger publisher is used.
    kafka_bootstrap_servers: str = field(
        default_factory=lambda: os.getenv("KAFKA_BOOTSTRAP_SERVERS", "")
    )
    fraud_alerts_topic: str = field(
        default_factory=lambda: os.getenv("FRAUD_ALERTS_TOPIC", "opendesk.fraud.alerts.v1")
    )

    # Ollama NL->Cypher (OpenAI-compatible chat API).
    ollama_base_url: str = field(
        default_factory=lambda: os.getenv("OLLAMA_BASE_URL", "http://localhost:11434/v1")
    )
    graph_ask_model: str = field(
        default_factory=lambda: os.getenv("GRAPH_ASK_MODEL", "qwen2.5:7b-instruct")
    )
    ollama_api_key: str = field(default_factory=lambda: os.getenv("OLLAMA_API_KEY", "ollama"))
    ollama_timeout_s: float = field(
        default_factory=lambda: float(os.getenv("OLLAMA_TIMEOUT_S", "30"))
    )

    # Postgres-free segment/audience persistence (JSON file store; see README).
    segment_store_dir: str = field(
        default_factory=lambda: os.getenv("SEGMENT_STORE_DIR", "./data/graph-service")
    )

    # Row caps (SPEC-W28 §4: ask results capped at 100).
    ask_row_cap: int = field(default_factory=lambda: _int("ASK_ROW_CAP", 100))
    query_row_cap: int = field(default_factory=lambda: _int("QUERY_ROW_CAP", 100))
    segment_row_cap: int = field(default_factory=lambda: _int("SEGMENT_ROW_CAP", 10000))

    log_level: str = field(default_factory=lambda: os.getenv("LOG_LEVEL", "INFO"))


def load_settings() -> Settings:
    return Settings()
