"""Environment configuration for knowledge-service."""

from __future__ import annotations

from pydantic import AliasChoices, Field, model_validator
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    """All runtime configuration is environment-driven (see README env table)."""

    model_config = SettingsConfigDict(env_prefix="", case_sensitive=False)

    port: int = 7008
    log_level: str = "INFO"

    # Postgres `knowledge` database (SPEC §7). Accepts DATABASE_URL directly or
    # PG_DSN + PG_DATABASE (root-compose convention).
    database_url: str = ""
    pg_dsn: str = ""
    pg_database: str = ""

    # OpenSearch (SPEC §3 port 9200).
    opensearch_url: str = Field(
        default="http://opensearch:9200",
        validation_alias=AliasChoices("OPENSEARCH_URL", "OPENSEARCH_ADDR"),
    )
    opensearch_index: str = "kb-chunks"
    opensearch_username: str = ""
    opensearch_password: str = ""

    # Embedding model (SPEC §10: all-MiniLM-L6-v2, 384-dim).
    embed_model: str = Field(
        default="all-MiniLM-L6-v2",
        validation_alias=AliasChoices("EMBED_MODEL", "EMBEDDING_MODEL"),
    )
    embed_dim: int = 384
    # Local HF cache; the Docker image pre-populates it at build time.
    sentence_transformers_home: str = "/models/hf"

    # Chunking (~500 tokens with overlap, SPEC-mission). 1 token ~ 0.75 words.
    chunk_words: int = 375  # ~500 tokens
    chunk_overlap_words: int = 64  # ~85 tokens of overlap

    # Search defaults.
    default_k: int = 5
    max_k: int = 50
    rrf_k: int = 60  # reciprocal-rank-fusion constant

    # Self-improving KB (SPEC-W3 §4, innovation 4): when the top RRF score of
    # /v1/search falls below this and the query looks like a question, record
    # a kb_suggestions row for staff review.
    suggest_threshold: float = 0.35

    # SPEC-W43 C1: tenant context comes from the gateway-injected
    # X-Tenant-Slugs header; explicit tenant params must exactly match one
    # of its entries (else 403). OPENDESK_TRUST_DIRECT_TENANT=1 is the
    # standalone-dev escape (legacy direct tenant selection) — OFF by
    # default, never set in compose.
    trust_direct_tenant: bool = Field(
        default=False, validation_alias=AliasChoices("OPENDESK_TRUST_DIRECT_TENANT")
    )

    # Dapr sidecar (for resolving tenant slugs via identity-service).
    dapr_host: str = "daprd-knowledge"
    dapr_http_port: int = 3500
    identity_app_id: str = "identity"
    # SPEC-W44 K2: X-Internal-Token sent on the identity tenant lookup
    # (IDENTITY_INTERNAL_TOKEN env; empty = header not sent).
    identity_internal_token: str = ""

    # Text-to-SQL (SPEC-W3 §3, innovation 8): OpenAI-compatible LLM
    # (default qwen3:8b via Ollama) + Trino HTTP API.
    llm_base_url: str = "http://ollama:11434/v1"
    llm_model: str = "qwen3:8b"
    llm_api_key: str = "ollama"
    trino_url: str = "http://trino:8080"
    trino_user: str = "opendesk-analytics"

    @model_validator(mode="after")
    def _compose_database_url(self) -> "Settings":
        if not self.database_url:
            if self.pg_dsn and self.pg_database:
                self.database_url = f"{self.pg_dsn.rstrip('/')}/{self.pg_database}"
            else:
                self.database_url = (
                    "postgres://opendesk:opendesk@postgres:5432/knowledge"
                )
        return self


settings = Settings()
