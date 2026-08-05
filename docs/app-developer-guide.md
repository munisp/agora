# App Developer Guide — Building Enterprise Apps on Agora

Audience: teams building enterprise apps that plug into the Agora app
platform (SPEC-W18 foundation; W19/W20 app backends). This guide covers the
catalog manifest, the entitlement model, the three-layer gating pattern,
lifecycle events, per-tenant app config conventions, and a testing recipe.

A **reference implementation** ships in booking-service:
`services/booking-service/internal/appgate/` (middleware + tests), wired
additively on `/v1/leads` behind catalog app `"cac"`.

---

## 1. The app catalog manifest

Every app — platform or enterprise — has one row in the catalog
(`platform_apps` table in identity-service, seeded at boot from
`services/identity-service/internal/apps/catalog.yaml`, idempotent upsert).
The manifest fields (contract, SPEC-W18 §1):

| Field              | Type     | Notes                                                                 |
| ------------------ | -------- | --------------------------------------------------------------------- |
| `app_id`           | string PK | Stable snake-case id, e.g. `helpdesk`. Never reused or renamed.       |
| `name`             | string   | Display name shown in the admin portal catalog grid.                  |
| `version`          | string   | Manifest/backend version (informational; bump on breaking changes).   |
| `description`      | string   | One-liner for the catalog card.                                       |
| `category`         | string   | Grouping used by the portal category filter.                          |
| `icon`             | string   | Emoji or short text token for the catalog card / nav.                 |
| `nav_route`        | string   | Portal route the app lives at, e.g. `/app/[orgSlug]/helpdesk`.        |
| `required_perms`   | string[] | Permify permissions the app needs (e.g. `manage_bookings`).           |
| `default_plan_tier`| enum     | `starter` \| `growth` \| `scale` — plan tier the app is bundled with. |
| `backend_note`     | string   | Honest pointer to the backend (e.g. `services/helpdesk-service`, or "backend lands W19"). |

To add a new app: add the row to `catalog.yaml` (identity-service owns the
registry — open a PR against that file; it is upserted at boot, so existing
deployments pick it up on restart). Catalog rows are global reference data;
per-tenant state lives in `tenant_apps` (see §2).

## 2. Entitlement model

Per-tenant app state is a `tenant_apps` row: `(tenant_id, app_id)` →
`status` (`enabled` | `disabled` | `suspended`) + `config` jsonb, with RLS
tenant isolation. An app with **no row** is `not_provisioned`.

Tenant-facing reads (authenticated, for the portal/BFF):

```
GET /v1/tenants/{slug}/apps
→ every catalog app with status ("enabled"|"disabled"|"suspended"|"not_provisioned") + config
```

Service-to-service check (the contract every gate codes against):

```
GET /internal/entitlements/check?app_id=<id>
Header: X-Tenant-Slug: <tenant slug>          (platform internal-call pattern)

200 → {"app_id": "helpdesk", "allowed": true,  "reason": "enabled"}
200 → {"app_id": "helpdesk", "allowed": false, "reason": "disabled" | "suspended" | "not_provisioned"}
404 → {"error": "..."}                          (unknown app_id — callers MUST treat as denied)
```

Reached from another service via Dapr service invocation:

```
GET http://<daprd>:3500/v1.0/invoke/identity/method/internal/entitlements/check?app_id=<id>
```

## 3. Three-layer entitlement gating

Gate at **all three layers**. Each layer alone is insufficient: UI hiding is
cosmetic, the BFF is bypassable, and the backend check without UI hiding is
a bad UX (users click into dead ends).

### Layer 1 — UI: hide nav when denied

The portal nav only renders an app's `nav_route` when its status is
`enabled`:

```tsx
// admin-web: fetch once per org page load via the BFF
const apps = await unwrap<TenantApp[]>(fetch(`/api/identity/v1/tenants/${orgSlug}/apps`));
const visible = apps.filter(a => a.status === "enabled");
// render nav entries only for `visible` (match on app_id / nav_route)
```

Denied apps are hidden, not disabled-with-tooltip — entitlement is not a
teaser surface. The admin catalog (`/app/[orgSlug]/apps`) is the only place
non-enabled apps are visible, and only to owner/admin.

### Layer 2 — BFF guard

The BFF route guarding the app's API surface calls the entitlement check
with a **60s cache** and maps denials before proxying:

```ts
// /api/<app>/[...path]/route.ts (pattern)
const ent = await cachedEntitlementCheck(orgSlug, appId); // 60s TTL cache
if (!ent.allowed) {
  const status = ent.reason === "not_provisioned" ? 402 : 403;
  return Response.json({ error: `app ${appId} not enabled`, app_id: appId, reason: ent.reason }, { status });
}
// else proxy to the backend service
```

### Layer 3 — backend middleware (reference implementation)

The backend enforces independently — BFFs are not a security boundary.
booking-service ships the reference middleware,
`internal/appgate` (SPEC-W18 contract §4):

- Dapr-invokes `GET /internal/entitlements/check` with the `X-Tenant-Slug`
  internal header.
- Caches decisions per (tenant, app) for `APP_GATE_CACHE_TTL_SECONDS`
  (default 60s) with **singleflight** — a cache miss fans out to exactly one
  upstream call even under concurrency.
- Failure policy (mirrors the `AUTHZ_OUTAGE_POLICY=fail_closed` idiom):

  | Upstream result                              | Gate response                                             |
  | -------------------------------------------- | -------------------------------------------------------- |
  | `allowed: true` (`enabled`)                  | request proceeds                                         |
  | `reason: not_provisioned`                    | **402** `{error, app_id, reason}`                        |
  | `reason: disabled` / `suspended`             | **403** `{error, app_id, reason}`                        |
  | unknown app (identity 404 `{error}`)         | **403** `{error, app_id, reason: "unknown_app"}`         |
  | 5xx / timeout / transport error              | **503** `{error, app_id, reason: "entitlement_unavailable"}` + `Retry-After: 5` — fail **closed**, never cached |

- **`APP_GATE_ENABLED=false` is the DEFAULT**: the middleware is then a pure
  pass-through — no upstream call, zero behavior change. Production behavior
  is unchanged unless a deployment explicitly opts in. This is deliberate:
  gating rolls out per environment, not with the binary.

Wiring (chi), exactly as done for `/v1/leads` behind app `"cac"`:

```go
appGate := appgate.New(appgate.Options{
    Enabled:       cfg.AppGateEnabled,      // APP_GATE_ENABLED, default false
    IdentityAppID: cfg.IdentityAppID,       // Dapr app-id of identity-service
    BaseURL:       fmt.Sprintf("http://%s:%d", cfg.DaprHost, cfg.DaprHTTPPort),
    CacheTTL:      cfg.AppGateCacheTTL,     // APP_GATE_CACHE_TTL_SECONDS, default 60s
    Logger:        logger,
})
// Prefer the tenant resolved by your tenant middleware over the raw header:
appGate.SetTenantSlugFunc(func(r *http.Request) string {
    if t := tenantFrom(r.Context()); t.Slug != "" { return t.Slug }
    return r.Header.Get("X-Tenant-Slug")
})

r.Route("/v1/leads", func(r chi.Router) {
    r.Use(appGate.Middleware("cac")) // one app_id per route group
    // ...routes
})
```

Rules for your own service: resolve the `app_id` **per route group** at
wiring time (never from request input), run the gate *after* tenant
resolution, and keep the opt-in flag default-off until your rollout plan
says otherwise.

## 4. Lifecycle events

identity-service publishes CloudEvents on topic **`opendesk.apps.lifecycle.v1`**
(via the existing Dapr/outbox idiom) whenever tenant app state changes:

| Event type                          | Fired when                                   |
| ----------------------------------- | -------------------------------------------- |
| `com.opendesk.apps.AppProvisioned`  | App provisioned+enabled for a tenant (POST)  |
| `com.opendesk.apps.AppStatusChanged`| Status patched or app disabled (PATCH/DELETE)|

Payload: `{"tenant_id", "app_id", "status", "actor", "ts"}`.

Use these to react eagerly instead of waiting out the 60s cache: refresh
portal nav, warm/invalidate local entitlement caches, kick off per-app
tenant onboarding (migrations, seed data) on `AppProvisioned`. Subscribe via
a Dapr pubsub subscription like any other platform topic. Note the platform
guarantee is eventual: gates may enforce a stale decision for up to one
cache TTL (60s) after a status change — design UIs accordingly.

## 5. Per-tenant app config (`config` jsonb)

`tenant_apps.config` is a free-form jsonb column (DEFAULT `'{}'`) holding
per-tenant settings for your app. Conventions:

- **Own your schema.** Only your app reads/writes its config shape; the
  platform stores it opaquely. Document the keys in your app's README.
- **Validate on write.** The admin portal config drawer is a JSON editor
  with client-side validation; the identity PATCH endpoint stores what it is
  given — so your app must re-validate on read. Reject or default unknown/
  malformed keys; never crash on a missing key.
- **Apply defaults in code**, not by backfilling rows. `config` for a fresh
  provisioning is `{}`; your service should behave sanely with it.
- **Version explicitly.** If you change the shape, carry a `"v": 1` key (or
  key the shape off your manifest `version`) and migrate on read.
- **No secrets.** Config is visible to tenant owner/admin and flows through
  the portal — API keys and credentials belong in your service's own secret
  store.
- **Config is not entitlement.** Never gate features on config contents;
  entitlement comes only from §3.

## 6. Testing recipe

Backend: stand up a **fake entitlement endpoint** with `httptest` and point
the gate at it (`Options.BaseURL`) — no Dapr, no identity-service, no DB.
This is exactly what
`services/booking-service/internal/appgate/gate_test.go` does; copy the
pattern:

```go
srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    // assert r.URL.Path == "/v1.0/invoke/identity/method/internal/entitlements/check"
    // assert r.Header.Get("X-Tenant-Slug") == "acme"
    json.NewEncoder(w).Encode(map[string]any{
        "app_id": "helpdesk", "allowed": false, "reason": "disabled",
    })
}))
defer srv.Close()
gate := appgate.New(appgate.Options{Enabled: true, BaseURL: srv.URL})
```

Cover at minimum (all present in `gate_test.go`):

1. **Decision matrix** — enabled → pass; `not_provisioned` → 402;
   `disabled`/`suspended` → 403.
2. **Unknown app** — fake answers 404 `{error}` → gate answers 403 with
   `reason: "unknown_app"`.
3. **Outage** — fake answers 500 (or is closed) → 503 with `Retry-After`;
   verify the failure is *not* cached (next request retries upstream).
4. **Cache hit** — second request for the same tenant+app makes zero HTTP
   calls; a different tenant is a separate entry; after the TTL the decision
   refreshes.
5. **Flag off** — with `Enabled: false` the handler runs and the fake
   records zero calls.

Frontend/BFF: mock the BFF entitlement response (or the
`/v1/tenants/{slug}/apps` payload) per status and assert nav entries hide,
the BFF returns 402/403, and the drawer shows a friendly error.

## 7. Checklist for W19/W20 app builders

- [ ] Catalog row added to `services/identity-service/internal/apps/catalog.yaml`
      with all manifest fields from §1 (honest `backend_note`).
- [ ] Backend service (or route group in an existing service) identified;
      `app_id` assigned per route group at wiring time.
- [ ] Layer 3: `appgate` middleware (or a port of it to your service's
      framework) wired behind the opt-in flag, default **off**.
- [ ] Layer 2: BFF route guard with 60s-cached entitlement check, 402/403
      mapping per §3.
- [ ] Layer 1: portal nav entry (`nav_route`) rendered only when status is
      `enabled`.
- [ ] Lifecycle subscription (if your app needs eager onboarding/teardown)
      on `opendesk.apps.lifecycle.v1`.
- [ ] Config schema documented; read-path validation + defaults; no secrets
      in `config`.
- [ ] Tests per §6: decision matrix, 404→403, outage→503 (fail closed),
      cache hit, flag-off pass-through.
- [ ] Rollout plan states when `APP_GATE_ENABLED` flips to `true` per
      environment.
