# NDPA 2023 Compliance Profile (Nigeria)

How Agora maps to the **Nigeria Data Protection Act 2023** (NDPA),
regulated by the Nigeria Data Protection Commission (NDPC). This profile
pairs the **nigeria-sme industry pack** (Pidgin-first persona, NGN pricing,
consent text — industries/nigeria-sme.yaml) with the **ndpa-profile env**
(infra/privacy/ndpa-profile.env) and the retention enforcement in
conversation-service.

> Section numbers below follow the gazetted NDPA 2023 structure (Part V
> principles s. 24, lawful bases/consent ss. 25-26, sensitive data s. 30,
> data-subject rights ss. 34-38, security s. 39, breach notification s. 40,
> cross-border transfer ss. 41-43). Verify citations against the gazetted
> Act and current NDPC guidance before relying on them in filings — this
> document is an engineering control mapping, not legal advice.

## Quick start

```bash
cat infra/privacy/ndpa-profile.env >> .env   # RETENTION_DAYS=180, VOICEPRINTS=off, INTEL_LLM=off
docker compose up -d
scripts/seed-industries.sh                    # creates acme-ng (nigeria-sme, NGN, Africa/Lagos)
```

## Control mapping

| NDPA 2023 | Requirement | Platform control |
|---|---|---|
| s. 24 (principles: purpose & **storage limitation**) | Keep personal data no longer than necessary | **Retention sweeper** in conversation-service (`app/retention.py`): startup + hourly hard-delete of turns older than `RETENTION_DAYS` (platform default 365; **NDPA profile: 180**), batched per tenant inside RLS tenant transactions, cutoff computed with the database clock. See "Retention" below. |
| s. 24 (data minimization) | Collect only what is needed | PII marker is a single flat `contact_phone` column (no turn-text scanning); NDPA profile keeps `INTEL_LLM=off` so transcript text is not sent to an LLM for NER (lexicon-only sentiment runs locally). |
| ss. 25-26 (lawful basis, **consent**) | Freely given, specific, informed consent; withdrawal | Pack **`consentText`** (nigeria-sme, clinic): spoken/data-processing + call-recording notice the tenant presents at call start (Pidgin text mirrors the register). **Voiceprint consent gate**: voice biometrics require `VOICEPRINTS=on` AND a recorded per-caller consent flag at enrollment (services/voice-agent-runtime/app/voiceprint.py); the NDPA profile ships `VOICEPRINTS=off`. |
| s. 30 (sensitive personal data) | Stricter basis + DPIA for biometric data | Voiceprints are biometric — disabled entirely under the NDPA profile (`VOICEPRINTS=off`). Enrollment is impossible while the gate is off (API raises `VoiceprintsDisabled`). |
| ss. 34-38 (**data-subject rights**: access, rectification, erasure, portability, objection) | Honour requests within the statutory window | The existing **GDPR export/erase workflows are reused for NDPA requests** (same rights substance): booking-service `POST /v1/privacy/export` → `GdprExportWorkflow` gathers bookings, conversations, ledger balance and CRM records into one JSON bundle uploaded to MinIO `exports`; `POST /v1/privacy/erase` → `GdprEraseWorkflow` publishes a `PrivacyEraseRequested` tombstone to `opendesk.privacy.events`, consumed by booking, conversation and crm-sync to delete/anonymize their copies. Procedure below. |
| s. 39 (security safeguards) | Appropriate technical/organisational measures | FORCE ROW LEVEL SECURITY per tenant (Postgres, `app.tenant_id` per transaction), per-service least-privilege DB roles (infra/postgres/init-scripts/05-app-roles.sql), Keycloak OIDC + APISIX jwt-auth on `/api/*`, daprd mTLS-capable service invocation, encrypted-at-rest options per host, backups under infra/backups. |
| s. 40 (**breach notification**) | Notify NDPC within 72 hours (and affected subjects when high risk) | **Hookup note (partially manual today):** there is no dedicated breach-detection workflow. Wire detection sources (WAF/openappsec alerts, OpenSearch anomaly dashboards, ops runbook triage) to the notification-worker **webhook platform** (SPEC-W3 webhook subscriptions/deliveries) or a plain `SendEmail` activity to alert the tenant owner + DPO contact immediately; the 72-hour NDPC filing itself is an operator action — record it in the incident runbook (docs/runbooks/security.md). |
| ss. 41-43 (cross-border transfer) | Adequacy basis or explicit consent for transfers | **Data residency by architecture**: Agora is self-hosted — run the full stack in-country (docker-compose on a Nigerian host, or the k3s appliance in deploy/k3s on-prem at the business site). Postgres, OpenSearch, MinIO and Kafka/Fluvio all hold personal data on local volumes. LLM/STT/TTS are local by default (Ollama, faster-whisper, Piper) — no transcript leaves the host. Region guidance + cloud-VM caveats: infra/privacy/ndpa-profile.env "Storage / residency". |
| Records of processing / accountability | Maintain processing records | This document + the pack's consentText + the seeded knowledge docs form the engineering-side record; the tenant's RoPA entry should reference the nigeria-sme pack, the 180-day retention window and the (empty) processor list under the NDPA profile. |
| DPO designation | Appoint a DPO where required (data controllers of importance) | **DPO contact config**: record the DPO in the tenant's pack `consentText` / knowledge seed (the nigeria-sme consent text instructs callers to "talk to our staff") and in the ops runbook; surface it in the admin dashboard Settings page alongside the Industry pack card. No dedicated `DPO_*` env is consumed by code today — see Pending items. |

## Retention (storage limitation in detail)

conversation-service runs `RetentionSweeper` (services/conversation-service/app/retention.py):

- Env: `RETENTION_ENABLED` (default true), `RETENTION_DAYS` (default 365,
  **NDPA profile 180**), `RETENTION_SWEEP_SECONDS` (default 3600),
  `RETENTION_BATCH_SIZE` (default 1000). Compose passes all of them through
  with dev defaults.
- Each cycle enumerates tenant ids and hard-deletes turns older than the
  window per tenant, in batches, inside a transaction with `app.tenant_id`
  set — FORCE ROW LEVEL SECURITY confines every batch to its tenant.
- The cutoff is `now()` **in SQL** — app clock skew cannot extend retention.
- Erasure (GDPR/NDPA rights) and retention are orthogonal: the privacy erase
  consumer deletes a subject's turns immediately; the sweeper removes aged
  rows erasure did not cover. Both hard-delete; nothing is resurrected.
- Conversation **shells** (id, tenant, timestamps) are kept for referential
  integrity with bookings/analytics; only turn content is deleted.
- **Caveats:**
  - Tenant enumeration requires an RLS-bypassing role (the default
    `opendesk` superuser DSN). Under an RLS-enforced role
    (`app_conversation_login`) the sweep finds no tenants and no-ops — run
    retention with the superuser DSN or a dedicated maintenance role.
  - Transcripts **indexed into OpenSearch** (index `conversations`) and
    raw records in Kafka/Fluvio are NOT covered by the Postgres sweeper;
    apply index/topic retention (e.g. OpenSearch ISM policy, Kafka
    `retention.ms`) to match the 180-day window in production.
  - MinIO export bundles are point-in-time snapshots; delete them after
    delivery to the data subject.

## NDPA data-subject request procedure

Reuse the GDPR plumbing (rights are materially the same):

1. **Identify the subject** by phone and/or e-mail (the same identifiers the
   platform stores on bookings and `conversations.contact_phone`).
2. **Access/portability (ss. 34, 37):**
   `POST /v1/privacy/export {tenant_id, tenant_slug, phone?, email?}` on
   booking-service → `GdprExportWorkflow` collects bookings, conversations,
   ledger balance and the CRM person into one JSON bundle in the MinIO
   `exports` bucket; deliver the object (dev: path; prod: presigned URL)
   to the requester.
3. **Erasure (s. 36):**
   `POST /v1/privacy/erase {tenant_id, tenant_slug, phone?, email?}` →
   `GdprEraseWorkflow` publishes the `PrivacyEraseRequested` tombstone;
   booking, conversation and crm-sync consumers delete/anonymize their
   copies (conversation turns hard-deleted, contact marker cleared).
4. **Rectification (s. 35):** update the contact record in the Twenty CRM
   (or via admin-web); conversation turns are immutable transcript records —
   rectify by erasure where the content is inaccurate.
5. **Objection (s. 34):** for recording objection at call time, the caller
   invokes the consent text's opt-out ("tell us now and we go stop the
   recording"). The voice runtime has no per-call recording-off flag yet
   (see Pending items) — operationally the tenant honours the opt-out by
   ending the recorded line and continuing via SMS/WhatsApp, or by erasure
   of the resulting conversation after the call.
6. **Log the request** (date, channel, fulfilment evidence) in the tenant's
   records-of-processing; NDPA expects requests honoured without undue
   delay.

## Voiceprint consent gate

`VOICEPRINTS=off` (NDPA profile) makes every voiceprint API raise
`VoiceprintsDisabled` — no embeddings are computed or stored. Even with
`VOICEPRINTS=on` (non-NDPA deployments), enrollment requires an explicit
per-caller `consent=True` recorded alongside the embedding
(app/voiceprint.py). Voiceprints are biometric data under NDPA s. 30; do
not enable them for Nigerian tenants without a DPIA and explicit consent.

## Pending items (honest gaps)

- **`consentText` passthrough**: the field is validated-tolerant
  (scripts/validate_pack.py + Go loader accept extra fields), but
  identity-service's Go `Pack`/`Summary` structs do not carry it yet, so it
  is not served in `GET /v1/tenants/{slug}`'s pack summary. Tooling reading
  the pack YAML directly can use it today. Adding `ConsentText` (and
  `Languages`) to the Go structs is a small follow-up in identity-service.
- **Automatic spoken consent + recording opt-out**: the voice runtime does
  not yet speak `consentText` at call start automatically, and has no
  per-call recording-off flag; tenants include the notice in their call
  flow/knowledge until the runtime consumes it (blocked on the passthrough
  above).
- **Breach-notification workflow**: alerting hookup exists (notification-
  worker webhooks + email activities) but there is no dedicated
  BreachDetected workflow with a 72-hour timer; the NDPC filing is manual.
- **OpenSearch/Kafka retention**: the Postgres sweeper does not expire
  indexed transcripts or stream topics; configure ISM/`retention.ms` to
  match `RETENTION_DAYS`.
