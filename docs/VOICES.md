# VOICES.md — voice consent roster

Roster of every voice shipped with, or enrolled into, the Agora platform.
Pattern credit: loudkit's `VOICES.md`. Licensing posture for the underlying
providers/models lives in `docs/voice-licensing.md`; this file tracks
**per-voice provenance and consent**.

## Enrolled voices

| voice_id | provider | languages | source | license | consent basis | enrolled date |
|---|---|---|---|---|---|---|
| _No enrolled voices._ | — | — | — | — | — | — |

**The platform ships no cloned voices today.** The roster starts empty;
stock provider voices (piper/mms/azure/spitch catalogs) are not enrolled
records — their posture is covered by `docs/voice-licensing.md` and the
provider's own terms.

## Enrolment contract

Every cloned/enrolled voice MUST appear in this roster, and every roster row
MUST carry a non-empty **consent basis** (who consented, how, and when —
e.g. "signed voice-release contract, Jane Operator, 2026-11-16").

- `consent_basis` is **mandatory at enrollment** and enforced **fail-closed
  by the `cosyvoice-tts` sidecar**: `POST /voices` without a non-empty
  `consent_basis` is rejected (HTTP 422). See `services/cosyvoice-tts/`
  (README + registry contract) for the enforcement details — this document
  does not duplicate them.
- The sidecar registry record
  `{voice_id, name, languages, source, license, consent_basis, enrolled_at}`
  mirrors the columns above; enrolling a voice through the API is necessary
  but not sufficient — add the roster row in the same change.
- `source` records where the reference audio came from (e.g. "studio
  recording, 15 s, operator's CEO"), `license` the license of any model or
  dataset the voice derives from (cross-reference
  `docs/voice-licensing.md`).
- Removing a voice is a two-step change: `DELETE /voices/{id}` on the
  sidecar **and** removal of the roster row here.

## Roster entry template

```
| <voice_id> | cosyvoice | <e.g. pcm, en> | <reference audio origin> | <model/voice license> | <who consented, how, when> | <YYYY-MM-DD> |
```
