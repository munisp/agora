# NDPA Compliance — Consent Registry & Erasure (SPEC-W12 §4, Agent C)

Nigeria Data Protection Act (NDPA 2023) guardrails for the CAC program:
a central consent registry in **identity-service**, a tombstone-only erasure
flow with a CloudEvent consumer contract, and the consent gate that fronts
KYC resolution (see `docs/kyc.md`).

## ConsentRecord (contract §4)

| Field | Type | Notes |
|---|---|---|
| `consent_id` | uuid | stable across idempotent replays |
| `tenant_id` | uuid | RLS-isolated (`tenant_isolation` policy, `app.tenant_id`) |
| `data_subject_id` | text | phone in E.164 **or** contact uuid — free-form by design |
| `purpose` | text | e.g. `kyc`, `marketing`; free-form, one record per (tenant, subject, purpose) |
| `captured_ts` | timestamptz | first capture wins (replays keep it) |
| `captured_channel` | text | `ussd`\|`web`\|`whatsapp`\|... where consent was taken |
| `captured_locale` | text | e.g. `en-NG` |
| `erasure_ts` | timestamptz null | tombstone; row is never deleted (audit) |

Storage: `identity.consents` (bootstrapped idempotently by the consent store,
`FORCE ROW LEVEL SECURITY` + `tenant_isolation` policy — the
02-identity-schema.sql pattern). Unique key `(tenant_id, data_subject_id,
purpose)` makes capture naturally idempotent.

## API (identity-service)

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/consents` | capture; idempotent on (tenant, subject, purpose) → `201` |
| `GET` | `/v1/consents?subject=` | list a subject's records incl. tombstones → `200` |
| `GET` | `/internal/consents/check?subject=&purpose=` | service-to-service gate → `200 {"allowed":true,...}` / `403` |
| `POST` | `/v1/consents/erasure` | tombstone + erasure event → `202` |

Tenant reference everywhere: `X-Tenant-ID` (uuid) or `X-Tenant-Slug` (slug)
header wins; body/query fields `tenant_id` / `tenant` are the fallback.
`/internal/consents/check` has **no auth middleware by design** (mesh-internal,
same trust level as `/internal/tenants/*`) and requires the tenant header.

Semantics:

- **Replay** of an existing (tenant, subject, purpose) returns the original
  record (`consent_id` + `captured_ts` unchanged); channel/locale refresh.
- **Re-capture after erasure** clears `erasure_ts` — an explicit re-consent.
- **Erasure** sets `erasure_ts = now()` on active records only. Optional
  `purpose` narrows to one purpose; omitted = all of the subject's purposes.
  Nothing-to-erase → `404`; replay-safe.

## Erasure event (contract §4)

CloudEvent on topic **`opendesk.consent.erasure.v1`** (`CONSENT_ERASURE_TOPIC`):

```json
{
  "specversion": "1.0",
  "type": "com.opendesk.consent.ErasureRequested",
  "source": "identity-service",
  "subject": "+2348012345678",
  "tenantid": "<tenant uuid>",
  "data": {
    "tenant_id": "<tenant uuid>",
    "data_subject_id": "+2348012345678",
    "purpose": "kyc",
    "erased_records": 1,
    "erasure_ts": "..."
  }
}
```

Published via identity-service's existing best-effort Dapr pubsub pattern
(same as `TenantProvisioned`). The tombstone row in `consents` is the durable
record — a failed publish is logged and reconcilable by republishing rows
where `erasure_ts` is set.

### Consumer contract (tombstone-only erasure)

identity-service **does not** delete or anonymize data in other services.
Consumers of `opendesk.consent.erasure.v1` MUST, on receipt:

1. match their own records by `(tenantid, data.data_subject_id)`;
2. **anonymize** (not delete, where referential integrity matters) the data
   subject's PII — e.g. booking `contacts` phone/name → tombstone tokens,
   conversation transcripts purged per retention;
3. process idempotently (events are at-least-once; `purpose` narrows scope
   when present);
4. stop using the subject for the erased purpose(s) immediately.

This mirrors the existing GDPR/privacy pattern (`PrivacyEraseRequested` on
`opendesk.privacy.events`, booking-service `consumer/privacy.go`) — the
consent erasure topic is the NDPA-scoped companion.

## Consent-gated processing

kyc-service refuses resolution without an active consent (`purpose=kyc`) —
see `docs/kyc.md`. Notification DND/quiet-hours guards (Agent B) and channel
captures (Agent A) record/honor the same registry. KYC **document** uploads
use the distinct purpose `kyc_document` (see below).

## KYC document lifecycle (SPEC-W58)

KYC document OCR (NIN slip, driver's license, international passport,
voter's card, CAC document) is an assistive pre-fill for human review —
extraction never auto-verifies. NDPA 2023 principle mapping:

| NDPA principle | Gate | Mechanism |
|---|---|---|
| **Purpose limitation** | R5 | Upload requires active consent under the distinct free-form purpose **`kyc_document`** via the existing identity consent gate: denied → `403`, gate down → `502`. Onboarding consent (`kyc`) never silently authorizes document retention. `consent_id`/subject are recorded in `kyc_audit`. |
| **Data minimization** | P8 / R7 | The durable kyc-service record (`kyc_documents`) is **hash-only**: HMAC-SHA256 (`KYC_HASH_SECRET`) per-field hashes + `id_value_hash`; raw values are never stored in kyc-service and never emitted. The full OCR transcript (`extracted_text`) is suppressed for kyc_* kinds. Cleartext fields exist only in the ocr DB, TTL-bound. |
| **Storage limitation / retention** | R3 | kyc_* ocr rows carry `expires_at = now() + OCR_KYC_TTL_HOURS` (default **72h**); a sweeper (`OCR_KYC_SWEEP_MINUTES`, default 15) deletes expired rows **and** MinIO objects. A MinIO bucket lifecycle rule on `documents` is the documented belt-and-braces layer (operator-set; cannot be provisioned from this repo). |
| **Erasure** | R2 | ocr-service consumes `ErasureRequested` (`opendesk.consent.erasure.v1`) and `TenantDeleted`: kyc_* rows are blanked (`extracted_fields`/`extracted_text`), set `status='erased'`, and the MinIO object is deleted; `sha256`/`phash` are retained for audit. TenantDeleted hard-deletes ALL of the tenant's ocr rows + objects (all kinds). The kyc-side audit trail survives as hashes — tombstone idiom, accountability preserved. |
| **Access scoping** | R4 | All KYC document routes are gated **owner/admin** (NOT `manage_bookings` — field agents hold member); the review GET serves fields only through that gate; the admin-web KYC review tab is role-hidden for everyone else (`canViewKycReview`). |
| **Auditability / accountability** | R8 | Every upload and every review decision writes an append-only `kyc_audit` row (REVOKE UPDATE/DELETE + BEFORE trigger); every review-field VIEW emits a structlog view-audit line (actor, document_id, tenant). |
| **Bus redaction** | R1 | `com.opendesk.ocr.DocumentExtracted` emits `extracted_fields: {}` for kyc_* kinds — KYC PII never rides Kafka. |
| **Human oversight** | P7 | `review_state` defaults to `pending_review`; only an owner/admin approve/reject (with reason, audited) is terminal. MRZ check digits for passports are validated by deterministic ICAO 9303 code — never claimed from the model. |

**Backup-window note (R12):** backups may retain erased/expired document
PII until rotation — `BACKUP_KEEP` must be sized to the NDPA erasure window
and erased subjects must never be restored without re-applying tombstones
(docs/data-residency.md). The 72h KYC TTL shrinks live exposure regardless
of the backup window.

## Data minimization notes

- The registry stores *that* consent exists, never the underlying identity
  documents. kyc-service stores only a SHA-256 hash of BVN/NIN
  (`kyc_audit.id_value_hash`).
- Tombstones are kept for audit (NDPA accountability principle); subject PII
  in downstream services is anonymized by consumers per the contract above.

## Ops

- Env: `CONSENT_ERASURE_TOPIC` (default `opendesk.consent.erasure.v1`).
- Topic provisioning: `infra/kafka/create-topics.sh` (Wave-12 additive, Agent D).
- Fresh/existing installs: no migration needed — the consent store
  bootstraps `consents` + RLS policy at boot (idempotent DDL).
