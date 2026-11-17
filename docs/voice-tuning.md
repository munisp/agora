# Voice Tuning: Latency Budget + Nigerian-Language Fluency (SPEC-W70)

> **STATUS: IMPLEMENTED** ✅ (unit-test + code-path verified; live model
> inference is environment-blocked — see the honesty note at the bottom).
> - A1 endpointing/VAD/preemptive-synthesis knobs: `services/voice-agent-runtime/app/config.py` + `app/livekit_worker.py` (signature-introspection wiring)
> - A2 STT decode quality + multilingual model routing: `app/pipeline/stt.py` (`condition_on_previous_text=False`, `WHISPER_MODEL_MULTILINGUAL`)
> - A3 spoken-fluency prompt block: `app/prompts.py` (`SPOKEN_STYLE_BLOCK`)
> - A4 Pidgin locale instruction: `app/multilang.py` (`locale_instruction("pcm")`)

Driver: user report — the voice agent on Nigerian languages + Pidgin had
"lots of lag, not fluent". This document is the operator's tuning guide;
the code defaults already improve latency with zero configuration.

---

## 1. Latency budget (per turn)

Where the silence between "caller stops speaking" and "agent starts
replying" goes, and the knob that owns each slice:

| Stage | Default budget | Knob | Notes |
|---|---|---|---|
| VAD silence detection | 0.35 s | `VOICE_VAD_MIN_SILENCE_S` (silero `min_silence_duration`; framework default 0.55) | Lower = snappier, but too low clips natural mid-sentence pauses |
| Endpointing (turn commit) | 0.3 s | `VOICE_MIN_ENDPOINTING_DELAY` (LiveKit 0.10 default 0.5; 0.10.x has no max-endpointing knob) | The min delay is the direct "lag" driver |
| Preemptive synthesis | on | `VOICE_PREEMPTIVE_SYNTHESIS` (LiveKit 0.10 `preemptive_synthesis`, framework default off) | LLM+TTS synthesis starts before end-of-turn finalizes; overlaps with endpointing |
| STT decode | model-bound | `WHISPER_MODEL` / `WHISPER_MODEL_MULTILINGUAL` | `beam_size=1` already; `condition_on_previous_text=False` (hard-set, W70 A2) shortens decode and kills repetition loops |
| LLM TTFT | endpoint-bound | `LLM_BASE_URL` / `LLM_MODEL` / `LLM_TIMEOUT` | streaming tokens into the TTS stage; fallback chain (`LLM_FALLBACK_*`) bounds outages, not latency |
| TTS first-chunk | first sentence | `VOICE_TTS_CHUNKED` (default true, W46 P4) | first audio frame ships after the first sentence synthesizes, not the full reply |

All A1 knobs are wired into `VoicePipelineAgent` / `silero.VAD.load` via
**signature introspection** (`filter_supported_kwargs` in
`app/livekit_worker.py`): a knob the pinned livekit-agents 0.10.x build does
not declare is dropped with a single info log and the framework default
applies — the worker never crashes on a version reshape. Honest caveat:
because the kwargs are signature-filtered against the pinned SDK, the
applied/dropped set is logged at startup (info level, once per stage) —
check the worker boot log to confirm every knob landed.

## 2. STT model tiers (CPU dev vs GPU prod)

| Tier | WHISPER_MODEL | WHISPER_MODEL_MULTILINGUAL | Hardware | Behavior |
|---|---|---|---|---|
| CPU dev | `base` (or `small`) | _(unset)_ | any | one fast model for every language; English great, yo/ig/ha degraded |
| GPU prod (recommended for Nigeria) | `small` | `large-v3-turbo` | CUDA | English stays on the fast model; every non-English turn routes to the accuracy tier |

Routing rule (`resolve_stt_model` in `app/pipeline/stt.py`, pure and
unit-tested): the multilingual model is used ONLY when it is set AND the
effective language — forced `language=` first, else whisper's last
auto-detection — is non-English. It lazy-loads on the first non-English
turn and stays resident; with `PRELOAD_MODELS=true` the worker prewarm hook
loads both models so no call ever pays a cold start. Nigerian Pidgin (`pcm`)
proxies to English for STT (whisper has no pcm model), so Pidgin speech
stays on the fast default model — fluency for Pidgin is handled on the
LLM/prompt side (§3).

## 3. Nigerian-language fluency guidance

- **Yoruba (yo) / Igbo (ig) / Hausa (ha)**: whisper `base` is weak here —
  use the GPU prod tier above (`WHISPER_MODEL_MULTILINGUAL=large-v3-turbo`).
  Multilingual detection switches the per-turn locale instruction
  automatically (Wave 5 #3); declare `languages: [en, yo, ha, ig]` in the
  industry pack to bound the auto-switch set.
- **Nigerian Pidgin (pcm)**: whisper reports Pidgin audio as `en` (no pcm
  model exists — honest limitation). Since SPEC-W70 A4,
  `locale_instruction("pcm")` emits an explicit natural-Pidgin instruction
  ("respond in natural, conversational Nigerian Pidgin"), so the LLM
  answers in Pidgin register even when the pack persona is thin. The
  pcm→en proxy still stands for STT and piper voice resolution.
- **Every prompt** carries the SPOKEN STYLE block (A3): short speakable
  sentences, no markdown/lists/emoji, amounts in naira words ("five
  thousand naira"), one-clause confirmation before acting, and immediate
  mirroring of the caller's language/register including mid-call switches.

## 4. TTS for Nigerian languages: MMS self-host vs hosted (Spitch/Azure)

| Option | Fluency for yo/ig/ha/pcm | Latency | Cost / ops | Data residency |
|---|---|---|---|---|
| **Piper** (default) | English only — Nigerian languages fall back to the configured English voice | lowest (local) | free, trivial | fully in-cluster |
| **MMS sidecar** (`TTS_PROVIDER_CHAIN=mms,piper`, `MMS_TTS_URL`, `TTS_VOICE_MAP={"yo":"mms:yor",...}`) | real yo/ig/ha/pcm voices, noticeably robotic prosody | low (local) | self-hosted GPU/CPU capacity | fully in-cluster (NDPA-friendly) |
| **Spitch** (`spitch` provider, `SPITCH_API_KEY`) | best Nigerian-language naturalness (built for these languages) | network hop to hosted API | per-minute billing | audio leaves the cluster — confirm NDPA posture |
| **Azure Speech** (`azure` provider) | good `en-NG` voices (EzinneNeural etc.), limited yo/ig/ha coverage | network hop | per-char billing | audio leaves the cluster |

Rule of thumb: dev = piper; Nigerian production = Spitch (or Azure for
en-NG) when hosted calls are acceptable, MMS sidecar when data residency
or offline operation wins. The `FallbackTTS` chain keeps piper as the
implicit last resort either way (SPEC-W10).


## 5. W71 performance levers (SPEC-W71)

Ten latency levers landed in W71; defaults improve latency with zero
configuration. Env defaults are triple-consistent across
`app/config.py`, `.env.example` and `docker-compose.fragment.yml`.

| # | Lever | Knob (default) | Where |
|---|---|---|---|
| P1 | Process-wide silero VAD cache — one VAD load per process instead of per session | _always on_ | `app/livekit_worker.py` (`_load_vad`, thread-safe lazy singleton keyed by tuned kwargs) |
| P2 | Whisper decode fast-path: `temperature=0.0` (no multi-temperature re-decode) + `without_timestamps=True` (no timestamp-token constraint decoding) | _always on_ | `app/pipeline/stt.py` transcribe call |
| P3 | CTranslate2 thread tuning passed to every WhisperModel construction | `WHISPER_CPU_THREADS` (0 = auto), `WHISPER_NUM_WORKERS` (1) | `app/pipeline/stt.py` + `app/config.py` |
| P4 | TTS chunk prefetch — depth-1 lookahead pipelines sentence synthesis (latency ~max(chunks)+first instead of sum); cache order, empty-skip and error propagation preserved | _always on_ | `app/pipeline/tts.py` `stream_pcm` |
| P5 | Subprocess TTS without temp files: piper `--output-raw` raw s16le on stdout (flag verified against the piper-tts 1.8.0 wheel), sample rate from the voice `.onnx.json` `audio.sample_rate` (cached per voice); WAV container assembled in memory for the chain adapter | _always on_ (subprocess mode) | `app/pipeline/tts.py` `_subprocess_pcm` / `_subprocess_wav` |
| P6 | Mid-call phrase cache warming — tool-ack/filler lines (from the tenant's real `ToolAckPolicy`) + standard closers synthesized into the TTS LRU after session start; never blocks the greeting; failures only log | _always on_ | `app/livekit_worker.py` (`tts_warm_phrases` / `schedule_tts_warmup`) |
| P7 | LLM response-length cap on our own OpenAI-compatible client paths (NOT the livekit-plugins-openai node — 0.10.x has no such param) | `VOICE_LLM_MAX_TOKENS` (300, 0 disables) | `app/pipeline/llm.py` + `app/config.py` |
| P8 | Chat-history windowing — chat_ctx trimmed in place after each committed agent reply, at safe boundaries only (never between an assistant `tool_calls` message and its tool results; system message always retained) | `VOICE_LLM_HISTORY_MAX` (60 messages, 0 disables) | `app/livekit_worker.py` (`trim_chat_messages` + `ChatHistoryTrimHook`) |
| P9 | Per-stage latency metrics: `voice_llm_first_token_seconds` (stream first chunk), `voice_tts_first_chunk_seconds` (stream_pcm first yield), `voice_turn_total_seconds` (user_speech_committed -> first agent audio via agent event hooks) | _always on_ | `app/metrics.py` + call sites |
| P10 | Ollama serving: model kept resident (default 5m idle-unload otherwise costs a multi-second reload on the next call) + parallel decode slots | `OLLAMA_KEEP_ALIVE` (24h), `OLLAMA_NUM_PARALLEL` (2) — compose passthroughs | `docker-compose.fragment.yml` (ollama service) |

Watch the new P9 series alongside the existing `voice_stt/llm/tts_latency_seconds`
on the worker `/metrics` endpoint (`VOICE_WORKER_METRICS_PORT`, default 9464).

## 6. Honest note (environment-blocked verification)

Live model inference (whisper / MMS / Spitch / Azure) and live LiveKit
round-trips are **not runnable in this development environment**, so the
latency and fluency improvements above are verified by unit tests +
code-path review only. **Operator validation on target hardware
(microphone-in → audio-out turn latency measurement on the GPU prod tier)
is a pre-deploy gate.** Watch `voice_stt_latency_seconds`,
`voice_llm_latency_seconds` and `voice_tts_latency_seconds` on the worker
`/metrics` endpoint (`VOICE_WORKER_METRICS_PORT`, default 9464) during the
first live calls.
