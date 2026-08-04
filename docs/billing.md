# Billing Engine (SPEC-W7 Part B)

`services/billing-engine` — Rust (axum 0.7), port **7012**. Usage metering,
rating/invoicing, QR payments (Paystack or static EMV), and dunning for the
Agora platform.

## Architecture

```
booking-service ──(outbox)──> opendesk.usage.events ──> billing-engine consumer (group: billing-engine)
                                                              │  idempotent via processed_events(event_id)
                                                              ▼
                                                   Postgres `billing` DB
                                                   usage_records / rate_cards / invoices
                                                              │
   admin-web ──(/api/billing/* via APISIX, OIDC)──> REST ─────┤  generate → issue → QR payment
                                                              │
   Paystack ──(/webhooks/paystack, public)──> HMAC verify ────┤  charge.success → paid
                                                              ▼
                                              opendesk.billing.events (com.opendesk.billing.InvoicePaid)
                                              sim ledger: DR/CR double-entry (codes 200/201/202)
```

- **System of record:** Postgres `billing` database (sqlx 0.7 pool). Schema in
  `services/billing-engine/migrations/0001_init.sql`, embedded and applied
  idempotently at boot (same pattern as notification-worker). The database
  itself is created by `infra/postgres/init-scripts/00-create-dbs.sql`;
  least-privilege role `app_billing`/`app_billing_login` in `05-app-roles.sql`.
- **Ledger:** in-memory double-entry sim ledger behind the `BillingLedger`
  trait (same ADR-0007 pattern as payments-service; a TigerBeetle backend can
  be dropped in later). Accounts/codes: `200` = AR-control
  (`tenant:{id}:ar`), `201` = revenue (`tenant:{id}:revenue`), `202` =
  payments-clearing (`platform:billing:clearing`).
  - invoice **issued** → DR AR-control / CR revenue (code 200)
  - invoice **paid** → DR payments-clearing / CR AR-control (code 202)
  - postings are idempotent by transfer id derived from the invoice id.
- **Events:** rdkafka producer, topic `opendesk.billing.events`
  (CloudEvents 1.0 envelope, `tenantid` extension attribute). Produce-only —
  notification-worker consumes it later.

## Rate-card plans

`plan_presets` (seeded by the migration) describe the platform tiers, mirrored
by the marketing site's pricing page. Tenants get concrete rows in
`rate_cards` via `PUT /v1/rate-cards/{tenant_id}` (copy a preset and adjust,
or set custom prices). Only metrics with a rate-card row are billed; other
usage stays metered but free.

| plan | metric | unit price (cents) | included quota / month |
| --- | --- | --- | --- |
| free | booking | 0 | 100 |
| free | call_minutes | 0 | 100 |
| free | message | 0 | 1,000 |
| standard | booking | 50 | 1,000 |
| standard | call_minutes | 5 | 500 |
| standard | message | 1 | 10,000 |
| pro | booking | 25 | 10,000 |
| pro | call_minutes | 2 | 5,000 |
| pro | message | 1 | 100,000 |

**Rating rule** (per metric line item): `billable = max(0, total −
included_quota)`; `amount = billable × unit_price_cents`. All money is integer
minor units — no floats, no rounding drift.

## Invoice lifecycle

```
draft ──issue──> issued ──webhook──> paid
  │                │  └─dunning (older than INVOICE_DUE_DAYS)──> past_due ──webhook──> paid
  └──void──> void  └──void──> void                                └──void──> void
```

- `POST /v1/invoices/generate {tenant_id, period:"YYYY-MM"}` aggregates
  `usage_records` for the month window and creates a **draft**. Regenerating
  replaces the draft in place (same invoice id). A partial unique index
  `(tenant_id, period) WHERE status <> 'void'` guarantees one active invoice
  per tenant/period — a second generate after issue returns **409**.
- Dunning sweep every `DUNNING_INTERVAL_S` (default 3600s, min 60) flips
  `issued` invoices older than `INVOICE_DUE_DAYS` (default 14) to `past_due`.
- `paid` and `void` are terminal.

## QR payment flows

### Paystack mode (`PAYSTACK_SECRET_KEY` set)

1. `POST /v1/invoices/{id}/payment-link` (invoice must be `issued`/`past_due`)
   calls Paystack `POST /transaction/initialize` with `{email, amount (kobo),
   reference: <invoice_id>, callback_url, metadata}` and stores the returned
   `authorization_url` as the invoice's `payment_ref`.
2. `GET /v1/invoices/{id}/qr` renders that URL as an SVG QR
   (`image/svg+xml`), scannable by any banking app / Paystack checkout.
3. After payment, Paystack POSTs `charge.success` to
   `https://<gateway>/webhooks/paystack`.

### Static mode (no secret key)

The payment link is an **EMVCo-like merchant-presented payload** built from
`BILLING_STATIC_ACCOUNT` (account) and `BILLING_MERCHANT_NAME`: TLV fields
00/01/26/52/53/54/58/59/62, terminated by `6304` + CRC16-CCITT (init `0xFFFF`,
poly `0x1021`, uppercase hex). The payload embeds the amount (major units) and
the invoice id as reference (62/05). It is stored as `payment_ref` and
rendered by the same `/qr` endpoint. Settlement reconciliation in static mode
is manual (the payload carries the invoice reference).

### Webhook setup (Paystack dashboard)

- Webhook URL: `https://<your-gateway-host>/webhooks/paystack`
  (APISIX route `webhooks-paystack`, public, rate-limited; exact URI wins over
  the `/webhooks/*` messaging wildcard).
- Secret key: same value as `PAYSTACK_SECRET_KEY`.
- Verification: `x-paystack-signature` = lowercase hex HMAC-SHA512 of the raw
  request body under the secret key, compared in constant time; mismatch →
  **401**. (`services/billing-engine/src/payments_qr.rs` implements
  HMAC-SHA512 in-crate — the dependency budget allows no extra crypto crates.)
- On `charge.success` with a reference matching an invoice id: idempotent
  `issued|past_due → paid` transition (already-paid → `200
  {"status":"already_paid"}`), ledger clearing posting, and a
  `com.opendesk.billing.InvoicePaid` CloudEvent. Unknown references are
  acked (`200 {"status":"ignored"}`) so Paystack stops retrying.
- Local test of the signature scheme (known vector used in the unit tests):

  ```sh
  python3 - <<'EOF'
  import hmac, hashlib
  secret = "sk_test_0123456789abcdef0123456789abcdef"
  body = '{"event":"charge.success","data":{"reference":"9b0b0d52-1c8b-4d3f-9e2a-6f6a2b7c1d20","amount":125000,"currency":"NGN","status":"success"}}'
  print(hmac.new(secret.encode(), body.encode(), hashlib.sha512).hexdigest())
  # 683c86c3dad9b20fe26cd1e35511d249972a63057b42b562b318161a322e9161...
  EOF
  ```

## Role matrix

Authentication flows through APISIX: `/api/billing/*` requires a Keycloak
Bearer token (openid-connect `bearer_only`, same pattern as `/api/payments/*`).
The service then enforces:

| Route | X-Tenant-ID match | realm role (`x-user-roles`) |
| --- | --- | --- |
| `PUT /v1/rate-cards/{tenant_id}` | required | any authenticated |
| `POST /v1/invoices/generate` | required | **owner or admin** (403 otherwise) |
| `GET /v1/invoices*` | required | any authenticated |
| `POST /v1/invoices/{id}/issue` | required | any authenticated |
| `POST /v1/invoices/{id}/void` | required | **owner or admin** (403 otherwise) |
| `POST /v1/invoices/{id}/payment-link`, `GET /v1/invoices/{id}/qr` | required | any authenticated |
| `POST /webhooks/paystack` | exempt | exempt (HMAC signature instead) |

`X-Tenant-ID` must equal the tenant that owns the resource (403 on
missing/malformed/mismatch). Dashboard-level gating (owner/billing realm
roles, Permify `view_billing`) is handled by admin-web (SPEC-W7 Part C); the
checks above are the service-side enforcement floor.

## API reference

Base path through the gateway: `/api/billing` (rewritten to `/` upstream).
Direct: `http://localhost:7012`.

### `PUT /v1/rate-cards/{tenant_id}`
```json
{ "metric": "booking", "unit_price_cents": 50, "included_quota": 1000, "currency": "USD" }
```
Upserts one row (PK `(tenant_id, metric)`). → `200` rate card.

### `POST /v1/invoices/generate`
```json
{ "tenant_id": "<uuid>", "period": "2026-03" }
```
→ `201` invoice (draft), or `409` when a non-draft invoice already exists for
the period, `400` on a malformed period.

### `GET /v1/invoices?tenant_id=<uuid>&status=issued`
→ `200` invoice list (status filter optional;
`draft|issued|paid|void|past_due`).

### `GET /v1/invoices/{id}` → `200` invoice | `404`.

### `POST /v1/invoices/{id}/issue`
draft → issued, sets `issued_at`, posts DR AR / CR revenue. → `200` invoice |
`409` illegal transition.

### `POST /v1/invoices/{id}/void`
draft/issued/past_due → void (owner/admin only). → `200` invoice | `409`.

### `POST /v1/invoices/{id}/payment-link`
```json
{ "email": "customer@example.com", "callback_url": "https://..." }
```
(body optional; defaults from env). Requires `issued`/`past_due` (else `409`).
Paystack mode → `200 { "mode": "paystack", "reference", "authorization_url" }`.
Static mode → `200 { "mode": "static", "reference", "payload" }`.

### `GET /v1/invoices/{id}/qr`
→ `200 image/svg+xml` QR of the stored payment ref | `404` before a payment
link exists.

### `POST /webhooks/paystack`
Headers: `x-paystack-signature`. Body: Paystack event JSON. → `200
{"status":"paid"|"already_paid"|"ignored"}` | `401` bad signature | `503`
when `PAYSTACK_SECRET_KEY` is unset.

### Invoice object
```json
{
  "id": "uuid", "tenant_id": "uuid", "period": "2026-03",
  "status": "issued", "subtotal_cents": 125000, "currency": "USD",
  "line_items": [
    { "metric": "booking", "quantity": 3500, "included_quota": 1000,
      "billable": 2500, "unit_price_cents": 50, "amount_cents": 125000 }
  ],
  "payment_ref": "https://checkout.paystack.com/...", 
  "created_at": "...", "issued_at": "...", "paid_at": null
}
```

## Operations

- **Compose:** `docker compose up billing-engine` (service block in
  `docker-compose.yml`; env: `DATABASE_URL` with `BILLING_PG_USER/PASS`
  override, `KAFKA_BROKERS`, `PAYSTACK_SECRET_KEY`, `BILLING_STATIC_ACCOUNT`,
  `DUNNING_INTERVAL_S`, `INVOICE_DUE_DAYS`).
- **Health:** `GET /healthz` returns mode (`paystack`/`static`) and event
  publish counters.
- **Consumer lag:** group `billing-engine` on `opendesk.usage.events`;
  redeliveries are safe (processed_events + deterministic ledger ids).
- **Tests:** `cargo test` — rating math (quota boundary, exact integer
  multiplication, saturation), period parsing, CRC16 check value, EMV payload
  known vector, SHA-512/HMAC RFC 4231 + Paystack known vectors, invoice state
  machine, ledger conservation/idempotency.
