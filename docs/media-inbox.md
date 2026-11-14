# Media inbox — WhatsApp media booking anchor (SPEC-W61)

Inbound WhatsApp **images and PDF documents** (the W60 media funnel) are
optionally **anchored to a booking**: after OCR success the messaging-gateway
posts a metadata row to booking-service, which resolves the sender's phone
against tenant contacts and either anchors the media to a recent booking or
**parks** it for staff review.

**Binding ruling: unknown senders are PARKED. The ingest path NEVER creates
contacts or leads.** Resolution is read-only over existing data; every
parked row waits for a human.

## Flow

```
Meta webhook ──► messaging-gateway /webhooks/whatsapp
                 │  tenant resolve (CHANNEL_SITE_MAP) → dedupe/ledger check
                 │  → per-sender throttle → fetch bytes (Meta 2-GET)
                 ▼
              ocr-service POST /v1/ocr/extract
                 (with expires_in_hours = MEDIA_INBOX_TTL_HOURS when the
                  anchor flag is on)
                 │
                 ▼  ledger.mark(message_id, document_id)
              booking-service POST /v1/media/inbox/ingest   (internauth only,
                 {channel, provider_message_id, document_id,  X-Internal-Token;
                  sender_phone, sender_phone_digits, mime,     edge-DENIED in
                  bytes, sha256_prefix, booking_ref_hint?}     APISIX)
                 │
                 ▼  resolve-or-park (single tx)
        digits-matched contact AND exactly ONE recent candidate booking
            │                                    │
            ▼                                    ▼
        state='anchored'                     state='parked'
        anchor_method='phone_digits'         (awaits staff review)
```

Sequencing preserves the W60 order: site-map resolve → dedupe/ledger check →
throttle → fetch → OCR → ledger.mark → **anchor POST** → `200`. An anchor
POST failure answers `500`, so Meta redelivers; the redelivery hits the
ingest ledger and retries **only** the anchor POST (OCR never re-runs).
The ledger is in-memory per replica (accepted W60 posture); the residual
window equals W60's post-commit failure window — a cross-replica redelivery
may re-OCR once.

### Phone normalization (one canonical form)

Matching is **digits-only, both sides**: the gateway computes
`sender_phone_digits` (keep `[0-9]`, strip a single leading `00`; no country
inference, no `+`), and booking-service re-derives SQL-side as
trust-but-verify against the UNNORMALIZED `contacts.phone` column:

```sql
regexp_replace(regexp_replace(c.phone,'\D','','g'),'^00','') = $digits
```

Stored rows stay byte-identical; only matching is normalized. A functional
index (`idx_contacts_phone_digits`) supports the lookup.

## States

```
            ingest (unique candidate)         staff link
 park┌──────────────► anchored ─────────────► linked
     │                  │                        ▲
     │ ingest (no       │ staff link             │ booking_id + contact_id
     │ unique match)    ▼                        │ recorded; linked_by=sub
     │               linked                      │
     │                                            │
     │ staff discard / auto-cap (oldest excess   │
     └──────────────► discarded ◄────────────────┘  (terminal)
```

| State | Meaning |
|---|---|
| `parked` | ingest could not resolve a unique booking — staff review queue |
| `anchored` | auto-anchored to a unique recent booking via digits-matched phone |
| `linked` | staff linked the row to a booking (`anchor_method='staff'`) |
| `discarded` | staff discarded it, or the parked cap auto-discarded it (`anchor_method='auto_cap'`) |

| `anchor_method` | Meaning |
|---|---|
| `phone_recency` | exactly one phone-matched candidate (hint absent or corroborating) |
| `phone_recency+ref_hint` | SPEC-W66: the caption hint narrowed 2+ phone candidates to exactly one |
| `ref_hint_mismatch` | SPEC-W66: the caption hint matched NONE of the phone candidates — parked, booking NOT touched |
| `staff` | staff link override |
| `auto_cap` | parked-cap auto-discard |

## Caption BK-ref hint (SPEC-W66 WS2)

A sender may quote the booking's public ref in the media caption ("receipt
for BK-7C3X9Q2M"). The messaging-gateway parses the caption **in memory**
for AT MOST ONE `BK-XXXXXXXX` token (case-insensitive regex
`BK-[0-9A-HJKMNPQRSTVWXYZ]{8}` — the Crockford alphabet, no I/L/O/U):

- one token (or the same token repeated) ⇒ uppercase-normalized and sent as
  the optional `booking_ref_hint` field on the anchor payload;
- ≥2 DISTINCT tokens ⇒ NO hint (ambiguity never narrows);
- malformed ⇒ treated as absent, never an error/400;
- **the caption string itself is never logged, stored, or forwarded (P8)** —
  only the 11-char token crosses the wire.

The hint rides the W61 ingest ledger, so an anchor-failure redelivery
re-sends the SAME hint byte-identically. Booking-side resolve rules
(hint NEVER anchors without phone corroboration; NEVER creates
contacts/leads/bookings):

- 1 phone candidate AND (no hint OR hint == that booking's `public_ref`)
  ⇒ anchored, `anchor_method='phone_recency'`, hint recorded.
- >1 candidates AND hint matches exactly one ⇒ anchored,
  `anchor_method='phone_recency+ref_hint'`.
- ≥1 candidates AND hint matches none ⇒ parked,
  `anchor_method='ref_hint_mismatch'`; the booking row is NOT touched.
- 0 candidates ⇒ parked, hint recorded, nothing anchored or created.
- Replay (same `provider_message_id`) returns the existing row unchanged;
  the replayed hint is ignored.

`booking_ref_hint` is an additive nullable column on `media_inbox`
(`ensureMediaInboxDDL`, both-sides idempotent). There is **no new feature
flag** — the hint rides the anchor payload, which only flows when
`WHATSAPP_MEDIA_ANCHOR_ENABLED` is on. The admin-web media-inbox page shows
the hint (Ref hint column) and the anchor method, both null-tolerant.
Subject erasure NULLs `booking_ref_hint` (the booking_id stays for audit);
tenant purge hard-deletes the row as before.

Rows carry: `channel`, `provider_message_id`, `document_id` (ocr-service),
`sender_phone` (+ `sender_phone_digits`), `mime`, `bytes`,
`sha256_prefix` (first 16 hex chars of the content sha256 — PII-light),
`state`, `anchor_method`, `booking_id`, `contact_id`, `linked_by`,
timestamps. `UNIQUE (tenant_id, channel, provider_message_id)` makes ingest
idempotent; RLS + FORCE mirror the existing ensure* chain.

**Parked cap:** constant `mediaInboxParkedCap = 500` (no env). When parked
rows for a tenant exceed the cap on ingest, the oldest excess parked rows
are auto-discarded (`state='discarded'`, `anchor_method='auto_cap'`).

## Staff review workflow

Admin-web **`/app/[orgSlug]/media-inbox`** (owner/admin only — server-side
role guard plus the same gate in booking-service; the nav entry hides for
other roles):

1. **Parked** tab (default): newest first, paginated (`limit` up to 500,
   "load more" refetches with a larger limit).
2. Each row shows received time, channel, sender phone, mime, byte size and
   state. Media **bytes are never served** — reviewers work from metadata.
3. **Link**: paste the booking UUID → `POST /v1/media/inbox/{id}/link`
   `{booking_id}`. The booking must exist in the tenant; the row becomes
   `linked` with `anchor_method='staff'`, `linked_by=<sub>` and the
   booking's `contact_id`.
4. **Discard** (with confirm): `POST /v1/media/inbox/{id}/discard` →
   `discarded`, `linked_by=<sub>`.
5. The **Anchored / Linked / Discarded** tabs filter the same list for
   audit read-back.

The machine ingest route (`POST /v1/media/inbox/ingest`) is
**edge-denied** in APISIX (`block-bookings-media-inbox-ingest`, 404) and
internauth-gated at the service (K2 fail-closed: 503 when
`BOOKING_INTERNAL_TOKEN` is unset, 401 constant-time on mismatch). Its only
caller is the messaging-gateway, in-cluster. Staff routes ride the existing
`/api/bookings/*` OIDC wildcard.

## Privacy / erasure posture

W60 documented that OCR `receipt`/`document` rows were not subject-bound;
W61 **creates** that binding (`document_id ↔ sender phone`) and therefore
closes the gap for WhatsApp-ingested media:

- **Subject erasure (rows):** `media_inbox` joins booking-service
  `subjectPiiSteps` — keyed on the subject's phone digits, the row's
  `sender_phone` is tombstoned to the salted `sha256:…` form and
  `sender_phone_digits` cleared; anchor columns are preserved for audit.
  SPEC-W66 additionally NULLs `booking_ref_hint`.
- **Subject erasure (bytes):** after a successful subject anonymize the
  booking privacy consumer looks up `document_id`s via
  `MediaDocumentIDsForSubject` and POSTs each to ocr-service
  `POST /v1/ocr/documents/{id}/erase` (internauth, fail-closed), which
  hard-deletes MinIO bytes + row, idempotently. Erase failures are logged
  and skipped — the TTL sweep is the byte backstop; the erasure never
  fails on ocr unavailability.
- **Retention TTL:** with the anchor flag on, the OCR extract call carries
  `expires_in_hours = MEDIA_INBOX_TTL_HOURS` (default **168**, range
  1..8760); the ocr sweeper reaps bytes + rows after expiry, bounding
  parked PII retention even without an erasure request.
- **Tenant purge:** `media_inbox` joins `tenantPIIPurgeSteps` (hard delete)
  and ocr-service already hard-deletes all tenant rows + objects on
  tenant deletion (W58 erasure.py).

## Rollout

Both flags default **OFF / production posture** — upgrading is a no-op:

| Env | Default | Meaning |
|---|---|---|
| `WHATSAPP_MEDIA_ANCHOR_ENABLED` | `false` | gateway; when false the W60 path is byte-identical (no ledger-anchor step) |
| `MEDIA_INBOX_TTL_HOURS` | `168` | forwarded as `expires_in_hours` on the OCR extract call (1..8760) |
| `MESSAGING_BOOKING_INTERNAL_TOKEN` | compose maps from `BOOKING_INTERNAL_TOKEN` | internauth token for the ingest POST (config.go `MESSAGING_BOOKING_INTERNAL_TOKEN`) |

Enable with `WHATSAPP_MEDIA_ENABLED=true` **and**
`WHATSAPP_MEDIA_ANCHOR_ENABLED=true`, and confirm
`MESSAGING_BOOKING_INTERNAL_TOKEN` equals the booking block's
`BOOKING_INTERNAL_TOKEN` (the dev compose wires the mapping; prod via the
secrets store). See also `docs/integrations/messaging-channels.md`.
