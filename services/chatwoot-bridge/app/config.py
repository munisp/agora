"""chatwoot-bridge configuration (pydantic-settings; env names verbatim).

SPEC-W118 §4. DEV ONLY defaults match docker-compose.yml / .env.example so
the dev stack boots without a .env; production MUST override every secret
via the secrets store (docs/runbooks/secrets.md).
"""

from __future__ import annotations

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(extra="ignore")

    # Postgres (chatwoot_bridge DB; least-privilege app_chatwoot_bridge_login
    # by default — infra/postgres/init-scripts/05-app-roles.sql).
    pg_dsn: str = (
        "postgres://app_chatwoot_bridge_login:"
        "app_chatwoot_bridge_dev_password@postgres:5432/chatwoot_bridge"
    )

    # Chatwoot rails (compose service) + the URL Chatwoot uses to call BACK
    # into this bridge (webhook registration target).
    chatwoot_base_url: str = "http://chatwoot-rails:3000"
    bridge_public_base_url: str = "http://chatwoot-bridge:7021"

    # conversation-service internal surface (SPEC-W118 §3; X-Internal-Token
    # must equal the conversation block's CONVERSATION_INTERNAL_TOKEN).
    conversation_service_url: str = "http://conversation:7007"
    chatwoot_conversation_internal_token: str = ""

    # Server side of the bridge's own /internal/* admin routes
    # (K19 guard idiom: 503 when unset, 401 on mismatch).
    chatwoot_bridge_internal_token: str = ""

    # Kafka (direct-broker consumer, W10 TranscriptIndexer idiom).
    kafka_brokers: str = "kafka:9092"
    transcripts_topic: str = "opendesk.conversation.transcripts"
    bridge_group: str = "chatwoot-bridge"
    consumer_batch_size: int = 50

    # Kill switch for the outbound consumer only (the inbound webhook —
    # the CRM-ingest direction — stays up).
    sync_enabled: bool = True

    chatwoot_timeout_s: float = 10.0
    conversation_timeout_s: float = 10.0

    port: int = 7021

    @property
    def brokers(self) -> list[str]:
        return [b.strip() for b in self.kafka_brokers.split(",") if b.strip()]


def load() -> Settings:
    return Settings()
