"""Environment-based configuration (envconfig style, no external deps)."""

from __future__ import annotations

import os
from dataclasses import dataclass, field


def _env(key: str, default: str) -> str:
    return os.environ.get(key) or default


@dataclass(frozen=True)
class Config:
    port: int = 7007

    # Postgres: PG_DSN is the base DSN (optionally without a database);
    # PG_DATABASE is appended/overrides (SPEC §7: one DB per service).
    pg_dsn: str = "postgres://opendesk:opendesk@postgres:5432"
    pg_database: str = "conversation"
    pg_min_size: int = 1
    pg_max_size: int = 10

    # Dapr sidecar (HTTP pubsub, SPEC §3: app talks to daprd-<svc>:3500).
    dapr_host: str = "daprd-conversation"
    dapr_http_port: int = 3500
    dapr_pubsub_name: str = "pubsub-kafka"
    transcripts_topic: str = "opendesk.conversation.transcripts"

    # Raw transcript sink (SPEC §5). "kafka" (aiokafka fallback) or "fluvio".
    transcript_sink: str = "kafka"
    fluvio_topic: str = "opendesk.transcripts-raw"
    kafka_brokers: list[str] = field(default_factory=lambda: ["kafka:9092"])

    # Call intelligence (SPEC-W3 §4, innovation 3). Lexicon sentiment always
    # runs; INTEL_LLM=on adds LLM NER via the same OpenAI-compatible env
    # family as the voice runtime (Ollama qwen3:8b works out of the box).
    intel_llm: bool = False
    intel_llm_base_url: str = "http://ollama:11434/v1"
    intel_llm_model: str = "qwen3:8b"
    intel_llm_api_key: str = "ollama"
    intel_llm_timeout_s: float = 3.0
    enriched_topic: str = "opendesk.conversation.enriched"

    # Call-quality sentiment enrichment (STRATEGY §3, Wave 5 innovation 2):
    # consume SessionEnded from the conversation events topic in a dedicated
    # group, publish CallQualityEnriched to the quality topic.
    quality_enrich_enabled: bool = True
    conversation_events_topic: str = "opendesk.conversation.events"
    quality_topic: str = "opendesk.conversation.quality"
    sentiment_group: str = "conversation-sentiment"

    # GDPR privacy events consumer (SPEC-W3 §2, innovation 13).
    privacy_enabled: bool = True
    privacy_topic: str = "opendesk.privacy.events"
    privacy_group: str = "conversation-service-privacy"

    # Data-retention enforcement (NDPA 2023 storage-limitation principle —
    # docs/compliance/ndpa.md). A background sweeper hard-deletes turns older
    # than retention_days, batched per tenant. Default 365 days; the NDPA
    # profile (infra/privacy/ndpa-profile.env) sets 180.
    retention_enabled: bool = True
    retention_days: int = 365
    retention_sweep_seconds: int = 3600
    retention_batch_size: int = 1000

    # Emergency-intent IDP emission (SPEC-W11 Part A): user turns scoring >=
    # incident_min_score emit an Incident Data Packet as CloudEvent
    # com.opendesk.incidents.IDPCreated to the incidents topic (non-blocking,
    # failures logged never raised).
    incident_enabled: bool = True
    incident_min_score: float = 0.6
    incidents_topic: str = "opendesk.incidents"

    # USSD inbound (SPEC-W12 contract §1/§2): synchronous hook at
    # POST /v1/ussd/turns invoked by messaging-gateway via Dapr.
    # ussd_text_mode_reply is the acknowledgement returned in pass-through
    # text mode (no tenant-pack ussd.menu); menu mode replies are rendered
    # from the pack menu. The 180s session TTL (contract §1) is enforced by
    # messaging-gateway's session store, not here — the session→conversation
    # mapping (uuid5(tenant, sessionId)) is stateless.
    ussd_enabled: bool = True
    ussd_text_mode_reply: str = (
        "Thank you. Your message has been received; an agent will respond shortly."
    )

    # OpenSearch indexer (SPEC §10, index `conversations`).
    opensearch_addr: str = "http://opensearch:9200"
    conversations_index: str = "conversations"
    indexer_enabled: bool = True
    indexer_group: str = "conversation-service-indexer"
    indexer_bulk_size: int = 100
    indexer_bulk_flush_seconds: float = 2.0

    @property
    def database_dsn(self) -> str:
        base = self.pg_dsn.rstrip("/")
        # If the DSN already ends with the target database, keep it.
        if base.endswith("/" + self.pg_database):
            return base
        return f"{base}/{self.pg_database}"


def load() -> Config:
    return Config(
        port=int(_env("PORT", "7007")),
        pg_dsn=_env("PG_DSN", "postgres://opendesk:opendesk@postgres:5432"),
        pg_database=_env("PG_DATABASE", "conversation"),
        pg_min_size=int(_env("PG_MIN_SIZE", "1")),
        pg_max_size=int(_env("PG_MAX_SIZE", "10")),
        dapr_host=_env("DAPR_HOST", "daprd-conversation"),
        dapr_http_port=int(_env("DAPR_HTTP_PORT", "3500")),
        dapr_pubsub_name=_env("DAPR_PUBSUB_NAME", "pubsub-kafka"),
        transcripts_topic=_env("TRANSCRIPTS_TOPIC", "opendesk.conversation.transcripts"),
        transcript_sink=_env("TRANSCRIPT_SINK", "kafka").lower(),
        fluvio_topic=_env("FLUVIO_TOPIC", "opendesk.transcripts-raw"),
        kafka_brokers=[b.strip() for b in _env("KAFKA_BROKERS", "kafka:9092").split(",") if b.strip()],
        intel_llm=_env("INTEL_LLM", "off").lower() in ("1", "on", "true", "yes"),
        intel_llm_base_url=_env("INTEL_LLM_BASE_URL", _env("LLM_BASE_URL", "http://ollama:11434/v1")),
        intel_llm_model=_env("INTEL_LLM_MODEL", _env("LLM_MODEL", "qwen3:8b")),
        intel_llm_api_key=_env("INTEL_LLM_API_KEY", _env("LLM_API_KEY", "ollama")),
        intel_llm_timeout_s=float(_env("INTEL_LLM_TIMEOUT_S", "3")),
        enriched_topic=_env("ENRICHED_TOPIC", "opendesk.conversation.enriched"),
        opensearch_addr=_env("OPENSEARCH_ADDR", "http://opensearch:9200"),
        conversations_index=_env("CONVERSATIONS_INDEX", "conversations"),
        indexer_enabled=_env("INDEXER_ENABLED", "true").lower() == "true",
        indexer_group=_env("INDEXER_GROUP", "conversation-service-indexer"),
        indexer_bulk_size=int(_env("INDEXER_BULK_SIZE", "100")),
        indexer_bulk_flush_seconds=float(_env("INDEXER_BULK_FLUSH_SECONDS", "2")),
        quality_enrich_enabled=_env("QUALITY_ENRICH_ENABLED", "true").lower() == "true",
        conversation_events_topic=_env(
            "CONVERSATION_EVENTS_TOPIC", "opendesk.conversation.events"
        ),
        quality_topic=_env("QUALITY_EVENTS_TOPIC", "opendesk.conversation.quality"),
        sentiment_group=_env("QUALITY_ENRICH_GROUP", "conversation-sentiment"),
        privacy_enabled=_env("PRIVACY_ENABLED", "true").lower() == "true",
        privacy_topic=_env("PRIVACY_EVENTS_TOPIC", "opendesk.privacy.events"),
        privacy_group=_env("PRIVACY_EVENTS_GROUP", "conversation-service-privacy"),
        retention_enabled=_env("RETENTION_ENABLED", "true").lower() == "true",
        retention_days=int(_env("RETENTION_DAYS", "365")),
        retention_sweep_seconds=int(_env("RETENTION_SWEEP_SECONDS", "3600")),
        retention_batch_size=int(_env("RETENTION_BATCH_SIZE", "1000")),
        incident_enabled=_env("INCIDENT_ENABLED", "true").lower() == "true",
        incident_min_score=float(_env("INCIDENT_MIN_SCORE", "0.6")),
        incidents_topic=_env("INCIDENTS_TOPIC", "opendesk.incidents"),
        ussd_enabled=_env("USSD_ENABLED", "true").lower() == "true",
        ussd_text_mode_reply=_env(
            "USSD_TEXT_MODE_REPLY",
            "Thank you. Your message has been received; an agent will respond shortly.",
        ),
    )
