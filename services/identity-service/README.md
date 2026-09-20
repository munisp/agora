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
- Member invites: Keycloak user creation (+ group join + execute-actions
  e-mail when realm SMTP is live, K8/K10), membership row, Permify
  relationship, realm-role mirror via role-mappings (STK O4), `MemberInvited`
  CloudEvent (payload `tenant_slug,email,display_name,role,invited_by,
  invite_ts` — notification-worker sends the invite e-mail from it).
- Member lifecycle (SPEC-W45 K16): `DELETE`/`PATCH
  /v1/tenants/{slug}/members/{user_id}` — Keycloak disable + session
  revocation, Permify unlink/write, realm-role swap, `MemberRemoved` /
  `MemberRoleChanged` events. Admin-gated (Permify `manage_catalog`);
  owner-only for the owner role. Re-invite of an existing e-mail →
  `409 {"error":"already_invited_or_member","resend":true}` (STK O3).
- Tenant API keys (SPEC-W45 K17): `POST`/`GET`/`DELETE
  /v1/tenants/{slug}/api-keys` (owner/admin; full key `prefix.secret` shown
  once, SHA-256 hash stored) + `POST /internal/api-keys/validate`
  (X-Internal-Token) → `{tenant_slug,scopes}` for service middleware.
- Plan enforcement (SPEC-W45 K18): member cap per plan (free 3, pro 20,
  scale/enterprise/twin unlimited) enforced at invite time (403 with upgrade
  message); `PATCH /v1/tenants/{slug}/plan` (owner or platform-admin,
  audit-logged, `TenantPlanChanged` event).
- Idempotent internal endpoints for the `TenantOnboardingWorkflow`.

## Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/healthz` | Liveness (pings Postgres) |
| GET | `/v1/tenants/{slug}` | Public tenant context (incl. `id`) |
| POST | `/v1/tenants` | Provision a tenant |
| GET | `/v1/tenants/{slug}/members` | List memberships |
| POST | `/v1/tenants/{slug}/members` | Invite member (role owner\|admin\|staff\|viewer; `realm_roles` analyst\|billing platform-admin-only; 409 already_invited_or_member+resend on re-invite) |
| DELETE | `/v1/tenants/{slug}/members/{user_id}` | Remove member (K16: disable IdP user, revoke sessions, Permify unlink, `MemberRemoved`) |
| PATCH | `/v1/tenants/{slug}/members/{user_id}` | Change member role (K16: Permify + realm-role swap, `MemberRoleChanged`) |
| POST | `/v1/tenants/{slug}/api-keys` | Create tenant API key (K17; full key returned once) |
| GET | `/v1/tenants/{slug}/api-keys` | List keys (prefix/scopes/status only — never hashes) |
| DELETE | `/v1/tenants/{slug}/api-keys/{key_id}` | Revoke key (soft delete) |
| PATCH | `/v1/tenants/{slug}/plan` | Change plan (K18; owner or platform-admin, audit + `TenantPlanChanged`) |
| POST | `/internal/api-keys/validate` | Validate an API key → `{tenant_slug,tenant_id,scopes,key_id,prefix}` or 401 (X-Internal-Token, K2) |
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

## Event stream contracts (operator/tenant streams)

`opendesk.identity.events` and `opendesk.apps.lifecycle.v1` are PUBLIC
operator event streams (SPEC-W45 ORPH O9 — declared contract, resolving the
create-topics comment drift where the lifecycle topic was listed with
placeholder event names):

- **`opendesk.apps.lifecycle.v1`** — app-platform operator stream.
  Event types: `com.opendesk.apps.AppProvisioned`,
  `com.opendesk.apps.AppStatusChanged`. Payload `{tenant_id, app_id, status,
  actor, ts}` (CloudEvents 1.0 envelope, `tenantid` extension). Retention:
  Kafka default (7d). Consumers welcome: subscribe with your own consumer
  group; the schema is additive-stable (new fields may appear, existing
  fields never change meaning).
- **`opendesk.identity.events`** — tenant/member operator stream.
  Event types: `com.opendesk.identity.TenantProvisioned` `{tenant_id, slug,
  name, plan, industry}`; `com.opendesk.identity.MemberInvited`
  `{tenant_slug, email, display_name, role, invited_by, invite_ts}`
  (notification-worker invite e-mail, K8); `com.opendesk.identity.
  MemberRemoved` / `MemberRoleChanged` (K16); `com.opendesk.identity.
  TenantDeleted` `{tenant_slug, tenant_id, deleted_at, actor}` (K9 purge
  cascade — booking/conversation/graph-sync/crm-sync consume);
  `com.opendesk.identity.TenantPlanChanged` `{tenant_slug, tenant_id,
  old_plan, new_plan, actor, changed_at}` (K18). Same envelope/retention/
  additive-stability contract as above.

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
| `KC_REALM_SMTP_HOST` | — (unset = skip) | Realm SMTP bootstrap (K10): when set, PATCHes the realm `smtpServer` at boot (fail-soft warn) so Keycloak's own credentials e-mails (K8 execute-actions) fire |
| `KC_REALM_SMTP_PORT` | `587` | Realm SMTP port |
| `KC_REALM_SMTP_FROM` | — | Realm SMTP sender address |
| `KC_REALM_SMTP_USER` | — | Realm SMTP auth user |
| `KC_REALM_SMTP_PASSWORD` | — | Realm SMTP auth password |
| `PORTAL_SECRET` | — (unset = portal path off) | Booking portal JWT HMAC secret (shared with booking-service). Lets data subjects self-serve consent data-access/erasure with their portal session (STK O13) |
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
  relations: owner/admin/member/viewer). Since SPEC-W45 (STK O4) the
  membership roles admin/staff/viewer are ALSO mirrored to same-named
  Keycloak realm roles via role-mappings at invite/role-change time
  (fail-soft — Permify stays the authorization source of truth); the
  functional realm roles `analyst`/`billing` are grantable at invite time by
  platform-admins only (`realm_roles` field).
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
  **Guard:** tenants flagged `is_twin=true` delete freely (the cleanup
  workflow calls over the internal token; SPEC-W44 W-I-3 — the old
  slug-substring heuristic is gone); every other slug requires the caller
  (JWT `sub` or `X-User-Id`) to hold `manage_catalog` on the organization
  (Permify check).
- **Cascade note (SPEC-W45 K9):** deletion (both the guarded `/v1` path and
  the internauth `/internal` path — `deleteTenantInternal` is shared by twin
  cleanup and admin delete) now (1) removes the identity rows (tenant +
  memberships; `tenant_api_keys` via FK cascade), (2) publishes
  `com.opendesk.identity.TenantDeleted` `{tenant_slug, tenant_id,
  deleted_at, actor}` on `opendesk.identity.events` — booking-service
  (anonymize contacts, cancel open bookings), conversation-service (purge
  sessions/history), graph-sync (delete tenant subgraph) and crm-sync
  (disable sync_map entries) consume it —, (3) deletes the Keycloak group
  `/tenants/{slug}`, and (4) deletes the Permify tenant. Steps 2–4 are
  best-effort: failures are logged at Error and surfaced in the response
  `warnings` array, never silently swallowed.
