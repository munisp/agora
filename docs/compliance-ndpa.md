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
captures (Agent A) record/honor the same registry.

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
