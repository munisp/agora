# identity-service

Tenant provisioning and identity context for OpenDesk (SPEC §7 identity schema,
§8 AuthN/AuthZ). Go 1.23, chi router, pgx/v5 + pgxpool, zap logging.

## Responsibilities

- Public tenant context for agent session injection (name, timezone, currency,
  locale, terminology, plan) — consumed by the voice/conversation services and
  by booking-service's tenant resolver.
- Tenant provisioning: DB row + Keycloak group `/tenants/{slug}` + Permify
  tenant/relationships + `TenantProvisioned` CloudEvent on
  `opendesk.identity.events` via the Dapr pubsub component `pubsub-kafka`.
- Member invites: Keycloak user creation (+ group join), membership row,
  Permify relationship, `MemberInvited` CloudEvent.
- Idempotent internal endpoints for the `TenantOnboardingWorkflow`.

## Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/healthz` | Liveness (pings Postgres) |
| GET | `/v1/tenants/{slug}` | Public tenant context (incl. `id`) |
| POST | `/v1/tenants` | Provision a tenant |
| GET | `/v1/tenants/{slug}/members` | List memberships |
| POST | `/v1/tenants/{slug}/members` | Invite member (role owner\|admin\|staff\|viewer) |
| POST | `/internal/tenants/{slug}/ensure-group` | Idempotent Keycloak group creation (Temporal onboarding) |
| POST | `/internal/tenants/{slug}/ensure-permify` | Idempotent Permify tenant creation |

## Apps API (SPEC-W18)

Platform app registry (`internal/apps`): the 16-app catalog (`platform_apps`,
global reference, seeded from `internal/apps/catalog.yaml` via `go:embed` +
boot upsert) and per-tenant provisioning (`tenant_apps`,
`tenant_isolation` RLS). Operator walkthrough:
[docs/apps-platform.md](../../docs/apps-platform.md).

| Method | Path | Description |
|---|---|---|
| GET | `/v1/apps` | The app catalog (all 16 apps) |
| GET | `/v1/tenants/{slug}/apps` | Catalog LEFT JOIN tenant_apps — every app with `status` (or `not_provisioned`) + `config` |
| POST | `/v1/tenants/{slug}/apps/{app_id}` | Provision + enable (idempotent upsert; authenticated owner/admin) |
| PATCH | `/v1/tenants/{slug}/apps/{app_id}` | Partial update `{status?, config?}` (authenticated owner/admin) |
| DELETE | `/v1/tenants/{slug}/apps/{app_id}` | Soft disable — row kept for audit, data retained (authenticated owner/admin) |
| GET | `/internal/entitlements/check?app_id=` | Service-to-service entitlement check (`X-Tenant-ID` or `X-Tenant-Slug` header) |

**Authorization for mutations** (POST/PATCH/DELETE): an authenticated
subject — JWT `sub` from the `Authorization` bearer or the `X-User-Id`
header (the `twin.go` trust model) — holding Permify `manage_catalog`
(owner/admin) on the organization. `401` without a subject, `403` without
the permission, `502` when the authorization service is unreachable.

Catalog list (`GET /v1/apps` → `200 {"apps": [{...}, ...]}`) — one row:

```json
{
  "app_id": "receptionist",
  "name": "AI Receptionist",
  "version": "1.0.0",
  "description": "Voice + text AI concierge that answers questions and books, reschedules and cancels appointments live, with warm handoff to staff.",
  "category": "Communications",
  "icon": "📞",
  "nav_route": "/voice-agent",
  "required_perms": ["manage_bookings"],
  "default_plan_tier": "free",
  "backend_note": "services/voice-agent-runtime + services/conversation-service; escalation call UI at /call (booking-service)."
}
```

Tenant app list (`GET /v1/tenants/{slug}/apps` → `200 {"apps": [{...}]}`):
the catalog rows plus `status`
(`enabled|disabled|suspended|not_provisioned`) and `config` (`{}` when not
provisioned).

Provision + enable (`POST /v1/tenants/acme/apps/helpdesk` → `201` on first
provision, `200` on idempotent replay):

```json
{
  "tenant_id": "8f3d2c10-…",
  "app_id": "helpdesk",
  "status": "enabled",
  "config": {},
  "provisioned_at": "2025-01-01T12:00:00Z",
  "provisioned_by": "user:…",
  "updated_at": "2025-01-01T12:00:00Z"
}
```

Partial update (`PATCH` with `{"status":"disabled"}` or
`{"config":{"sla_hours":4}}` — `config` replaces the JSON document
wholesale → `200` + row). `DELETE` → `200` + row with `status=disabled`;
the row (and all app data) is retained, re-enabling reuses it.

Entitlement check (`GET /internal/entitlements/check?app_id=helpdesk` with
`X-Tenant-ID: <uuid>` or `X-Tenant-Slug: acme` — mesh-internal, deliberately
no auth middleware, same trust level as `/internal/consents/check`): `200`
for every **known** app, with denials carried in the body:

```json
{ "app_id": "helpdesk", "allowed": false, "reason": "disabled" }
```

`reason` ∈ `enabled|disabled|suspended|not_provisioned`. An **unknown**
`app_id` returns `404 {"error": "unknown app: …"}` — callers must treat
that as denied. Missing `app_id`/tenant header → `400`.

Lifecycle CloudEvents (`internal/apps/publisher.go`, via the `pubsub-kafka`
Dapr component on topic `opendesk.apps.lifecycle.v1`):
`com.opendesk.apps.AppProvisioned` on first provision,
`com.opendesk.apps.AppStatusChanged` on enable/disable/suspend transitions
(incl. re-enable and DELETE); an enabled→enabled replay publishes nothing.
Payload `{tenant_id, app_id, status, actor, ts}`.

## Environment variables

| Var | Default | Description |
|---|---|---|
| `PORT` | `7001` | HTTP listen port |
| `DATABASE_URL` | — (required) | Postgres DSN for the `identity` DB |
| `KEYCLOAK_URL` | `http://keycloak:8080` | Keycloak base URL |
| `KEYCLOAK_REALM` | `opendesk` | Realm |
| `KEYCLOAK_ADMIN_CLIENT_ID` | — | Admin client id (client_credentials) |
| `KEYCLOAK_ADMIN_CLIENT_SECRET` | — | Admin client secret |
| `PERMIFY_URL` | `http://permify:3476` | Permify HTTP API base |
| `DAPR_HOST` | `daprd-identity` | daprd sidecar host |
| `DAPR_HTTP_PORT` | `3500` | daprd HTTP port |
| `DAPR_PUBSUB_NAME` | `pubsub-kafka` | Dapr pubsub component |
| `IDENTITY_EVENTS_TOPIC` | `opendesk.identity.events` | Identity events topic |
| `APPS_LIFECYCLE_TOPIC` | `opendesk.apps.lifecycle.v1` | App lifecycle CloudEvents topic (SPEC-W18; `AppProvisioned`/`AppStatusChanged`) |
| `NOTIFICATION_APP_ID` | `notification` | Dapr app-id of notification-worker (fire-and-forget `POST /dev/trigger-onboarding` after provisioning starts the `TenantOnboardingWorkflow`) |
| `SHUTDOWN_TIMEOUT_SECONDS` | `15` | Graceful shutdown budget |

## Run

```bash
go build ./... && go test ./...
DATABASE_URL=postgres://opendesk:opendesk@localhost:5432/identity \
KEYCLOAK_ADMIN_CLIENT_ID=service-accounts KEYCLOAK_ADMIN_CLIENT_SECRET=... \
  ./server
# or
docker build -t opendesk/identity-service .
```

## Notes / deviations

- **Permify via HTTP API v1** (not gRPC): `POST /v1/tenants/{t}/permissions/check`
  and `/data/relationships/write`. Exported as the `permify.Authorizer`
  interface so checks are mockable; the same pattern is used by
  booking-service.
- Keycloak/Permify failures during `POST /v1/tenants` are logged and deferred
  to the durable `TenantOnboardingWorkflow` (which calls the idempotent
  `/internal/.../ensure-*` endpoints) instead of failing provisioning.
- Realm role `staff` maps to the Permify relation `member` (SPEC §8 schema
  relations: owner/admin/member/viewer).
- CloudEvents 1.0 envelope per SPEC §4: `{specversion, id, source, type,
  subject, time, tenantid, data}`.

## Digital twins (SPEC-W3 §3, innovation 12)

- `POST /internal/tenants/{slug}/twin` creates an ephemeral copy of the
  tenant: slug `{slug}-twin-{6rand}` (base truncated to fit the 63-char slug
  rule), industry/timezone/currency/locale/terminology copied, `plan='twin'`,
  `metadata={"twin_of": "<slug>"}`. Onboarding is triggered exactly like
  `POST /v1/tenants` (same `TenantOnboardingWorkflow`), and a
  `TwinCleanupWorkflow` is armed via notification-worker's
  `POST /dev/trigger-twin-cleanup` (24h timer → Dapr
  `DELETE /v1/tenants/{slug}`).
- `DELETE /v1/tenants/{slug}` deletes a tenant + its memberships.
  **Guard (permify-free by design):** slugs containing `-twin-` delete
  freely — the cleanup workflow calls over the private Dapr mesh and
  operators via the admin UI; every other slug requires the caller (JWT
  `sub` or `X-User-Id`) to hold `manage_catalog` on the organization
  (Permify check).
- **Cascade note:** only the identity rows (tenant + memberships) are
  removed. Twin data in booking/conversation/knowledge expires with the
  twin's 24h lifetime and is reclaimed by those services' own retention —
  twins are short-lived sandboxes, not production tenants.
