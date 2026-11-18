"""Environment-driven settings (see README.md env table)."""

from __future__ import annotations

import json
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


def _env_float(key: str, default: float) -> float:
    """Float env with fallback-to-default on bad values (SPEC-W70 A1: a typo
    in a latency knob must not crash the worker boot)."""
    try:
        return float(os.environ.get(key, str(default)))
    except (TypeError, ValueError):
        return default


@dataclass(frozen=True)
class Settings:
    # Control plane
    port: int = 7006
    log_level: str = "info"
    # W44/F15-09: the LiveKit worker PROCESS (app/livekit_worker.py) serves
    # the app/metrics.py registry exposition on its own port so Prometheus
    # can scrape the per-call voice_* series the dashboards query (the
    # control-plane /metrics on `port` is a different process). 0 disables.
    worker_metrics_port: int = 9464

    # Dapr sidecar (SPEC §3: companion daprd container, app-id `voice`)
    dapr_host: str = "daprd-voice"
    dapr_http_port: int = 3500
    dapr_pubsub: str = "pubsub-kafka"
    booking_app_id: str = "booking"
    identity_app_id: str = "identity"
    # SPEC-W44 K2: X-Internal-Token sent on the identity tenant-context
    # lookup (IDENTITY_INTERNAL_TOKEN env; empty = header not sent —
    # identity fails closed on its own side when unset).
    identity_internal_token: str = ""
    knowledge_app_id: str = "knowledge"
    booking_commands_topic: str = "opendesk.booking.commands"
    conversation_events_topic: str = "opendesk.conversation.events"

    # LiveKit (SPEC §11: dev keys on 7880)
    livekit_url: str = "ws://livekit:7880"
    # What browsers dial (wss through the public edge); livekit_url stays the in-cluster address for workers.
    livekit_public_url: str = ""
    # Cloudflare Realtime TURN (app/ice.py): per-session relay credentials for browsers when LiveKit takes no
    # inbound UDP. Empty = no TURN offered.
    cf_turn_key_id: str = ""
    cf_turn_api_token: str = ""
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
    # Extra JSON merged into every chat-completions body (LLM_EXTRA_BODY), e.g. {"reasoning_effort": "none"} so
    # a thinking model answers a caller straight away instead of reasoning first.
    llm_extra_body: dict = field(default_factory=dict)
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
    # SPEC-W70 A2: optional second whisper model (e.g. large-v3-turbo)
    # lazy-loaded alongside the default and used ONLY when the effective
    # language (forced language= or last auto-detection) is non-English —
    # the accuracy tier for yo/ig/ha. Empty = off (single-model behavior).
    whisper_model_multilingual: str = ""
    # SPEC-W71 P3: CTranslate2 thread tuning, passed to every WhisperModel
    # construction (default + multilingual override). whisper_cpu_threads
    # (WHISPER_CPU_THREADS, default 0 = CTranslate2 auto) sets the threads
    # per worker; whisper_num_workers (WHISPER_NUM_WORKERS, default 1) sets
    # the parallel decode workers. Both are throughput/latency levers on
    # multi-core hosts; defaults preserve pre-W71 behavior exactly.
    whisper_cpu_threads: int = 0
    whisper_num_workers: int = 1

    # SPEC-W70 A1: turn-taking / endpointing latency tuning, wired into
    # VoicePipelineAgent + silero.VAD.load via signature introspection
    # (app/livekit_worker.py) so the pinned livekit-agents==0.10.* contract
    # stays safe. Defaults deliberately improve responsiveness over the
    # framework defaults (0.5s endpointing / 0.55s VAD silence / preemptive
    # synthesis off) with zero configuration. Knob names verified against
    # the livekit_agents-0.10.2 wheel (min_endpointing_delay and
    # preemptive_synthesis are the real VoicePipelineAgent parameters;
    # 0.10.x has no max-endpointing knob).
    voice_min_endpointing_delay: float = 0.3
    voice_vad_min_silence_s: float = 0.35
    voice_preemptive_synthesis: bool = True
    # SPEC-W71 P7: LLM response-length cap (VOICE_LLM_MAX_TOKENS, default
    # 300, 0 disables) applied ONLY on our own OpenAI-compatible client
    # paths (app/pipeline/llm.py chat_with_tools / stream_with_tools) —
    # spoken replies are bounded, which bounds LLM decode time and
    # time-to-first-TTS per turn. The livekit-plugins-openai node is NOT
    # touched (0.10.x has no such param — W70 lesson).
    voice_llm_max_tokens: int = 300
    # SPEC-W71 P8: chat-history windowing (VOICE_LLM_HISTORY_MAX, default 60
    # messages, 0 disables). The in-call chat_ctx is trimmed in place after
    # each agent reply commits, at safe boundaries only (never between an
    # assistant tool_calls message and its tool results; the system message
    # is always retained) — unbounded history inflates every turn's TTFT.
    voice_llm_history_max: int = 60
    # SPEC-W48-C: streaming STT endpoint (/v1/stt/stream, WebSocket).
    # Rolling-window chunked inference: window_s of audio re-transcribed
    # every hop_s; an utterance endpoints after silence_ms of trailing
    # silence. Undrained client input is bounded at max_buffer_s of audio
    # (backpressure -> close 1008).
    stt_stream_window_s: float = 1.0
    stt_stream_hop_s: float = 0.25
    stt_stream_silence_ms: int = 700
    stt_stream_max_buffer_s: float = 8.0

    # SPEC-W73 A1: dead-mic detection (app/dead_mic.py). Per-session
    # asyncio watchdog started after the greeting is queued: no user speech
    # within voice_dead_mic_first_prompt_s -> a short "check your
    # microphone" re-prompt is spoken; still silent after
    # voice_dead_mic_second_prompt_s -> polite closer + the session is
    # ended via room disconnect when voice_dead_mic_hangup_enabled. ANY
    # user speech activity resets the watchdog to zero. Defaults are ON but
    # conservative (12s/10s), preserving pre-W73 session behavior.
    voice_dead_mic_enabled: bool = True
    voice_dead_mic_first_prompt_s: float = 12.0
    voice_dead_mic_second_prompt_s: float = 10.0
    voice_dead_mic_hangup_enabled: bool = True

    # SPEC-W73 A2: pre-rendered greetings (app/greeting_prerender.py). At
    # worker boot (and in each warm job process) the greeting of every
    # tenant slug in voice_greeting_prerender_tenants (csv) is synthesized
    # through the real TTS chain so the session-path greeting is a TTS LRU
    # cache hit. Empty tenant list = boot prerender is a no-op (honest
    # fallback: there is no tenant-enumeration seam to reuse). Per-tenant
    # failures are non-fatal; total boot delay is bounded by
    # voice_greeting_prerender_timeout_s (on timeout the worker boots with
    # the partially warmed cache).
    voice_greeting_prerender_enabled: bool = True
    voice_greeting_prerender_tenants: str = ""
    voice_greeting_prerender_concurrency: int = 4
    voice_greeting_prerender_timeout_s: float = 30.0

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

    # SPEC-W46 P1/P2: tenant-context TTL cache (VOICE_TENANT_CTX_TTL_S,
    # default 120s) keyed by site_slug, shared by the chat path and the
    # ElevenLabs tool webhook — the 3-invoke bootstrap no longer runs on
    # every turn/tool call. On a refresh failure a stale entry is served
    # for up to tenant_ctx_stale_s (stale-while-error, default 300s); when
    # no usable entry exists the fetch error propagates exactly as before
    # (fail-closed session bootstrap preserved).
    tenant_ctx_ttl_s: int = 120
    tenant_ctx_stale_s: int = 300

    # SPEC-W46 P-02: direct booking-service base URL for the tool/tenant
    # invoke paths (mirrors messaging-gateway ResolveInvokeBase): calls to
    # app-id `booking` go straight to the compose service address instead
    # of voice->daprd->booking; a transport failure falls back to the daprd
    # invoke API. Empty disables the direct path (pure daprd). Default is
    # the in-cluster compose address (service `booking`, port 7002 — same
    # target as BOOKING_URL used by the K15(c) OTP verifier).
    booking_base_url: str = "http://booking:7002"

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

    # SPEC-W45 K15(c) caller verification (OTP): mutating tools
    # (lookup/reschedule/cancel) require a VERIFIED session. Verification
    # goes through the booking customer-portal magic-code endpoints
    # (POST {BOOKING_URL}/public/sites/{slug}/portal/request + /portal/verify)
    # with X-Internal-Token (VOICE_BOOKING_INTERNAL_TOKEN, K2 pattern).
    # FAIL-CLOSED: when either is unset the verification tools answer
    # "verification_unavailable" (honest 503-equivalent on the tool path)
    # and the mutating tools refuse — no fallback to self-asserted numbers.
    booking_url: str = ""
    voice_booking_internal_token: str = ""

    # SPEC-W45 K15(d) + verifier F-2: /voice-admin/* staff paths
    # (brand-voice enrollment, escalation staff-token mint) are guarded by
    # _require_admin_access — EITHER X-Internal-Token matching
    # VOICE_ADMIN_INTERNAL_TOKEN (constant-time; 503 fail-closed when unset
    # and no other auth applies, 401 on mismatch) OR the gateway-injected
    # X-User-Roles carrying staff|admin|platform-admin (APISIX
    # api-voice-admin route: OIDC + role gate + K1 header injection; the
    # gateway strips client-supplied x-internal-token AND x-user-roles, so
    # only in-cluster callers can present the token and only the gateway
    # can mint the role header — see control_plane for the trust-boundary
    # caveat). The same token also authorizes channel-identity pinning on
    # /voice/chat (K15(c): the messaging-gateway asserts provider-verified
    # identities such as the WhatsApp wa_id).
    voice_admin_internal_token: str = ""

    # SPEC-W45 K15(f): HMAC-SHA256 key for hashing caller phone numbers in
    # ToolInvoked / capture_location events (W28 scheme: tenant-bound,
    # digits-normalized; graph-sync graph.PhoneHash family). Empty = the
    # phone is OMITTED from events entirely (never emitted in plaintext,
    # warning logged).
    phone_hash_salt: str = ""

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
    # conversation-service base for transcripts (app/transcripts.py); "" disables recording.
    conversation_url: str = "http://conversation:7007"
    agents_cache_ttl_s: int = 30

    # Multilingual receptionist (Wave 5 #3, app/multilang.py): language ->
    # piper voice map (PIPER_VOICE_MAP JSON). Languages without an entry fall
    # back to `piper_voice`.
    piper_voice_map: dict = field(default_factory=dict)

    # SPEC-W46 P4: TTS result cache + chunked streaming. tts_cache_size
    # (VOICE_TTS_CACHE_SIZE, 0 disables) bounds the per-process LRU keyed
    # (voice, text, format); tts_chunked (VOICE_TTS_CHUNKED=false restores
    # the pre-W46 full-buffer behavior) streams sentence-level chunks so the
    # first audio frame ships after the first chunk's synthesis.
    tts_cache_size: int = 256
    tts_chunked: bool = True

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
    # Nigerian-language speech on the GPU host (services/naija-speech): YarnGPT voices for ig/yo/ha, MMS for pcm/ful,
    # MMS speech recognition for all. Empty = callers can't pick a Nigerian language (English only).
    naija_speech_url: str = ""
    # Languages a caller may choose on the call page (ISO codes; en always allowed).
    call_languages: str = "en,pcm,yo,ha,ig"
    # Follow a caller who changes language mid-call. Off: the language picked on the call page holds for the whole
    # call (real Nigerian-accented English was taken for Yoruba at 0.976 and the receptionist switched languages).
    voice_auto_language_switch: bool = True
    # Nigerian-accented English voice on the naija-speech service (a YarnGPT English speaker, e.g. "idera"). Set: the
    # receptionist speaks English and Pidgin in it instead of piper (piper stays the fallback). Empty = piper/MMS.
    naija_english_voice: str = ""
    # Per-language naija-speech voices for the other Nigerian languages, "yo=yoruba_male1,ig=igbo_male2" (unset
    # languages: the service's default voice for that language).
    naija_language_voices: dict = field(default_factory=dict)
    cosyvoice_tts_url: str = "http://cosyvoice-tts:5820"
    # SPEC-W74 A1: Qwen3-TTS (nari-labs/nari-qwen3-tts) GPU-tier provider —
    # the published image IS the sidecar (OpenAI-shaped /v1/audio/speech).
    # Requires an NVIDIA H100; engine tested with English as the primary
    # language ONLY (NOT a Nigerian-language path — see
    # docs/voices-sidecars.md + docs/voice-licensing.md). Opt-in via
    # TTS_PROVIDER_CHAIN + the "voice-gpu" compose profile.
    qwen3_tts_url: str = "http://qwen3-tts:8000"
    qwen3_tts_model: str = "Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice"
    qwen3_tts_voice: str = "ryan"
    qwen3_tts_language: str = "english"
    qwen3_tts_timeout_s: float = 30.0
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


def parse_naija_language_voices(raw: str) -> dict[str, str]:
    """"yo=yoruba_male1, ig=igbo_male2" -> {"yo": "yoruba_male1", "ig": "igbo_male2"}; malformed entries are skipped."""
    out: dict[str, str] = {}
    for part in (raw or "").split(","):
        lang, sep, voice = part.partition("=")
        if sep and lang.strip() and voice.strip():
            out[lang.strip().lower()] = voice.strip()
    return out


def load_settings() -> Settings:
    return Settings(
        port=_env_int("PORT", 7006),
        log_level=_env("LOG_LEVEL", "info"),
        worker_metrics_port=_env_int("VOICE_WORKER_METRICS_PORT", 9464),
        dapr_host=_env("DAPR_HOST", "daprd-voice"),
        dapr_http_port=_env_int("DAPR_HTTP_PORT", 3500),
        dapr_pubsub=_env("DAPR_PUBSUB_NAME", "pubsub-kafka"),
        booking_app_id=_env("BOOKING_APP_ID", "booking"),
        identity_app_id=_env("IDENTITY_APP_ID", "identity"),
        identity_internal_token=_env("IDENTITY_INTERNAL_TOKEN", ""),
        knowledge_app_id=_env("KNOWLEDGE_APP_ID", "knowledge"),
        booking_commands_topic=_env("BOOKING_COMMANDS_TOPIC", "opendesk.booking.commands"),
        conversation_events_topic=_env(
            "CONVERSATION_EVENTS_TOPIC", "opendesk.conversation.events"
        ),
        livekit_url=_env("LIVEKIT_URL", "ws://livekit:7880"),
        livekit_public_url=_env("LIVEKIT_PUBLIC_URL", ""),
        cf_turn_key_id=_env("CF_TURN_KEY_ID", ""),
        cf_turn_api_token=_env("CF_TURN_API_TOKEN", ""),
        livekit_api_key=_env("LIVEKIT_API_KEY", "devkey"),
        livekit_api_secret=_env("LIVEKIT_API_SECRET", "secret"),
        llm_base_url=_env("LLM_BASE_URL", "http://ollama:11434/v1"),
        llm_model=_env("LLM_MODEL", "qwen3:8b"),
        # Optional pass-through to the OpenAI-compatible client: Ollama
        # ignores it, hosted providers (e.g. MiniMax) require it.
        llm_api_key=_env("LLM_API_KEY", "ollama"),
        llm_timeout_s=float(os.environ.get("LLM_TIMEOUT", "20")),
        llm_extra_body=json.loads(os.environ.get("LLM_EXTRA_BODY") or "{}"),
        llm_fallback_base_url=_env("LLM_FALLBACK_BASE_URL", ""),
        llm_fallback_model=_env("LLM_FALLBACK_MODEL", ""),
        llm_fallback_api_key=_env("LLM_FALLBACK_API_KEY", ""),
        llm_cb_failures=_env_int("LLM_CB_FAILURES", 3),
        llm_cb_cooldown_s=float(os.environ.get("LLM_CB_COOLDOWN_S", "60")),
        whisper_model=_env("WHISPER_MODEL", "base"),
        whisper_device=_env("WHISPER_DEVICE", "auto"),
        whisper_compute_type=_env("WHISPER_COMPUTE_TYPE", "int8"),
        whisper_model_multilingual=_env("WHISPER_MODEL_MULTILINGUAL", "").strip(),
        whisper_cpu_threads=_env_int("WHISPER_CPU_THREADS", 0),
        whisper_num_workers=_env_int("WHISPER_NUM_WORKERS", 1),
        voice_min_endpointing_delay=_env_float("VOICE_MIN_ENDPOINTING_DELAY", 0.3),
        voice_vad_min_silence_s=_env_float("VOICE_VAD_MIN_SILENCE_S", 0.35),
        voice_preemptive_synthesis=_env("VOICE_PREEMPTIVE_SYNTHESIS", "true").lower()
        not in ("0", "false", "no"),
        voice_llm_max_tokens=_env_int("VOICE_LLM_MAX_TOKENS", 300),
        voice_llm_history_max=_env_int("VOICE_LLM_HISTORY_MAX", 60),
        stt_stream_window_s=float(os.environ.get("STT_STREAM_WINDOW_S", "1.0")),
        stt_stream_hop_s=float(os.environ.get("STT_STREAM_HOP_S", "0.25")),
        stt_stream_silence_ms=_env_int("STT_STREAM_SILENCE_MS", 700),
        stt_stream_max_buffer_s=float(os.environ.get("STT_STREAM_MAX_BUFFER_S", "8")),
        voice_dead_mic_enabled=_env("VOICE_DEAD_MIC_ENABLED", "1").lower()
        not in ("0", "false", "no"),
        voice_dead_mic_first_prompt_s=_env_float("VOICE_DEAD_MIC_FIRST_PROMPT_S", 12.0),
        voice_dead_mic_second_prompt_s=_env_float("VOICE_DEAD_MIC_SECOND_PROMPT_S", 10.0),
        voice_dead_mic_hangup_enabled=_env("VOICE_DEAD_MIC_HANGUP_ENABLED", "1").lower()
        not in ("0", "false", "no"),
        voice_greeting_prerender_enabled=_env(
            "VOICE_GREETING_PRERENDER_ENABLED", "1"
        ).lower()
        not in ("0", "false", "no"),
        voice_greeting_prerender_tenants=_env("VOICE_GREETING_PRERENDER_TENANTS", ""),
        voice_greeting_prerender_concurrency=_env_int(
            "VOICE_GREETING_PRERENDER_CONCURRENCY", 4
        ),
        voice_greeting_prerender_timeout_s=_env_float(
            "VOICE_GREETING_PRERENDER_TIMEOUT_S", 30.0
        ),
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
        tenant_ctx_ttl_s=_env_int("VOICE_TENANT_CTX_TTL_S", 120),
        tenant_ctx_stale_s=_env_int("VOICE_TENANT_CTX_STALE_S", 300),
        booking_base_url=_env("BOOKING_BASE_URL", "http://booking:7002"),
        preload_models=_env("PRELOAD_MODELS", "true").lower() not in ("0", "false", "no"),
        agent_idle_processes=_env_int("AGENT_IDLE_PROCESSES", 2),
        load_threshold=float(os.environ.get("LOAD_THRESHOLD", "0.7")),
        tool_timeout_s=float(os.environ.get("TOOL_TIMEOUT_SECONDS", "4")),
        tool_ack_grace_ms=_env_int("TOOL_ACK_GRACE_MS", 400),
        phone_confirmation_required=_env("PHONE_CONFIRMATION_REQUIRED", "true").lower()
        not in ("0", "false", "no"),
        booking_url=_env("BOOKING_URL", ""),
        voice_booking_internal_token=_env("VOICE_BOOKING_INTERNAL_TOKEN", ""),
        voice_admin_internal_token=_env("VOICE_ADMIN_INTERNAL_TOKEN", ""),
        phone_hash_salt=_env("PHONE_HASH_SALT", ""),
        copilot_mode=_env("COPILOT_MODE", "true").lower() not in ("0", "false", "no"),
        plugin_allowed_hosts=_env(
            "PLUGIN_ALLOWED_HOSTS", "booking,knowledge,identity"
        ),
        voiceprints=_env("VOICEPRINTS", "off").lower() in ("1", "on", "true", "yes"),
        voiceprint_threshold=float(os.environ.get("VOICEPRINT_THRESHOLD", "0.75")),
        tenant_phone_map=parse_tenant_phone_map(_env("TENANT_PHONE_MAP", "")),
        sip_default_site=_env("SIP_DEFAULT_SITE", ""),
        agents_registry_url=_env("AGENTS_REGISTRY_URL", "http://conversation:7007"),
        conversation_url=_env("CONVERSATION_URL", "http://conversation:7007"),
        agents_cache_ttl_s=_env_int("AGENTS_CACHE_TTL_S", 30),
        piper_voice_map=parse_voice_map(_env("PIPER_VOICE_MAP", "")),
        tts_cache_size=_env_int("VOICE_TTS_CACHE_SIZE", 256),
        tts_chunked=_env("VOICE_TTS_CHUNKED", "true").lower()
        not in ("0", "false", "no"),
        tts_provider_chain=_env("TTS_PROVIDER_CHAIN", "piper"),
        tts_voice_map=parse_tts_voice_map(_env("TTS_VOICE_MAP", "")),
        mms_tts_url=_env("MMS_TTS_URL", "http://mms-tts:5800"),
        xtts_tts_url=_env("XTTS_TTS_URL", "http://xtts-tts:5810"),
        naija_speech_url=_env("NAIJA_SPEECH_URL", ""),
        call_languages=_env("CALL_LANGUAGES", "en,pcm,yo,ha,ig"),
        naija_english_voice=_env("NAIJA_ENGLISH_VOICE", "").strip(),
        voice_auto_language_switch=_env("VOICE_AUTO_LANGUAGE_SWITCH", "1").lower() in ("1", "true", "yes", "on"),
        naija_language_voices=parse_naija_language_voices(_env("NAIJA_LANGUAGE_VOICES", "")),
        cosyvoice_tts_url=_env("COSYVOICE_TTS_URL", "http://cosyvoice-tts:5820"),
        qwen3_tts_url=_env("QWEN3_TTS_URL", "http://qwen3-tts:8000"),
        qwen3_tts_model=_env("QWEN3_TTS_MODEL", "Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice"),
        qwen3_tts_voice=_env("QWEN3_TTS_VOICE", "ryan"),
        qwen3_tts_language=_env("QWEN3_TTS_LANGUAGE", "english"),
        qwen3_tts_timeout_s=_env_float("QWEN3_TTS_TIMEOUT_S", 30.0),
        azure_speech_key=_env("AZURE_SPEECH_KEY", ""),
        azure_speech_region=_env("AZURE_SPEECH_REGION", ""),
        spitch_api_key=_env("SPITCH_API_KEY", ""),
        spitch_base_url=_env("SPITCH_BASE_URL", "https://api.spitch.app"),
        tts_cb_failures=_env_int("TTS_CB_FAILURES", 3),
        tts_cb_cooldown_s=float(os.environ.get("TTS_CB_COOLDOWN_S", "60")),
        eval_persona_override=_env("EVAL_PERSONA_OVERRIDE", "false").lower()
        in ("1", "true", "yes", "on"),
    )
