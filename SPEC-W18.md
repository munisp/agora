# SPEC-W18 — App Platform Foundation (Enterprise Apps Program, Wave 1 of 3)

Foundation for running enterprise apps on OpenDesk and for the management/admin
portal that provisions and manages them. Four builders, strict ownership.
Delivery protocol identical to SPEC-W12 §Delivery ($HOME workspaces — /tmp gets
wiped; additive rsync to /mnt; md5-verify FROM /mnt; real gate tails).
NOTE: W17 (seeding) is concurrently in flight in scripts/seeds, sql/, docs/,
infra/lakehouse, deploy/mojaloop, Makefile, infra/grafana, and ONE additive
identity-service consent change (Agent D there). W18 builders must NOT touch
scripts/seeds, sql/, Makefile, infra/grafana, deploy/mojaloop, docs/data-seeding.md,
or services/identity-service/internal/consent/.

## The app catalog this foundation ships (contract, binds everyone)
Existing platform apps (status: shipped backends, portal-manageable):
  receptionist (AI receptionist & booking), messaging (omnichannel),
  cac (customer acquisition), payments, kyc-compliance, analytics, incidents, geo-campaigns
New enterprise apps (catalog entries NOW, backends land W19/W20):
  helpdesk (SLA ticketing), field-service, loyalty-wallet, campaign-studio,
  crm-360, surveys-voc, lending, workforce
Each catalog row: {app_id, name, version, description, category, icon emoji/text,
nav_route, required_perms[], default_plan_tier starter|growth|scale, backend_note}.

## Cross-agent contracts
1. **identity-service owns the registry** (Agent A):
   - Tables (RLS tenant_isolation on tenant_apps; platform_apps is global reference,
     no RLS — document why): platform_apps(app_id PK, name, version, description,
     category, icon, nav_route, required_perms text[], default_plan_tier, backend_note,
     created_at) and tenant_apps(tenant_id, app_id, status enabled|disabled|suspended,
     config jsonb DEFAULT '{}', provisioned_at, provisioned_by, updated_at,
     PK(tenant_id, app_id)).
   - REST (auth'd via existing middleware; owner/admin for mutations):
     GET /v1/apps (catalog), GET /v1/tenants/{slug}/apps (catalog LEFT JOIN tenant_apps
     → every app with status|not_provisioned + config),
     POST /v1/tenants/{slug}/apps/{app_id} (provision+enable, idempotent upsert),
     PATCH /v1/tenants/{slug}/apps/{app_id} ({status?, config?} partial),
     DELETE /v1/tenants/{slug}/apps/{app_id} (soft → status disabled; row kept for audit).
   - Lifecycle CloudEvents on topic opendesk.apps.lifecycle.v1:
     com.opendesk.apps.AppProvisioned / AppStatusChanged (payload {tenant_id, app_id,
     status, actor, ts}) via the existing Dapr/outbox idiom in identity-service (inspect
     how consent events are published — mirror it; if identity has no publisher, use the
     direct Dapr client idiom from another Go service and document).
   - Entitlement check (service-to-service, X-Tenant-Slug internal pattern):
     GET /internal/entitlements/check?app_id= → {app_id, allowed bool, reason
     enabled|disabled|suspended|not_provisioned|unknown_app}. unknown_app → 404 shape
     {error} — callers treat as denied.
2. **Admin portal** (Agent B): admin-web section /app/[orgSlug]/apps:
   - Catalog grid (all 16 apps: icon, name, category, tier badge, status pill
     Enabled/Disabled/Suspended/Not provisioned), search + category filter.
   - Provision/Enable/Disable actions (owner/admin only — mirror existing role guards),
     confirm dialog for disable (warns: existing data retained).
   - Per-app config drawer: JSON editor with validation + friendly error surfacing
     (reuse the unwrap<T>() envelope convention from W13 — inspect
     app/app/[orgSlug]/cac/cac-client.tsx).
   - org-nav entry "apps" (mirror how "growth" was added in W14 — inspect org-nav).
   - BFF calls go through /api/identity/... (inspect how settings page calls identity
     today — SAME path style; W15-D found settings PATCHes /v1/tenants/{slug} which 405s
     — do NOT replicate that bug: only call endpoints Agent A actually implements).
3. **Catalog seed** (Agent C): Go-embedded seed in identity-service (mirror how packs
   load from industries/ — but apps catalog loads from a NEW
   services/identity-service/internal/apps/catalog.yaml embedded via go:embed, upserted
   into platform_apps at boot, idempotent) containing all 16 apps per the contract list.
4. **App developer guide** (Agent D): docs/app-developer-guide.md — how an app plugs in:
   manifest fields, entitlement gating pattern (BFF calls GET /internal/entitlements/check
   with 60s cache; UI hides nav when denied; backend returns 402/403 with reason), lifecycle
   events, config schema conventions, testing recipe. PLUS a reference implementation:
   booking-service ADDITIVE entitlement middleware helper
   (internal/appgate/gate.go: cached Dapr invoke of /internal/entitlements/check, fail-
   closed on 5xx per AUTHZ_OUTAGE_POLICY idiom, 402 Payment Required when
   reason=not_provisioned, 403 when disabled/suspended) + unit tests + ONE example wiring
   on an existing lightweight route (e.g. gate /v1/leads behind app_id "cac" — ADDITIVE,
   default-permissive config flag APP_GATE_ENABLED=false so prod behavior unchanged unless
   opted in; document loudly).

## Agent A — identity-service app registry
Owns: services/identity-service/internal/apps/ (NEW: model.go, store.go (RLS, mirror
consent store idioms), handlers.go, catalog.go (go:embed catalog.yaml, boot upsert),
catalog.yaml (Agent C SUPPLIES the 16-row content — code against contract §3 fields;
if C's yaml hasn't landed, write a 2-row placeholder and REPLACE it when it lands —
coordinate: check /mnt before final delivery), publisher.go (lifecycle CloudEvents),
apps_test.go + store_test.go (embedded-pg idiom from consent tests)), wiring ADDITIVE in
server.go/main.go/config.go (APPS_* envs if needed). Do NOT touch internal/consent/
(W17-D owns it concurrently). go build/vet/test green.

## Agent B — admin portal UI
Owns: apps/admin-web/app/app/[orgSlug]/apps/ (page.tsx + apps-client.tsx +
config-drawer.tsx), components/apps/ (app-card.tsx, status-pill.tsx, tier-badge.tsx),
org-nav additive entry, lib/api additive types if a shared types file exists (inspect).
Follow EXACTLY the cac/growth client idioms (unwrap envelopes, role guards, BFF path
style /api/identity/...). tsc --noEmit green, no package.json/lock changes.

## Agent C — catalog content + integration glue
Owns: services/identity-service/internal/apps/catalog.yaml (ALL 16 apps, contract §1
fields, honest backend_note per app: e.g. helpdesk "backend lands W19"; cac
"services/booking-service/internal/leads + analytics-pipeline"), docs/apps-platform.md
(operator doc: provisioning flows, entitlement model, tier mapping, lifecycle events,
how billing plans map to default_plan_tier — inspect services/billing-engine for plan
tiers and cite real tier names), .env.example ADDITIVE at repo root (APPS lifecycle
topic env) IF identity-service config adds envs (coordinate with A's report at the end;
check /mnt), services/identity-service/README.md ADDITIVE section (apps API reference:
endpoints, shapes, examples). If Agent A's handlers aren't in /mnt when you write the
README section, cite the SPEC contract paths and note "implementation: internal/apps".

## Agent D — developer guide + reference gate
Owns: docs/app-developer-guide.md, services/booking-service/internal/appgate/ (NEW
gate.go + gate_test.go — contract §4; inspect internal/httpapi middleware + Dapr invoke
idioms first; 60s cache with singleflight; APP_GATE_ENABLED config ADDITIVE in
config.go; example wiring on leads routes ADDITIVE in httpapi/server.go),
tests: httptest fake entitlement endpoint; cache behavior; fail-closed vs flag-off
permissive; 402 vs 403 mapping. go build/vet/test green for booking-service.

## Gates per builder: as delivery protocol. Independent verification gate follows.
