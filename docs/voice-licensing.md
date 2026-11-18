# TTS voice licensing posture (SPEC-W72)

This document records the licensing posture of every TTS voice technology
Agora can synthesize with, and the rules operators must follow before a voice
reaches production. It is the licensing companion to
`docs/voices-sidecars.md` (sidecar runbook) and `docs/VOICES.md` (consent
roster for enrolled/cloned voices).

## Provider / model license matrix

| Provider | Model / service | License | Commercial use? | Posture |
|---|---|---|---|---|
| `mms` | Meta MMS checkpoints (`facebook/mms-tts-{eng,pcm,yor,ibo,hau}`) | **CC-BY-NC 4.0 (NON-COMMERCIAL)** | **No, not by default** | Development / evaluation default — see below |
| `cosyvoice` | CosyVoice-2 (`FunAudioLLM/CosyVoice`) | **Apache-2.0** | **Yes** | Recommended commercial-safe path for owned Nigerian voices |
| `qwen3` | Qwen3-TTS 1.7B CustomVoice served by nari-qwen3-tts (`nari-labs/nari-qwen3-tts` serving stack; `QwenLM/Qwen3-TTS` model) | **Apache-2.0** (BOTH the serving-stack repo LICENSE and the Qwen3-TTS model repo — verified against the repo LICENSE files, see below) | **Yes** | GPU-tier English-primary option (**requires NVIDIA H100**; **NOT a Nigerian-language path**) |
| `spitch` | Spitch hosted API | Vendor terms (hosted commercial API) | Yes, per Spitch terms | Operator contract governs |
| `azure` | Azure AI Speech (e.g. `en-NG-EzinneNeural`, `en-NG-AbeoNeural`) | Microsoft Azure service terms | Yes, per Azure terms | Operator subscription governs |
| `piper` | Piper (rhasspy/piper) | MIT (code); per-voice-model licenses vary | Check per voice model | Verify the specific voice model license before shipping |
| `xtts` | Coqui XTTS-v2 | Coqui Public Model License (non-commercial) | **No, not by default** | Same posture as MMS: dev/eval only without counsel sign-off |

## Meta MMS — CC-BY-NC 4.0 (non-commercial)

The MMS checkpoints served by the `services/mms-tts/` sidecar are released
under **CC-BY-NC 4.0**: commercial use is not granted by the license.

**Posture: development/evaluation default.** MMS voices may be used for
development, demos, and evaluation. **Production commercial use requires the
operator's counsel sign-off, or substitution of an alternate voice** (the
commercial-safe `cosyvoice` path below, or a hosted commercial API such as
Azure/Spitch).

### Where MMS is used today

- **Sidecar:** `services/mms-tts/` (FastAPI, port 5800 — see
  `docs/voices-sidecars.md`). Its README already carries the CC-BY-NC-4.0
  notice.
- **Pack voice maps:** every shipped industry pack defaults to
  `provider: mms` and maps Nigerian languages to `mms:` voice ids:

  | Pack | MMS voice mappings |
  |---|---|
  | `industries/agritech.yaml` | `pcm: mms:pcm`, `ha: mms:hau` |
  | `industries/ecommerce-d2c.yaml` | `pcm: mms:pcm` |
  | `industries/fintech-agent-banking.yaml` | `pcm: mms:pcm`, `yo: mms:yor`, `ig: mms:ibo` |
  | `industries/fmcg-retail.yaml` | `pcm: mms:pcm` |
  | `industries/hospitality.yaml` | `pcm: mms:pcm` |
  | `industries/nigeria-sme.yaml` | `pcm: mms:pcm` |
  | `industries/pension-fund.yaml` | `pcm: mms:pcm`, `yo: mms:yor`, `ig: mms:ibo`, `ha: mms:hau` |

  (English in these packs routes to `azure:` neural voices — a hosted
  commercial API.)

An operator taking any of these packs to commercial production with MMS
voices must obtain counsel sign-off or re-map the affected languages (e.g.
`pcm: "cosyvoice:naija-1"` once an owned voice is enrolled).

## CosyVoice-2 — Apache-2.0 (commercial-safe, recommended)

[CosyVoice-2](https://github.com/FunAudioLLM/CosyVoice) is released under
**Apache-2.0**, which permits commercial use. It is the **recommended path
for owned Nigerian voices**: zero-shot voice cloning from a ~15 s reference
recording lets an operator enroll a consented brand voice and serve it
through the `cosyvoice` provider (`services/cosyvoice-tts/` sidecar, port
5820 — see `docs/voices-sidecars.md`).

Notes:

- Apache-2.0 covers the **model/code**, not the cloned voice itself — the
  reference recording still requires documented consent from the speaker
  (see `docs/VOICES.md`; enrollment is fail-closed on `consent_basis`).
- Real CosyVoice-2 inference is GPU-gated; the sidecar ships mock-default
  (`COSYVOICE_MOCK=1`) like the xtts sidecar.

## Qwen3-TTS (nari) — Apache-2.0 (commercial-safe, GPU-tier, English-primary)

[nari-qwen3-tts](https://github.com/nari-labs/nari-qwen3-tts) — the serving
stack Agora's `qwen3` provider talks to (SPEC-W74) — is released under the
**Apache License, Version 2.0**, verified directly against the repo LICENSE
file (<https://raw.githubusercontent.com/nari-labs/nari-qwen3-tts/main/LICENSE>):
the file is the verbatim Apache-2.0 text ("Apache License, Version 2.0,
January 2004, http://www.apache.org/licenses/"), and GitHub's license
detection reports `apache-2.0`. The underlying model it serves,
[Qwen3-TTS](https://github.com/QwenLM/Qwen3-TTS) (`Qwen/Qwen3-TTS-12Hz-1.7B-
CustomVoice`), is also released under **Apache-2.0** (verified against the
QwenLM/Qwen3-TTS repo LICENSE). **Commercial use is permitted by both
licenses.**

Caveats that are deployment constraints, not license restrictions:

- **Hardware: the published container requires an NVIDIA H100** (NVIDIA
  Container Toolkit + CUDA 13.0-compatible driver). There is no CPU or
  consumer-GPU path.
- **Language: the engine has been tested with English as the primary
  language ONLY.** It is **NOT a Nigerian-language path** — cosyvoice
  remains the recommended commercial-safe path for owned Nigerian voices
  (pcm/yo/ha/ig).
- The model weights are pulled from Hugging Face at first boot (HF_TOKEN);
  operators remain responsible for complying with the model's own Hugging
  Face distribution terms at pull time.

## Spitch / Azure — hosted commercial APIs

`spitch` and `azure` voices are synthesized by hosted commercial APIs. No
model weights are distributed by Agora; usage is governed by the operator's
own subscription and the vendor's terms of service. No additional Agora-side
license posture applies, but operators remain responsible for their vendor
terms (e.g. Azure's disclosure requirements for synthetic voices).

## Naija-TTS-Dataset — pcm fine-tune/eval corpus candidate

**Candidate corpus:** *Naija-TTS-Dataset* on the Mozilla Data Collective —
1,987 clips / ~5h55m of urban Nigerian Pidgin (pcm) read speech, distributed
as MP3+TSV pairs, TTS-pipeline-ready. Intended use: fine-tuning and/or
evaluation corpus for an owned Pidgin voice on the CosyVoice-2 path.

### PRE-USE GATE — do not download or train yet

The dataset's **license and consent terms must be verified on its Mozilla
Data Collective page before any download, fine-tuning, or evaluation run.**
When the terms are verified, record them in this document (replace the
PENDING entry below) with the verifier, date, and the exact license/consent
text or URL.

| Field | Value |
|---|---|
| Dataset | Naija-TTS-Dataset (Mozilla Data Collective) |
| Content | 1,987 clips / ~5h55m urban Nigerian Pidgin read speech (MP3+TSV) |
| Intended use | pcm fine-tune / evaluation corpus for CosyVoice-2 |
| **Terms verification** | **PENDING** — verify license + consent terms on the dataset's Mozilla Data Collective page before any download or training |
| Verified by / date | — (fill in when verified) |
| Verified terms | — (record license name + consent language here when verified) |

Until this gate is cleared, the dataset is **not** downloaded or used by any
Agora pipeline (see SPEC-W72 "Honest register").
