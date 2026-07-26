# Voices — TTS provider layer (SPEC-W10)

The voice runtime synthesizes speech through a pluggable **provider chain**
(`app/tts_providers/`) instead of talking to Piper directly. Default
configuration is **piper-only and byte-identical to pre-W10 behavior**;
African/Nigerian voices are enabled by adding providers to the chain and
routing languages to them via `TTS_VOICE_MAP`.

## Providers

| Provider | Kind | Enablement | Voices |
| --- | --- | --- | --- |
| `piper` | local sidecar/subprocess (existing) | always configured; implicit last resort | `PIPER_VOICE`, `PIPER_VOICE_MAP` ids |
| `mms` | sidecar HTTP (`services/mms-tts`) | `MMS_TTS_URL` (default `http://mms-tts:5800`) + `mms` in chain | eng, pcm, yor, ibo, hau (ISO-639-3) |
| `xtts` | sidecar HTTP (`services/xtts-tts`, GPU) | `XTTS_TTS_URL` (default `http://xtts-tts:5810`) + `xtts` in chain | enrolled brand voices (`voice_id`) |
| `azure` | Azure Cognitive Speech REST | `AZURE_SPEECH_KEY` + `AZURE_SPEECH_REGION` + `azure` in chain | curated list below |
| `spitch` | Spitch REST API | `SPITCH_API_KEY` (+ optional `SPITCH_BASE_URL`) + `spitch` in chain | named characters (sade, femi, …) for en/yo/ha/ig |

Sidecar runbooks, model sizes and licenses (MMS **CC-BY-NC** — commercial use
needs care; XTTS Coqui license; GPU requirements) live in
`docs/voices-sidecars.md`.

## Chain & circuit breakers

`TTS_PROVIDER_CHAIN` (comma-separated, default `"piper"`) sets the fallback
order. `FallbackTTS` (`app/tts_providers/chain.py`) implements the existing
`TTSInterface` (`sample_rate` + `synthesize_pcm`) and is consumed by the
control-plane voice endpoints; the LiveKit worker's PiperTTS call-site is
unchanged. Every provider hop is guarded by a per-provider circuit breaker
(mirroring `FallbackLLM`): `TTS_CB_FAILURES` consecutive failures (default 3)
open the circuit for `TTS_CB_COOLDOWN_S` (default 60s), then one half-open
probe; a failed probe re-opens. Failures are counted on
`tts_provider_failures_total{provider}` (`/metrics`). **Piper is always the
implicit last resort** when configured, even if not listed in the chain.

## Voice routing

Resolution order for a turn's voice (see `app/multilang.py
resolve_tts_voice`):

1. **Per-tenant override** — tenant context `tts_voice` (defensive getattr;
   pack-supplied, e.g. from a pack `voice` block via the identity service).
2. **`TTS_VOICE_MAP`** — JSON mapping language tags to **provider-qualified**
   `"provider:voiceId"` values, consulted before `PIPER_VOICE_MAP`:

   ```json
   {"pcm": "mms:pcm", "en-NG": "azure:en-NG-EzinneNeural",
    "yo": "mms:yor", "ha": "mms:hau", "ig": "mms:ibo"}
   ```

   Keys keep region subtags (`en-NG` ≠ `en`); lookup tries the full tag, then
   the primary subtag. Unlike the piper path, **`pcm` is NOT proxied to
   English** — MMS/Spitch have real Pidgin voices. Bare (unqualified) values
   keep legacy piper semantics.
3. **`PIPER_VOICE_MAP`** → `PIPER_VOICE` default (unchanged legacy behavior —
   when `TTS_VOICE_MAP` is unset, resolution is byte-identical to before).

A provider-qualified spec routes synthesis to that provider **first**, then
falls through the remaining chain order (the pinned provider receives the
qualified voice id; fallback providers receive their own defaults).

## Control-plane endpoints

Same (unauthenticated, internal-network) posture as the other control-plane
endpoints; the admin BFF gates enrollment to owner/admin.

- `GET /voice/voices` → `{providers:[{name, available, voices:[{id, languages, gender, labels}]}]}`
  — aggregates configured providers; unreachable ones are listed with
  `available:false` (never 5xx).
- `POST /voice/tts-preview {text, language?, provider?, voice?}` → `audio/wav`
  — routes through the same `FallbackTTS`; `voice` may be provider-qualified,
  `provider` pins the attempt to one provider (400 if unknown, 502 if the
  chain fails, 422 on empty text).
- `POST /voice/voices/enroll {name, sample_base64, tenant}` → `{voice_id}`
  — XTTS brand-voice enrollment; 400 when `xtts` is not in the chain or the
  sample is not valid base64. **Consent required (NDPA):** the admin UI shows
  a mandatory "speaker consented" checkbox before calling this endpoint;
  enroll only with documented speaker consent (6–30s clean sample).

## Azure voice list (curated)

Single source of truth: `AZURE_VOICES` in `app/tts_providers/azure.py`.
Verified against the official Microsoft list (GA voices):
<https://learn.microsoft.com/en-us/azure/ai-services/speech-service/language-support>

| Voice | Locale | Gender |
| --- | --- | --- |
| `en-NG-EzinneNeural` | English (Nigeria) | Female |
| `en-NG-AbeoNeural` | English (Nigeria) | Male |
| `sw-KE-ZuriNeural` | Swahili (Kenya) | Female |
| `sw-KE-RafikiNeural` | Swahili (Kenya) | Male |
| `am-ET-MekdesNeural` | Amharic (Ethiopia) | Female |
| `am-ET-AmehaNeural` | Amharic (Ethiopia) | Male |

**Verified gap:** Azure has **no Yoruba (yo-NG) or Hausa (ha-NG) voices** —
route `yo`/`ha`/`ig`/`pcm` to `mms` or `spitch`. Synthesis uses
`POST https://{region}.tts.speech.microsoft.com/cognitiveservices/v1` (SSML,
`Ocp-Apim-Subscription-Key`, `X-Microsoft-OutputFormat:
riff-24khz16bit-mono-pcm`) with the returned wav passed through.
`available()` is a credentials-present probe (Azure has no cheap
unauthenticated ping); real failures trip the chain breaker.

## Spitch notes

Docs: <https://docs.spitch.app/api/speech/tts> (SDK-level reference only).
**Assumption flag:** Spitch does not publish a raw HTTP reference page; the
request shape was taken from the **official SDK source**
(<https://github.com/spi-tch/spitch-python>, `resources/speech.py`
`generate()`): `POST {SPITCH_BASE_URL=https://api.spitch.app}/v1/speech`,
`Authorization: Bearer {SPITCH_API_KEY}`, `Accept: audio/wav`, JSON
`{text, voice, format:"wav", language, speed:1.0}` → `audio/wav` bytes. The
shape is isolated in `SpitchTTS.build_request()` (`app/tts_providers/spitch.py`)
— if the contract differs in practice, that one method is the fix point.
Documented languages: `en`, `yo`, `ha`, `ig`; named character voices
(`sade`, `femi` confirmed in docs examples — the docs mention 8 characters
but do not publish the full list; extend `SPITCH_VOICES` when published).

## Environment variables (new in W10)

| Var | Default | Purpose |
| --- | --- | --- |
| `TTS_PROVIDER_CHAIN` | `piper` | ordered provider fallback chain |
| `TTS_VOICE_MAP` | _(unset)_ | JSON `lang -> "provider:voiceId"` routing |
| `MMS_TTS_URL` | `http://mms-tts:5800` | MMS sidecar base URL |
| `XTTS_TTS_URL` | `http://xtts-tts:5810` | XTTS sidecar base URL |
| `AZURE_SPEECH_KEY` / `AZURE_SPEECH_REGION` | _(unset)_ | Azure direct TTS credentials |
| `SPITCH_API_KEY` / `SPITCH_BASE_URL` | _(unset)_ / `https://api.spitch.app` | Spitch credentials/base |
| `TTS_CB_FAILURES` / `TTS_CB_COOLDOWN_S` | `3` / `60` | per-provider breaker tuning |

Example (Nigerian SME deployment — azure en-NG for English, MMS for Pidgin,
piper as last resort):

```sh
TTS_PROVIDER_CHAIN=azure,mms,piper
TTS_VOICE_MAP={"en-NG": "azure:en-NG-EzinneNeural", "en": "azure:en-NG-EzinneNeural", "pcm": "mms:pcm"}
AZURE_SPEECH_KEY=... AZURE_SPEECH_REGION=westeurope
```
