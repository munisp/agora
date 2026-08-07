# Apps Platform — Operator Guide (SPEC-W18)

How enterprise apps are modeled, provisioned, entitled and observed on
Agora. The registry lives in **identity-service** (`internal/apps`,
shipped in W18; catalog content in
`services/identity-service/internal/apps/catalog.yaml`).

---

## 1. App model

Two tables in the `identity` database:

| Table | Scope | RLS | Purpose |
|---|---|---|---|
| `platform_apps` | **global** reference data | **none** (deliberate: the catalog is identical for every tenant — it is seeded from an embedded YAML at boot and carries no tenant data, so `tenant_isolation` RLS would add nothing but join overhead) | the 16-app catalog |
| `tenant_apps` | per-tenant | `tenant_isolation` RLS (same policy idiom as the consent tables) | one row per (tenant, app) once provisioned |

`platform_apps` columns: `app_id` (PK), `name`, `version`, `description`,
`category`, `icon`, `nav_route`, `required_perms text[]`,
`default_plan_tier`, `backend_note`, `created_at`.

`tenant_apps` columns: `tenant_id`, `app_id`, `status`
(`enabled|disabled|suspended`), `config jsonb DEFAULT '{}'`,
`provisioned_at`, `provisioned_by`, `updated_at`, PK `(tenant_id, app_id)`.

The catalog YAML is embedded with `go:embed` and **upserted at boot**
(idempotent — safe to restart; catalog edits ship with the binary, no
migration needed for content changes).

### The 16 apps

Existing platform apps (backends shipped): `receptionist`, `messaging`,
`cac`, `payments`, `kyc-compliance`, `analytics`, `incidents`,
`geo-campaigns`.
New enterprise apps (catalog entries now, backends land W19/W20):
`helpdesk`, `field-service`, `loyalty-wallet`, `campaign-studio`, `crm-360`,
`surveys-voc`, `lending`, `workforce`.

`nav_route` is the portal route **relative to `/app/{orgSlug}`**. Apps whose
dedicated portal section has not landed yet point at the planned
`/apps/<app_id>` route and say so in `backend_note`; they are still
provisionable and appear in the `/apps` catalog grid.

---

## 2. Provisioning lifecycle

```
                 POST /v1/tenants/{slug}/apps/{app_id}
   not_provisioned ───────────────────────────────► enabled
                                                       │  PATCH {status}
                              ┌────────────────────────┤
                              ▼                        ▼
                          disabled ◄──PATCH──► suspended
                              │
                              ► re-enable: PATCH {status: enabled}
                              ► DELETE = soft disable (see below)
```

- **Provision** (`POST`) is an idempotent upsert: first call creates the
  `tenant_apps` row with `status=enabled`, repeat calls return the existing
  row. Safe to retry from the portal or a script.
- **Enable / disable / suspend** (`PATCH {status}`) flips the status.
  `suspended` is platform-initiated (e.g. billing hold); `disabled` is
  tenant-initiated.
- **DELETE is a soft disable**: it sets `status=disabled` and **keeps the
  row** (with `config`, `provisioned_at`, `provisioned_by`) for audit.

### Data retention on disable

Disabling or suspending an app **never deletes the app's data**. Rows the
app wrote in its backing service (bookings, leads, invoices, KYC
references, …) stay put; only the entitlement flips, so API calls start
failing closed (403). Re-enabling restores access exactly where it left
off. Actual data deletion is a separate, explicit path (GDPR/NDPA erasure —
see `docs/compliance-ndpa.md` and the consent erasure flow), never a side
effect of an app toggle.

---

## 3. Entitlement check flow (three-layer gating)

Entitlement is enforced at three independent layers — any one of them alone
is not sufficient:

1. **BFF / service-to-service** — before proxying or invoking an app's
   backend, the caller checks:

   ```
   GET /internal/entitlements/check?app_id=<app_id>
   X-Tenant-ID: <uuid>            (or X-Tenant-Slug: <slug> — existing
                                   service-to-service pattern)
   → 200 { "app_id": "...", "allowed": true|false,
           "reason": "enabled|disabled|suspended|not_provisioned" }
                                   (200 for every KNOWN app — denials are
                                   carried in the body, not the status code)
   → 404 { "error": "..." }       unknown app_id — callers MUST treat as denied
   → 400 { "error": "..." }       missing app_id or tenant header
   ```

   Callers cache the answer for **60 s** (singleflight) and **fail closed on
   5xx**, per the `AUTHZ_OUTAGE_POLICY` idiom used for Permify outages
   (see `services/booking-service/internal/httpapi/authz_outage_test.go`).
2. **UI** — admin-web hides the nav entry and catalog actions when the
   entitlement read says the app is denied/not provisioned, and badges the
   status pill (Enabled/Disabled/Suspended/Not provisioned). Hiding is a
   courtesy, never the enforcement.
3. **Backend** — the app's own middleware (reference implementation:
   booking-service `internal/appgate`, W18 Agent D) maps the reason to a
   status code: `402 Payment Required` when `not_provisioned`, `403
   Forbidden` when `disabled`/`suspended`, and includes the `reason` in the
   error body so the portal can render a targeted call-to-action.

---

## 4. Lifecycle CloudEvents

Published by identity-service via the existing Dapr pubsub component
`pubsub-kafka` (same publisher idiom as the consent erasure events in
`internal/consent`).

| | |
|---|---|
| **Topic** | `opendesk.apps.lifecycle.v1` |
| **Types** | `com.opendesk.apps.AppProvisioned`, `com.opendesk.apps.AppStatusChanged` |
| **Payload** | `{ "tenant_id": "<uuid>", "app_id": "<slug>", "status": "enabled|disabled|suspended", "actor": "<user-or-service>", "ts": "<rfc3339>" }` |

`AppProvisioned` fires only on the first successful provision.
`AppStatusChanged` fires on every transition that actually changes `status`
(PATCH enable/disable/suspend, DELETE, and re-enable via re-POST); an
enabled→enabled idempotent replay publishes nothing. Consumers (W19+:
notification-worker digests, billing entitlement sync, audit lakehouse)
should key idempotency on `(tenant_id, app_id, status, ts)`.

---

## 5. Plan tiers and the tier→app mapping

The catalog's `default_plan_tier` uses the **real billing-engine plan
names** seeded in `services/billing-engine/migrations/0001_init.sql`
(`plan_presets`): `free`, `standard`, `pro`. The SPEC-W18 contract names
map onto them as **starter→free, growth→standard, scale→pro** (identity
tenants also default to `plan="free"` — see
`internal/httpapi/server.go`). `default_plan_tier` is the tier at which the
app is included **by default**; it is a catalog default, not an enforcement
— entitlement is decided solely by the `tenant_apps` row (§3), so sales can
grant any app to any tenant by provisioning it.

| Plan (billing-engine) | Contract name | Apps included by default |
|---|---|---|
| `free` | starter | receptionist, messaging |
| `standard` | growth | cac, payments, kyc-compliance, analytics, helpdesk, surveys-voc |
| `pro` | scale | incidents, geo-campaigns, field-service, loyalty-wallet, campaign-studio, crm-360, lending, workforce |

Higher tiers include everything below them (pro = all 16; standard = free ∪
standard rows).

---

## 6. Per-app config conventions

- `tenant_apps.config` is a free-form `jsonb` object, default `{}`. It is
  the tenant's instance config for that app (feature flags, thresholds,
  provider overrides) — never secrets (those live in SOPS/Vault, see
  `docs/runbooks/secrets.md`).
- Update it with `PATCH /v1/tenants/{slug}/apps/{app_id}` —
  `{ "config": { ... } }` **replaces** the object wholesale (partial-PATCH
  semantics apply to the row, not inside the JSON document); send the full
  desired config.
- The portal edits config through a JSON editor drawer with client-side
  JSON validation; server-side schema validation per app lands with the
  app's backend in W19/W20. Keep keys `snake_case` to match the platform's
  API conventions.

---

## 7. Runbook: provision an app for a tenant

All operator calls go **through the BFF** (`/api/identity/*` → APISIX route
`api-identity` strips the prefix → `identity:7001`), exactly like the
portal's settings pages do. With a browser session the BFF attaches the
Keycloak token for you; from a shell, get a client-credentials token first
(mutations require an authenticated subject — JWT `sub` or `X-User-Id`,
the `twin.go` trust model — holding Permify `manage_catalog`, i.e.
**owner/admin**, on the organization; `401`/`403`/`502` otherwise):

```bash
TOKEN=$(curl -s http://localhost:8080/realms/opendesk/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=service-accounts \
  -d client_secret="$KEYCLOAK_ADMIN_CLIENT_SECRET" | jq -r .access_token)
H="Authorization: Bearer $TOKEN"
B=http://localhost:9080/api/identity   # APISIX; via the Next BFF use http://localhost:3000/api/identity
```

1. **List the catalog** (all 16 apps):

   ```bash
   curl -s -H "$H" $B/v1/apps | jq '.apps[0]'
   # { "app_id": "receptionist", "name": "AI Receptionist", "version": "1.0.0",
   #   "category": "Communications", "icon": "📞", "nav_route": "/voice-agent",
   #   "required_perms": ["manage_bookings"], "default_plan_tier": "free",
   #   "backend_note": "services/voice-agent-runtime + ..." }
   ```

2. **List a tenant's apps** (catalog LEFT JOIN tenant_apps — every app, with
   `status` or `not_provisioned`, plus `config`):

   ```bash
   curl -s -H "$H" $B/v1/tenants/acme/apps | jq '.apps[] | {app_id, status}'
   ```

3. **Provision + enable** (idempotent — rerun freely):

   ```bash
   curl -s -X POST -H "$H" $B/v1/tenants/acme/apps/helpdesk | jq .
   # 201 on first provision, 200 on idempotent replay:
   # { "tenant_id": "…", "app_id": "helpdesk", "status": "enabled",
   #   "config": {}, "provisioned_at": "…", "provisioned_by": "…" }
   ```

4. **Set config / change status**:

   ```bash
   curl -s -X PATCH -H "$H" -H 'Content-Type: application/json' \
     -d '{"config":{"sla_hours":4,"queue":"support"}}' \
     $B/v1/tenants/acme/apps/helpdesk | jq .

   curl -s -X PATCH -H "$H" -H 'Content-Type: application/json' \
     -d '{"status":"disabled"}' $B/v1/tenants/acme/apps/helpdesk | jq .
   ```

5. **Soft-delete (disable, row kept for audit)**:

   ```bash
   curl -s -X DELETE -H "$H" $B/v1/tenants/acme/apps/helpdesk | jq .
   # 200 with the row (status=disabled) — data retained; re-enable any
   # time with PATCH {"status":"enabled"}
   ```

6. **Entitlement check** (service-to-service only — the `/internal/*`
   endpoints are not gateway-routed, and service ports are not host-published
   since W34 GF4, so probe inside the compose network):

   ```bash
   docker compose exec apisix curl -s -H 'X-Tenant-Slug: acme' \   # or X-Tenant-ID: <uuid>
     'http://identity:7001/internal/entitlements/check?app_id=helpdesk' | jq .
   # { "app_id": "helpdesk", "allowed": false, "reason": "disabled" }
   ```

---

## 8. Permissions referenced by the catalog

`required_perms` strings that are **enforced in middleware today**
(`services/booking-service/internal/httpapi/server.go`): `manage_bookings`
(bookings/contacts/leads/geo/dispatch/privacy mutations), `manage_catalog`
(offerings, team, availability, site), `view_analytics` (dashboard reads:
leads/campaigns/referrals/payouts/devices). `manage_billing` is currently
enforced as the `owner|billing` realm-role guard in admin-web
(`apps/admin-web/lib/roles.ts`, `BILLING_ROLES`) and becomes a first-class
middleware perm with the W19 entitlement-gate rollout. Portal mutations of
apps themselves (provision/PATCH/DELETE) are guarded by the existing
identity middleware: **owner/admin only**.
