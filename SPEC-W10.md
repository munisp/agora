# SPEC-W10 — Voice Wave: African/Nigerian TTS (MMS, XTTS cloning, Azure en-NG, Spitch)

Wave 10 contract. Repo: `/mnt/agents/output/opendesk` (flaky FUSE — work in `/tmp`, rsync ADDITIVELY, md5-verify).

OWNERSHIP (collision-critical):
- Agent A (runtime core): `services/voice-agent-runtime/app/tts_providers/**` (new), `app/pipeline/tts.py`,
  `app/config.py`, `app/multilang.py`, `app/control_plane.py`, `app/metrics.py` (additive only),
  `tests/test_tts_providers.py` (new), `docs/voices.md`
- Agent B (sidecars): `services/mms-tts/**` (new), `services/xtts-tts/**` (new),
  `infra/compose/voices.compose.yml` (new override — do NOT edit docker-compose.yml), `docs/voices-sidecars.md`
- Agent C (admin UI): `apps/admin-web/**` only
- Agent D (pack voice defaults): `scripts/validate_pack.py`, Go pack loader (identity-service packs.go),
  `industries/nigeria-sme.yaml`, `industries/hospitality.yaml`, `industries/index.json` (sha regen for the 2 edits)

## Cross-agent contracts
- Sidecar HTTP (B builds, A consumes):
  - MMS: `POST {MMS_TTS_URL=http://mms-tts:5800}/tts {text, lang} → audio/wav` (langs: eng, pcm, yor, ibo, hau); `GET /voices` → `{voices:[{id,languages,gender,labels}]}`
  - XTTS: `POST {XTTS_TTS_URL=http://xtts-tts:5810}/tts {text, voice_id, language} → audio/wav`; `POST /voices {name, sample_base64} → {voice_id}`; `GET /voices`; `DELETE /voices/{id}`
- Runtime control plane (A builds, C consumes):
  - `GET /voice/voices` → `{providers:[{name, available, voices:[{id, languages, gender, labels}]}]}` (aggregates configured providers; unavailable providers listed with available:false)
  - `POST /voice/tts-preview {text, language?, provider?, voice?} → audio/wav` (routes through the same FallbackTTS; used by admin preview; reuse the existing control-plane auth pattern)
  - `POST /voice/voices/enroll {name, sample_base64, tenant}` → `{voice_id}` (XTTS brand-voice enrollment; requires xtts provider enabled; consent_required note in docs)

## Part A — TTS provider layer (Agent A)

1. `app/tts_providers/`: `base.py` (Voice{id,languages,gender,labels}, provider protocol: `list_voices`, `synthesize(text, voice, language) -> wav bytes`, `available()`), and providers:
   - `mms.py` — HTTP client for MMS sidecar contract above (10s timeout, one retry).
   - `xtts.py` — HTTP client for XTTS sidecar contract above (30s timeout, GPU latency).
   - `azure.py` — direct Azure Cognitive Speech TTS: `POST https://{AZURE_SPEECH_REGION}.tts.speech.microsoft.com/cognitiveservices/v1` SSML, `Ocp-Apim-Subscription-Key`, voices en-NG-AbeoNeural/en-NG-EzinneNeural (+ a small curated list: sw-KE, am-ET, yo-NG if documented — verify via web search, isolate voice list in one constant). Output riff 24khz16bit → wav passthrough.
   - `spitch.py` — Spitch API (Nigerian languages) behind ONE isolated `build_request()` with docs URL comment; SPITCH_API_KEY; if docs are inconclusive, implement the documented shape + flag assumption in docs/voices.md.
2. `chain.py` — `FallbackTTS` implementing the EXISTING `TTSInterface` (app/pipeline/tts.py): ordered chain from `TTS_PROVIDER_CHAIN` env (default "piper"), per-provider circuit breaker (mirror app/pipeline/llm.py FallbackLLM breaker: N failures → open 60s), failure metrics (extend app/metrics.py additively: tts_provider_failures_total{provider}), piper always implicit last-resort if configured.
3. Voice routing — `TTS_VOICE_MAP` env JSON `{"pcm": "mms:pcm", "en-NG": "azure:en-NG-EzinneNeural", "yo": "mms:yor", "ha": "mms:hau", "ig": "mms:ibo"}`; multilang.py: extend language→voice resolution to consult TTS_VOICE_MAP first (provider-qualified), then PIPER_VOICE_MAP fallback — keep existing behavior when unset. Per-tenant override via tenant context `tts_voice` (defensive getattr).
4. config.py — TTS_PROVIDER_CHAIN, TTS_VOICE_MAP, MMS_TTS_URL, XTTS_TTS_URL, AZURE_SPEECH_KEY/REGION, SPITCH_API_KEY/BASE_URL.
5. control_plane.py — the 3 endpoints in the cross-agent contract (follow existing auth patterns; enrollment is admin-path).
6. tests/test_tts_providers.py — chain ordering, breaker open/half-open, voice-map parsing+precedence, provider clients (httpx mocks), endpoint tests (follow existing test patterns). Full pytest suite must pass.

## Part B — Sidecars (Agent B)

1. `services/mms-tts/` — FastAPI, python:3.11-slim Dockerfile: `transformers`, `torch` (CPU), `soundfile`;
   lazy per-lang model load `facebook/mms-tts-{eng,pcm,yor,ibo,hau}` behind env MMS_LANGS; POST /tts
   (contract above), GET /voices (static catalog), /healthz; MOCK mode (MMS_MOCK=1, default): sine-based
   deterministic wav keyed by lang so e2e works without 200MB downloads; real-model lines ready to enable.
2. `services/xtts-tts/` — FastAPI: coqui `TTS` XTTS-v2 lazy load behind XTTS_MOCK=1 default (GPU notes in
   README — ≥6GB VRAM; CPU unusable); contract endpoints above; in-memory+volume voice registry
   (voices.json + samples dir); mock returns sine wav with voice-specific pitch.
3. `infra/compose/voices.compose.yml` — override with both services (:5800/:5810, healthchecks, volumes).
4. `docs/voices-sidecars.md` — runbooks, mock vs real enablement, model sizes/licenses (MMS CC-BY-NC note
   for commercial use!, XTTS Coqui license note), GPU requirements.
5. Validate: py_compile; python import smoke of FastAPI apps with mocks (if deps installable, else careful static review).

## Part C — Admin voice studio (Agent C, admin-web)

- NEW `app/app/[orgSlug]/voices/` page ("Voices" nav, owner/admin/staff view; manage=enroll owner/admin):
  - Provider/voice browser from `GET /voice/voices` (via the existing BFF/gateway pattern — find how
    call-client reaches /voice/session and follow it), grouped by provider with availability badges,
    language chips (en, en-NG, pcm, yo, ha, ig, sw…).
  - Preview composer: text input + language + voice select → `POST /voice/tts-preview` → play returned
    wav in an <audio> element (blob URL, cleanup).
  - Brand-voice enrollment card (owner/admin): name + 6–30s sample upload (base64) →
    `POST /voice/voices/enroll` → shows new voice_id; consent checkbox required ("I confirm this speaker
    consented") — NDPA note.
- Warm low-saturation style; tsc --noEmit must pass (npm ci first).

## Part D — Pack voice defaults (Agent D)

- validate_pack.py: optional `voice: {provider: enum(piper|mms|xtts|azure|spitch), voiceId?, languages: {lang: "provider:voiceId"}}` block; all 31 packs still pass.
- identity-service packs.go: additive voice passthrough into tenant context JSON (mirror mcpServers pattern); go build/vet/test green.
- industries/nigeria-sme.yaml + hospitality.yaml: add `voice` blocks (nigeria-sme: provider mms,
  languages {en: "azure:en-NG-EzinneNeural", pcm: "mms:pcm"} with comment that azure falls back to mms/piper;
  hospitality: en-NG azure + pcm mms). Regenerate their index.json sha256 (upsert-index); validate-index passes.
- Note in docs/industries.md (one line): packs may declare voice defaults (see docs/voices.md).
