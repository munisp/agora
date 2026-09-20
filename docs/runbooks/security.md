# Security runbook

Covers the SPEC-W3 §2 controls: WAF (open-appsec) posture changes, Postgres
RLS enforcement, gateway rate limiting/authz, and the ZAP baseline scan.

---

## 1. open-appsec: detect → prevent promotion

The open-appsec nano agent (profile `appsec`, see
`infra/docker-compose.edge.yml`) watches `infra/openappsec/local_policy.yaml`
and hot-reloads it — no restarts needed for any step below.

### 1.1 Learning period (detect-learn)

- Default posture is `detect-learn` everywhere: events are logged, nothing
  is blocked. Keep this for **at least one week of representative traffic**
  (or a full run of `tests/e2e/` + `scripts/smoke-test.sh`) per environment.
- The contextual ML engine builds one model per `specific-rules` host entry
  (`localhost/api`, `localhost/p`, `localhost`).
- Inspect detections (logs stay local; `cloud: false`):

  ```sh
  docker logs opendesk-openappsec 2>&1 | grep -i "detect" | tail -50
  ```

- Triage false positives BEFORE enforcing: narrow a `specific-rules` host,
  or tune `minimum-confidence` in `opendesk-web-attack-practice`.

### 1.2 Enforce switch (prevent-learn)

1. In `infra/openappsec/local_policy.yaml` set
   `policies.default.mode: prevent-learn` and the same on each specific rule
   you want enforced (practices inherit via `override-mode: as-top-level`).
2. Raise `opendesk-web-attack-practice.web-attacks.minimum-confidence` to
   `high` so borderline traffic is logged, not blocked.
3. Save — the agent hot-reloads. Watch `prevent-events` in the logs for an
   hour and be ready to flip back (step 1.1 values) as an instant rollback.

### 1.3 API discovery + OpenAPI schema upload

While learning, open-appsec auto-discovers the API surface of `/api/*`
(paths, methods, parameter names/types). To validate requests against the
**authoritative** spec instead of the learned one:

1. Mount the specs (kept in `docs/api/openapi/`: `booking.yaml`,
   `identity.yaml`, `payments.yaml`) into the agent container in
   `infra/docker-compose.edge.yml`:

   ```yaml
   volumes:
     - ../docs/api/openapi:/ext/openapi:ro
   ```

2. Point the discovery practice at them and enforce:

   ```yaml
   # in opendesk-api-discovery-practice
   openapi-schema-validation:
     override-mode: prevent-learn
     files:
       - /ext/openapi/booking.yaml
       - /ext/openapi/identity.yaml
       - /ext/openapi/payments.yaml
   ```

3. Requests whose shape contradicts the uploaded schema are now blocked;
   schema drift on covered endpoints shows up as prevent events.

---

## 2. Postgres row-level security (RLS)

- Every tenant table in the booking/conversation/knowledge schemas has
  `ENABLE` + `FORCE ROW LEVEL SECURITY` with policy
  `tenant_id = current_setting('app.tenant_id', true)::uuid`
  (init scripts 01/03/04).
- Application stores set the tenant per transaction:
  - **booking-service (Go)**: `internal/store.withTenant` opens a tx and runs
    `SELECT set_config('app.tenant_id', $1, true)` before any statement. All
    tenant-scoped queries go through it. Documented exceptions:
    schema bootstrap DDL, the cross-tenant outbox dispatcher
    (`FetchUnsentOutbox`/`MarkOutboxSent`), and public site-slug resolution
    (`GetSiteBySlug` — tenant unknown until the slug resolves; all queries
    after resolution are tenant-scoped).
  - **conversation-service (Python)**: `Database._tenant_tx` (same
    `set_config(..., true)` pattern) — verified, already compliant.
- **Per-service DB roles** (`infra/postgres/init-scripts/05-app-roles.sql`):
  NOLOGIN group roles `app_booking` / `app_conversation` / `app_knowledge`
  hold per-database grants; LOGIN variants `app_*_login` inherit them. The
  superuser `opendesk` bypasses RLS — services must connect with their
  LOGIN role for FORCE RLS to actually bind. Wire-up:
  `BOOKING_PG_USER`/`BOOKING_PG_PASS` in `.env` (see `.env.example`), consumed
  by the booking `DATABASE_URL` construction in `docker-compose.yml` (and by
  booking-service's config fallback `PG_DSN`+`PG_USER`/`PG_PASS`).
- Verify enforcement manually:

  ```sh
  psql "postgres://app_booking_login:app_booking_dev_password@localhost:5432/booking" \
    -c "SELECT count(*) FROM bookings"   -- 0 rows: no app.tenant_id set
  ```

---

## 3. Gateway authz & rate limiting (APISIX)

- `/api/*` routes: `openid-connect` bearer_only against Keycloak + redis
  `limit-count` 600/min per IP.
- `/voice/*` (public anonymous chat/session): no OIDC possible — instead a
  stricter 30/min per-IP `limit-count`. A Turnstile-style bot gate in front
  of `/voice/chat` is the documented prod follow-up (see the comment on the
  `voice-runtime` route in `infra/apisix/apisix.yaml`).
- `/ws` stays on **in-app JWT** (gateway-edge validates against Keycloak
  JWKS): browsers can't set headers on WebSocket upgrades, and the edge must
  bind tenant claims to channels before subscribing. Rationale is documented
  on the `ws-gateway` route.
- Plan-tier quotas: example route `api-plan-tier-example` (disabled) shows
  `limit-count` keyed on `http_x_tenant_plan` with the documented plan map
  `free=60/min`, `pro=600/min` (clone per plan with a `vars` match).

---

## 4. OWASP ZAP baseline scan

`scripts/security-scan.sh` runs the ZAP baseline against the gateway
(`http://host.docker.internal:9080`) in Docker and writes an HTML report to
`reports/`. See the script header for usage and CI wiring. Baseline findings
are informational — triage into issues; do not gate CI on it until the false
positive list is maintained.

---

## 5. GDPR data-subject requests (innovation 13)

- `POST /v1/privacy/export` and `POST /v1/privacy/erase` on booking-service
  (Permify `manage_bookings`) start `GdprExportWorkflow` /
  `GdprEraseWorkflow` (notification-worker).
- Export collects bookings (`?contact=`), conversations (`?contact=`), the
  tenant ledger balance, and the Twenty person (`/v1/people/lookup`) into a
  JSON bundle uploaded to the MinIO `exports` bucket (plain S3 PUT); the
  workflow result is the object path (presigned URLs are a prod add-on).
- Erase publishes a `PrivacyEraseRequested` tombstone CloudEvent to
  `opendesk.privacy.events`; booking (anonymizes contacts), conversation
  (deletes turns) and crm-sync (deletes the Twenty person + sync_map rows)
  consume it. Tombstones are idempotent; replays are safe.

---

## 6. First platform admin bootstrap (STK O15)

"Platform admin" is a **cross-tenant operator** — the only principal that may
set a non-`free` plan (`POST /v1/tenants` plan field, `PATCH
/v1/tenants/{slug}/plan`) and perform other platform-level actions
(assigning the `analyst`/`billing` realm roles, lending KYC overrides).
identity-service recognizes a caller as platform admin when EITHER
(`internal/httpapi/auth.go` `isPlatformAdmin`, SPEC-W43 I-01):

1. the JWT carries the **`platform-admin` realm role** (realm_access.roles), or
2. the JWT `sub` is in the **`OPENDESK_PLATFORM_ADMINS`** allowlist (CSV env
   on identity-service).

The shipped realm (`infra/keycloak/realm-opendesk.json`) deliberately has
**no** `platform-admin` role and no users — bootstrap is a conscious operator
act, not a default.

### 6.1 Create the role + first platform admin (kcadm)

```sh
# 0. Authenticate kcadm (master-realm bootstrap admin — see
#    infra/keycloak/README.md; password was exported before `make up`).
docker compose exec keycloak /opt/keycloak/bin/kcadm.sh config credentials \
  --server http://localhost:8080 --realm master \
  --user admin --password "$KC_BOOTSTRAP_ADMIN_PASSWORD"

# 1. Create the realm role ONCE (add-roles fails for a role that doesn't exist).
docker compose exec keycloak /opt/keycloak/bin/kcadm.sh create roles -r opendesk \
  -s name=platform-admin \
  -s description='Platform operator — cross-tenant plan/admin actions'

# 2. Create the user and set a real password.
docker compose exec keycloak /opt/keycloak/bin/kcadm.sh create users -r opendesk \
  -s username=platform-admin -s enabled=true
docker compose exec keycloak /opt/keycloak/bin/kcadm.sh set-password -r opendesk \
  --username platform-admin --new-password '<real-password>'

# 3. Grant the role.
docker compose exec keycloak /opt/keycloak/bin/kcadm.sh add-roles -r opendesk \
  --uusername platform-admin --rolename platform-admin
```

Do **not** add this user to any `/tenants/*` group: platform admin is a
platform capability, not a tenant membership.

### 6.2 Alternative/complement: the subject allowlist

`OPENDESK_PLATFORM_ADMINS` is a CSV of Keycloak user **ids** (the JWT `sub`),
read by identity-service at boot (`internal/config/config.go`). Useful for
break-glass or when the realm is managed externally:

```sh
SUB=$(docker compose exec keycloak /opt/keycloak/bin/kcadm.sh get users \
  -r opendesk -q username=platform-admin --fields id --format csv --noquotes)
# .env:  OPENDESK_PLATFORM_ADMINS=<uuid>[,<uuid>...]
docker compose up -d identity   # pick up the env change
```

Prefer the realm role for humans (revocable in one place, visible in the
admin console); use the allowlist sparingly and audit it — both paths are
logged by identity-service when a plan override is applied.

### 6.3 Dev invite emails: capture with Mailpit

Two invite-mail paths exist (SPEC-W45 K8/K10): notification-worker sends the
MemberInvited email through the Dapr SMTP output binding
(`infra/dapr/components/bindings.smtp.yaml`, env `SMTP_HOST`/`SMTP_PORT`/
`SMTP_USER`/`SMTP_FROM`/`SMTP_PASSWORD`), and identity-service's bootstrap
PATCHes the Keycloak realm `smtpServer` from `KC_REALM_SMTP_HOST`/`_PORT`/
`_FROM`/`_USER`/`_PASSWORD` so Keycloak's own execute-actions email
(UPDATE_PASSWORD, VERIFY_EMAIL) fires. Neither sends anything real in dev —
there is no SMTP server in the default compose. To see the emails locally,
run Mailpit and point both var sets at it:

```sh
docker run -d --name mailpit --network opendesk_opendesk \
  -p 1025:1025 -p 8025:8025 axllent/mailpit
# .env:  SMTP_HOST=mailpit  SMTP_PORT=1025  SMTP_FROM="OpenDesk <no-reply@opendesk.local>"
#        KC_REALM_SMTP_HOST=mailpit  KC_REALM_SMTP_PORT=1025  KC_REALM_SMTP_FROM=no-reply@opendesk.local
docker compose up -d notification identity
# inbox UI: http://localhost:8025
```

(If the compose project network differs, `docker network ls` and adjust; both
SMTP configs fail soft — invites are still created, only the email is dropped,
and the failure is logged.)
