"""Environment-driven settings (see README.md env table)."""

from __future__ import annotations

import os
from dataclasses import dataclass, field

from .multilang import parse_tts_voice_map, parse_voice_map
from .sip import parse_tenant_phone_map


def _env(key: str, default: str) -> str:
    return os.environ.get(key, default)


def _env_int(key: str, default: int) -> int:
    try:
        return int(os.environ.get(key, str(default)))
    except ValueError:
        return default


@dataclass(frozen=True)
class Settings:
    # Control plane
    port: int = 7006
    log_level: str = "info"

    # Dapr sidecar (SPEC §3: companion daprd container, app-id `voice`)
    dapr_host: str = "daprd-voice"
    dapr_http_port: int = 3500
    dapr_pubsub: str = "pubsub-kafka"
    booking_app_id: str = "booking"
    identity_app_id: str = "identity"
    knowledge_app_id: str = "knowledge"
    booking_commands_topic: str = "opendesk.booking.commands"
    conversation_events_topic: str = "opendesk.conversation.events"

    # LiveKit (SPEC §11: dev keys on 7880)
    livekit_url: str = "ws://livekit:7880"
    livekit_api_key: str = "devkey"
    livekit_api_secret: str = "secret"

    # LLM via OpenAI-compatible endpoint (Ollama default; vLLM/hosted pluggable).
    # Open-model-first (SPEC-W3 §0): default is qwen3:8b on Ollama; the
    # MiniMax-M2 long-context path uses LLM_BASE_URL=https://api.minimax.io/v1
    # + LLM_MODEL=MiniMax-M2 + LLM_API_KEY (see README model-routing table).
    llm_base_url: str = "http://ollama:11434/v1"
    llm_model: str = "qwen3:8b"
    llm_api_key: str = "ollama"  # Ollama ignores the key; hosted providers require one
    llm_timeout_s: float = 20.0  # per-call timeout; exceeding it trips fallback
    # LLM fallback chain (VOICE-SCALING §3): secondary OpenAI-compatible
    # endpoint used when the primary fails (connection error, 429, 5xx,
    # timeout). Empty base URL disables the chain.
    llm_fallback_base_url: str = ""
    llm_fallback_model: str = ""
    llm_fallback_api_key: str = ""
    llm_cb_failures: int = 3  # consecutive primary failures before circuit opens
    llm_cb_cooldown_s: float = 60.0  # fallback-only window before probing primary

    # STT (faster-whisper, in-process, lazy-loaded)
    whisper_model: str = "base"
    whisper_device: str = "auto"  # auto|cpu|cuda
    whisper_compute_type: str = "int8"

    # TTS (piper): http sidecar or local subprocess
    piper_mode: str = "http"  # http|subprocess
    piper_http_url: str = "http://piper:5500"
    piper_voice: str = "en_US-lessac-medium"
    piper_bin: str = "piper"
    piper_model_dir: str = "/voices"
    piper_sample_rate: int = 22050

    # Agent backend abstraction (SPEC §11: optional ElevenLabs adapter)
    agent_backend: str = "livekit"  # livekit|elevenlabs
    elevenlabs_api_key: str = ""
    elevenlabs_agent_id: str = ""

    # Avatar presence (SPEC-W9 Part A, app/avatar/): which provider joins a
    # visual avatar into the LiveKit room after session mint. `none` (default)
    # keeps sessions audio-only; per-tenant override via tenant context
    # `avatar_provider` (defensive getattr) when present. Joining is
    # fire-and-forget — failures never block session creation.
    avatar_provider: str = "none"  # none|tavus|musetalk
    # Tavus CVI (hosted) provider settings.
    tavus_api_key: str = ""
    tavus_replica_id: str = ""
    tavus_persona_id: str = ""
    # Open lip-sync path: avatar-renderer sidecar (services/avatar-renderer).
    # avatar_renderer=enabled means the sidecar is deployed and will join
    # `site-*` rooms; musetalk_room_agent publishes the join intent per
    # session. avatar_renderer_mode is informational here (the sidecar reads
    # its own env) and logged with the intent.
    avatar_renderer: str = "disabled"  # disabled|enabled
    avatar_renderer_mode: str = "mock"  # mock|musetalk
    musetalk_room_agent: bool = False

    # Session bootstrap
    knowledge_snippet_count: int = 3
    knowledge_query: str = "opening hours services pricing"
    http_timeout_s: float = 15.0

    # Worker plane (VOICE-SCALING §2): prewarming + load gating.
    preload_models: bool = True  # eager whisper load + piper warmup at boot
    agent_idle_processes: int = 2  # num_idle_processes warm job processes
    load_threshold: float = 0.7  # CPU load (0..1) above which worker stops taking jobs

    # Async tools (VOICE-SCALING §5): hard timeout per Dapr tool call and the
    # grace window before the filler ack line is spoken (fast calls skip it).
    tool_timeout_s: float = 4.0
    tool_ack_grace_ms: int = 400

    # Phone-confirmation policy
    phone_confirmation_required: bool = True

    # Warm handoff / whisper-copilot (SPEC-W3 §4, innovation 1): after an
    # escalation the agent keeps drafting suggested replies into the
    # escalation room data channel.
    copilot_mode: bool = True

    # Plugin tools (SPEC-W3 §4, innovation 15): SSRF guard — comma-separated
    # allowlist of hosts pack customTools may call.
    plugin_allowed_hosts: str = "booking,knowledge,identity"

    # Voice biometrics scaffold (SPEC-W3 §4, innovation 2): consent gate,
    # default OFF. Not wired into the audio pipeline (see README).
    voiceprints: bool = False
    voiceprint_threshold: float = 0.75

    # SIP telephony inbound (Wave 5 #1, app/sip.py): dialed-number -> tenant
    # map (TENANT_PHONE_MAP JSON, dev-mode; production = phone_numbers table)
    # and the fallback site when the dialed number is unmapped (empty =
    # reject the call bootstrap with an error log).
    tenant_phone_map: dict = field(default_factory=dict)
    sip_default_site: str = ""

    # Agents registry (SPEC-W38 F1, app/agents_registry.py): dialed-number ->
    # agent resolution via conversation-service (internal, no APISIX). The
    # static TENANT_PHONE_MAP above remains the fallback when the registry
    # misses or is unreachable (fail-open). Empty URL disables the registry
    # path; AGENTS_CACHE_TTL_S bounds the in-process resolve cache.
    agents_registry_url: str = "http://conversation:7007"
    agents_cache_ttl_s: int = 30

    # Multilingual receptionist (Wave 5 #3, app/multilang.py): language ->
    # piper voice map (PIPER_VOICE_MAP JSON). Languages without an entry fall
    # back to `piper_voice`.
    piper_voice_map: dict = field(default_factory=dict)

    # TTS provider chain (SPEC-W10 Part A, app/tts_providers/): ordered
    # comma-separated provider fallback chain (TTS_PROVIDER_CHAIN). Default
    # "piper" = byte-identical pre-W10 behavior; piper is always the implicit
    # last resort when configured. TTS_VOICE_MAP JSON maps language tags to
    # provider-qualified "provider:voiceId" values (consulted BEFORE
    # piper_voice_map; see app/multilang.py resolve_tts_voice).
    tts_provider_chain: str = "piper"
    tts_voice_map: dict = field(default_factory=dict)
    mms_tts_url: str = "http://mms-tts:5800"
    xtts_tts_url: str = "http://xtts-tts:5810"
    azure_speech_key: str = ""
    azure_speech_region: str = ""
    spitch_api_key: str = ""
    spitch_base_url: str = "https://api.spitch.app"
    tts_cb_failures: int = 3  # consecutive provider failures before circuit opens
    tts_cb_cooldown_s: float = 60.0  # open window before a half-open probe

    # A/B prompt testing (Wave 5 #8, eval/ab_test.py): allow POST /voice/chat
    # to carry a `persona_override` replacing the tenant persona. OFF by
    # default — enabling it on a public endpoint is a prompt-injection
    # surface; only turn on for eval runs.
    eval_persona_override: bool = False

    extra: dict = field(default_factory=dict)

    @property
    def dapr_base_url(self) -> str:
        return f"http://{self.dapr_host}:{self.dapr_http_port}"


def load_settings() -> Settings:
    return Settings(
        port=_env_int("PORT", 7006),
        log_level=_env("LOG_LEVEL", "info"),
        dapr_host=_env("DAPR_HOST", "daprd-voice"),
        dapr_http_port=_env_int("DAPR_HTTP_PORT", 3500),
        dapr_pubsub=_env("DAPR_PUBSUB_NAME", "pubsub-kafka"),
        booking_app_id=_env("BOOKING_APP_ID", "booking"),
        identity_app_id=_env("IDENTITY_APP_ID", "identity"),
        knowledge_app_id=_env("KNOWLEDGE_APP_ID", "knowledge"),
        booking_commands_topic=_env("BOOKING_COMMANDS_TOPIC", "opendesk.booking.commands"),
        conversation_events_topic=_env(
            "CONVERSATION_EVENTS_TOPIC", "opendesk.conversation.events"
        ),
        livekit_url=_env("LIVEKIT_URL", "ws://livekit:7880"),
        livekit_api_key=_env("LIVEKIT_API_KEY", "devkey"),
        livekit_api_secret=_env("LIVEKIT_API_SECRET", "secret"),
        llm_base_url=_env("LLM_BASE_URL", "http://ollama:11434/v1"),
        llm_model=_env("LLM_MODEL", "qwen3:8b"),
        # Optional pass-through to the OpenAI-compatible client: Ollama
        # ignores it, hosted providers (e.g. MiniMax) require it.
        llm_api_key=_env("LLM_API_KEY", "ollama"),
        llm_timeout_s=float(os.environ.get("LLM_TIMEOUT", "20")),
        llm_fallback_base_url=_env("LLM_FALLBACK_BASE_URL", ""),
        llm_fallback_model=_env("LLM_FALLBACK_MODEL", ""),
        llm_fallback_api_key=_env("LLM_FALLBACK_API_KEY", ""),
        llm_cb_failures=_env_int("LLM_CB_FAILURES", 3),
        llm_cb_cooldown_s=float(os.environ.get("LLM_CB_COOLDOWN_S", "60")),
        whisper_model=_env("WHISPER_MODEL", "base"),
        whisper_device=_env("WHISPER_DEVICE", "auto"),
        whisper_compute_type=_env("WHISPER_COMPUTE_TYPE", "int8"),
        piper_mode=_env("PIPER_MODE", "http"),
        piper_http_url=_env("PIPER_HTTP_URL", "http://piper:5500"),
        piper_voice=_env("PIPER_VOICE", "en_US-lessac-medium"),
        piper_bin=_env("PIPER_BIN", "piper"),
        piper_model_dir=_env("PIPER_MODEL_DIR", "/voices"),
        piper_sample_rate=_env_int("PIPER_SAMPLE_RATE", 22050),
        agent_backend=_env("AGENT_BACKEND", "livekit"),
        elevenlabs_api_key=_env("ELEVENLABS_API_KEY", ""),
        elevenlabs_agent_id=_env("ELEVENLABS_AGENT_ID", ""),
        avatar_provider=_env("AVATAR_PROVIDER", "none"),
        tavus_api_key=_env("TAVUS_API_KEY", ""),
        tavus_replica_id=_env("TAVUS_REPLICA_ID", ""),
        tavus_persona_id=_env("TAVUS_PERSONA_ID", ""),
        avatar_renderer=_env("AVATAR_RENDERER", "disabled"),
        avatar_renderer_mode=_env("AVATAR_RENDERER_MODE", "mock"),
        # SPEC spells this flag MUSEtalk_ROOM_AGENT; honor both casings.
        musetalk_room_agent=_env(
            "MUSETALK_ROOM_AGENT", _env("MUSEtalk_ROOM_AGENT", "false")
        ).lower()
        in ("1", "true", "yes", "on"),
        knowledge_snippet_count=_env_int("KNOWLEDGE_SNIPPET_COUNT", 3),
        knowledge_query=_env("KNOWLEDGE_QUERY", "opening hours services pricing"),
        http_timeout_s=float(os.environ.get("HTTP_TIMEOUT_S", "15")),
        preload_models=_env("PRELOAD_MODELS", "true").lower() not in ("0", "false", "no"),
        agent_idle_processes=_env_int("AGENT_IDLE_PROCESSES", 2),
        load_threshold=float(os.environ.get("LOAD_THRESHOLD", "0.7")),
        tool_timeout_s=float(os.environ.get("TOOL_TIMEOUT_SECONDS", "4")),
        tool_ack_grace_ms=_env_int("TOOL_ACK_GRACE_MS", 400),
        phone_confirmation_required=_env("PHONE_CONFIRMATION_REQUIRED", "true").lower()
        not in ("0", "false", "no"),
        copilot_mode=_env("COPILOT_MODE", "true").lower() not in ("0", "false", "no"),
        plugin_allowed_hosts=_env(
            "PLUGIN_ALLOWED_HOSTS", "booking,knowledge,identity"
        ),
        voiceprints=_env("VOICEPRINTS", "off").lower() in ("1", "on", "true", "yes"),
        voiceprint_threshold=float(os.environ.get("VOICEPRINT_THRESHOLD", "0.75")),
        tenant_phone_map=parse_tenant_phone_map(_env("TENANT_PHONE_MAP", "")),
        sip_default_site=_env("SIP_DEFAULT_SITE", ""),
        agents_registry_url=_env("AGENTS_REGISTRY_URL", "http://conversation:7007"),
        agents_cache_ttl_s=_env_int("AGENTS_CACHE_TTL_S", 30),
        piper_voice_map=parse_voice_map(_env("PIPER_VOICE_MAP", "")),
        tts_provider_chain=_env("TTS_PROVIDER_CHAIN", "piper"),
        tts_voice_map=parse_tts_voice_map(_env("TTS_VOICE_MAP", "")),
        mms_tts_url=_env("MMS_TTS_URL", "http://mms-tts:5800"),
        xtts_tts_url=_env("XTTS_TTS_URL", "http://xtts-tts:5810"),
        azure_speech_key=_env("AZURE_SPEECH_KEY", ""),
        azure_speech_region=_env("AZURE_SPEECH_REGION", ""),
        spitch_api_key=_env("SPITCH_API_KEY", ""),
        spitch_base_url=_env("SPITCH_BASE_URL", "https://api.spitch.app"),
        tts_cb_failures=_env_int("TTS_CB_FAILURES", 3),
        tts_cb_cooldown_s=float(os.environ.get("TTS_CB_COOLDOWN_S", "60")),
        eval_persona_override=_env("EVAL_PERSONA_OVERRIDE", "false").lower()
        in ("1", "true", "yes", "on"),
    )
