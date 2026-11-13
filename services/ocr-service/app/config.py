"""Environment configuration for ocr-service (SPEC-W53 Scope A).

All runtime configuration is env-driven; every value has a dev-safe default
matching docker-compose.yml / .env.example. OCR_MOCK is safe-default-OFF —
never default it on (XTTS_MOCK precedent, RECON §4).
"""

from __future__ import annotations

from pydantic import AliasChoices, Field, model_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

from .logging import get_logger

log = get_logger(__name__)

#: MIME allow-list (magic-byte validated, RECON §4): images + PDF only.
ALLOWED_MIMES = ("image/jpeg", "image/png", "image/webp", "application/pdf")

#: KYC document kinds (SPEC-W58 P9). The single `kyc_` prefix keeps the
#: NDPA redaction/retention branching a one-`startswith` check (RECON §3).
KYC_DOCUMENT_KINDS = (
    "kyc_nin_slip",
    "kyc_drivers_license",
    "kyc_passport",
    "kyc_voters_card",
    "kyc_cac_document",
)

#: Document kinds accepted by POST /v1/ocr/extract (CHECK constraint mirrors).
DOCUMENT_KINDS = ("receipt", "document", "knowledge_scan") + KYC_DOCUMENT_KINDS


def is_kyc_kind(kind: str) -> bool:
    """True for the KYC document kinds (NDPA gates R1-R3 key off this)."""
    return kind.startswith("kyc_")


def _int_or_default(key: str, raw: str, default: int) -> int:
    """voice-agent _env_int idiom: invalid value warns + falls back to the
    default instead of hard-failing boot (SPEC-W58 A1)."""
    try:
        return int(str(raw).strip())
    except (TypeError, ValueError):
        log.warning("config.invalid_int_env", key=key, value=raw, default=default)
        return default


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="", case_sensitive=False)

    port: int = 7020
    log_level: str = "INFO"

    # Postgres `ocr` database (00-create-dbs.sql / 31-ocr-schema.sql).
    # Accepts OCR_DATABASE_URL (preferred, .env.example) or DATABASE_URL, or
    # PG_DSN + PG_DATABASE (root-compose convention).
    database_url: str = Field(
        default="", validation_alias=AliasChoices("OCR_DATABASE_URL", "DATABASE_URL")
    )
    pg_dsn: str = ""
    pg_database: str = ""

    # --- MiniCPM-OCR llama.cpp sidecar (infra/compose/ocr.compose.yml) ---
    # OpenAI-compatible base URL of the llama-server sidecar.
    llm_base_url: str = Field(
        default="http://llama-ocr:8080/v1",
        validation_alias=AliasChoices("OCR_LLM_BASE_URL"),
    )
    # Model name sent on /v1/chat/completions (llama-server accepts any
    # string; keep it equal to the provisioned GGUF alias for observability).
    ocr_model: str = Field(default="minicpm-ocr", validation_alias=AliasChoices("OCR_MODEL"))
    # Sidecar call timeout. OCR on CPU is slow (RECON §5: multi-second per
    # page); 120s, NO retries (a retry would double-bill ocr_page).
    ocr_timeout_s: float = Field(default=120.0, validation_alias=AliasChoices("OCR_TIMEOUT_S"))
    # Max PDF pages rasterized per document (ctx 40960 / CPU latency bound).
    ocr_max_pages: int = Field(default=8, validation_alias=AliasChoices("OCR_MAX_PAGES"))

    # DEV ONLY deterministic fixture extraction (fixed vendor/amount/date).
    # Safe default OFF; CRITICAL startup log when on. NEVER set in prod.
    ocr_mock: bool = Field(default=False, validation_alias=AliasChoices("OCR_MOCK"))

    # Upload hard cap: 25 MB (SPEC-W53 sacred constraint).
    ocr_max_bytes: int = Field(
        default=25 * 1024 * 1024, validation_alias=AliasChoices("OCR_MAX_BYTES")
    )

    # --- ClamAV sidecar (SPEC-W59 P1/P9; bare CLAMAV_ prefix, MINIO_* ---
    # shared-indirection precedent; `_S` suffix matches OCR_TIMEOUT_S).
    # Env parity: compose names == these reads, defaults identical,
    # .env.example mirrors (docker-compose.yml ocr block).
    # clamd zINSTREAM TCP host; EMPTY disables scanning entirely (CRITICAL
    # startup log + skipped metric — P10, never a silent gap).
    clamav_host: str = Field(default="clamav", validation_alias=AliasChoices("CLAMAV_HOST"))
    clamav_port: int = Field(default=3310, validation_alias=AliasChoices("CLAMAV_PORT"))
    # Connect + stream + reply total budget (<< the 150s caller budgets).
    clamav_timeout_s: float = Field(default=30.0, validation_alias=AliasChoices("CLAMAV_TIMEOUT_S"))
    # Fail-closed when clamd is unreachable. kyc_* kinds are fail-closed
    # ALWAYS (hard-coded on is_kyc_kind, regardless of this knob — P2);
    # receipt may fail-open ONLY when this is false (never silent: every
    # skipped scan emits the metric + an ops alert).
    clamav_required: bool = Field(default=True, validation_alias=AliasChoices("CLAMAV_REQUIRED"))

    # SPEC-W58 P4/P12: kyc_* retention. Read as RAW STRINGS so an invalid
    # value warns + falls back to the default (voice-agent _env_int idiom)
    # instead of hard-failing boot; the derived int fields below are what
    # the code reads. (The int fields are named so their auto-derived env
    # names do NOT collide with the pinned OCR_* envs.)
    ocr_kyc_ttl_hours_raw: str = Field(
        default="72", validation_alias=AliasChoices("OCR_KYC_TTL_HOURS")
    )
    ocr_kyc_sweep_minutes_raw: str = Field(
        default="15", validation_alias=AliasChoices("OCR_KYC_SWEEP_MINUTES")
    )
    kyc_ttl_hours: int = 72
    kyc_sweep_minutes: int = 15

    # Service-to-service auth (SPEC-W44 K2): X-Internal-Token, constant-time
    # compare, FAIL-CLOSED (503) when unset. Callers (booking-service
    # fieldcapture) carry the SAME value.
    internal_token: str = Field(
        default="", validation_alias=AliasChoices("OCR_INTERNAL_TOKEN")
    )

    # MinIO document storage (PII — tenant-prefixed keys, bucket `documents`
    # created by the lakehouse createbucket job).
    minio_endpoint: str = Field(
        default="minio:9000", validation_alias=AliasChoices("MINIO_ENDPOINT", "OCR_MINIO_ENDPOINT")
    )
    minio_access_key: str = Field(
        default="minioadmin",
        validation_alias=AliasChoices("MINIO_ROOT_USER", "MINIO_ACCESS_KEY"),
    )
    minio_secret_key: str = Field(
        default="minioadmin",
        validation_alias=AliasChoices("MINIO_ROOT_PASSWORD", "MINIO_SECRET_KEY"),
    )
    minio_bucket: str = Field(
        default="documents", validation_alias=AliasChoices("OCR_MINIO_BUCKET")
    )
    minio_secure: bool = Field(default=False, validation_alias=AliasChoices("MINIO_SECURE"))

    # Dapr pubsub (Kafka) for usage metering + OCR domain events.
    dapr_host: str = "daprd-ocr"
    dapr_http_port: int = 3500
    dapr_pubsub_name: str = Field(
        default="pubsub-kafka", validation_alias=AliasChoices("DAPR_PUBSUB_NAME")
    )
    usage_topic: str = Field(
        default="opendesk.usage.events", validation_alias=AliasChoices("OCR_USAGE_TOPIC")
    )
    ocr_events_topic: str = Field(
        default="opendesk.ocr.events", validation_alias=AliasChoices("OCR_EVENTS_TOPIC")
    )

    # Tenant binding (SPEC-W43 C1): X-Tenant-Slugs header binding for
    # gateway-authenticated calls; slugs resolve via identity-service.
    trust_direct_tenant: bool = Field(
        default=False, validation_alias=AliasChoices("OPENDESK_TRUST_DIRECT_TENANT")
    )
    identity_app_id: str = "identity"
    identity_internal_token: str = Field(
        default="", validation_alias=AliasChoices("IDENTITY_INTERNAL_TOKEN")
    )

    @model_validator(mode="after")
    def _derive_kyc_settings(self) -> "Settings":
        self.kyc_ttl_hours = _int_or_default(
            "OCR_KYC_TTL_HOURS", self.ocr_kyc_ttl_hours_raw, 72
        )
        self.kyc_sweep_minutes = _int_or_default(
            "OCR_KYC_SWEEP_MINUTES", self.ocr_kyc_sweep_minutes_raw, 15
        )
        return self

    @model_validator(mode="after")
    def _compose_database_url(self) -> "Settings":
        if not self.database_url:
            if self.pg_dsn and self.pg_database:
                self.database_url = f"{self.pg_dsn.rstrip('/')}/{self.pg_database}"
            else:
                self.database_url = (
                    "postgres://app_ocr_login:app_ocr_dev_password@postgres:5432/ocr"
                )
        return self


settings = Settings()
