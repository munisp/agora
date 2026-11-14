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
- **Runtime-created tables (W51-C, V-FUNDS grants gap)**: several services
  apply schema DDL at boot on their own runtime DSN (booking-service
  `ensure*` migrations, payments-service boot bootstrap, notification-worker,
  crm-sync, kyc, billing, conversation/knowledge helpers), so those tables
  are owned by the `app_*_login` role — not by the bootstrap superuser.
  `05-app-roles.sql` therefore sets `ALTER DEFAULT PRIVILEGES FOR ROLE
  <app_*_login> IN SCHEMA public` (in addition to `opendesk`/`postgres`) in
  every service database, granting SELECT/INSERT/UPDATE/DELETE on future
  tables and USAGE/SELECT on future sequences to the `app_*` group — and to
  the `app_*_internal` escape group where one exists (identity,
  notifications, payments). Without this, runtime tables such as
  `booking_reviews` / `booking_payment_attempts` were invisible to the other
  app roles. Tables already existing when the script (re)runs are covered by
  the same section's `GRANT ... ON ALL TABLES` statements; `USAGE ON SCHEMA
  public` is granted per database for every app role.
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

---

## 7. Cross-tenant document-fraud registry (SPEC-W67)

Operational companion to `docs/ocr.md` ("Cross-tenant document-fraud
registry") and `docs/compliance-ndpa.md` (NDPA gate-map row + erasure
consistency + the PRE-DEPLOY lawful-basis gate). The registry is the
hash-only, HMAC-keyed, platform-scoped pair of tables
(`ocr_doc_registry`, `ocr_doc_registry_tenants`) in the ocr DB that detects
the same exact document bytes at >=2 distinct tenants — sha256-EXACT only,
phash EXCLUDED from every cross-tenant artifact.

### 7.1 Threat model (what the implementation must answer — RECON-W67 §4)

1. **Oracle attacks.** The registry is unreachable except via document
   upload; exact-sha256-only means a probe costs possessing the exact
   document and yields 1 bit ("exists elsewhere"); phash is excluded
   (invertible 8x8 thumbnail + hill-climbing oracle). The alert `tenant_count`
   is the only cross-tenant datum. Upload-API response invariance (status,
   body, latency class identical whether or not a match fires) removes the
   timing channel. Gates: zero-disclosure suite + response-invariance test
   (`services/ocr-service/tests/test_w67_zero_disclosure.py`).
2. **Tenant enumeration.** Match events/alerts carry NO other tenant id —
   only the later tenant's own ids + one integer. The registry has NO
   tenant-facing endpoint and NO read endpoint at all in v1 (alerts-only
   design; the W55 platform-admin-page sketch is deferred). Platform-side
   observability via metrics (`registry_keys_total`,
   `registry_matches_total`) and platform logs only.
3. **Data minimization.** Registry stores HMAC(sha256) keys, tenant-uuid
   memberships, timestamps, counts — nothing else. Raw sha256 is never
   written to the registry; no document_id, storage_key, phash, kind,
   fields, or text in registry tables (schema-allowlist test). Match event
   payload = own document_id + own sha256 + kind + one integer
   (payload-allowlist test, both sides).
4. **Access control.** Both tables FORCE RLS; the ONLY pass is the
   `app.ocr_registry` GUC escape, set only inside the registry module's
   dedicated connection path (grep gate: the GUC string appears only in the
   registry module, the two DDL copies, and tests/docs). The backfill
   endpoint is internauth fail-closed (X-Internal-Token, 503 unset / 401
   wrong). No APISIX route exposes registry paths.
5. **Audit logging.** Every match is logged platform-side (registry_key,
   both tenant ids, ts) — platform operator only, NEVER tenant-visible.
   Every registry write/delete is audited (NDPA accountability, R8 idiom).
6. **Retention / erasure.** Registry rows live while >=1 membership exists;
   TenantDelete removes only own memberships (parent row dropped at zero);
   subject erasure leaves the registry untouched BY DESIGN (no subject
   linkage — tombstone audit-hash precedent); per-document TTL expiry does
   not decrement memberships (accepted residual: stale count => possible
   alert needing human triage, never a disclosure).
7. **HMAC posture.** `OCR_REGISTRY_HMAC_SECRET` fail-closed
   (KYC_HASH_SECRET idiom): unset => registry DISABLED (one startup log
   line; upserts skipped). Rotation = re-key backfill (§7.3).
8. **Legal sign-off (PRE-DEPLOY gate, not a build gate).** NDPA lawful
   basis (legitimate interest / security of processing) confirmed by
   counsel + DPIA note for the cross-tenant processing registry BEFORE the
   secret is provisioned in production. See `docs/compliance-ndpa.md`.

### 7.2 Backfill runbook (`POST /v1/internal/ocr/backfill-document-registry`)

Use for first deployment and for re-key after HMAC rotation. Endpoint
contract (auth, request/response shapes): `docs/ocr.md` §registry. RLS
split: tenant enumeration via `sweep_conn()`, row reads under
`tenant_conn()`, registry writes only via the registry module's GUC
connection.

```bash
# 0. Sizing pass — writes NOTHING, emits NOTHING.
curl -X POST :7020/v1/internal/ocr/backfill-document-registry   -H "X-Internal-Token: $OCR_INTERNAL_TOKEN" -d '{"dry_run": true}'

# 1. Page to completion (next_cursor null), keeping the cursor chain.
curl -X POST :7020/v1/internal/ocr/backfill-document-registry   -H "X-Internal-Token: $OCR_INTERNAL_TOKEN" -d '{"limit": 5000}'
curl -X POST :7020/v1/internal/ocr/backfill-document-registry   -H "X-Internal-Token: $OCR_INTERNAL_TOKEN" -d '{"cursor": "<next_cursor>"}'

# 2. graph-sync projects seen_elsewhere_count onto the later tenants' own
#    (:Document) nodes; run D9 to raise the S4 alerts.
curl -X POST :7017/v1/detect/run -H "X-Internal-Token: $FRAUD_INTERNAL_TOKEN"   -d '{"detector": "d9_document_abuse", "lookback_days": 365}'
```

- **Expect a one-time alert burst** on the first real run: every
  pre-existing cross-tenant duplicate produces exactly one
  `cross_tenant_exact_duplicate` S4 alert for each LATER tenant
  (first-use-wins; earlier tenants are never alerted). Size the window and
  the expected burst from the dry_run counters before emitting, and
  pre-brief triage.
- **Re-runs are safe and free:** the backfill calls the SAME upsert
  function as the live path (membership `ON CONFLICT DO NOTHING` + `alerted`
  flag), and D9 alert MERGE dedup (`alert_id
  document_abuse:{T}:none:xtenant:{sha256}`) absorbs replays — a second run
  adds zero memberships and emits zero new match events.
- Invariant per page: `scanned == upserted + skipped` and
  `memberships_added >= matches`. Keyset `(created_at, id)` order IS
  first-use-wins chronology.

### 7.3 HMAC key rotation (re-key via registry backfill)

Rotating `OCR_REGISTRY_HMAC_SECRET` changes every `registry_key`, so the
registry must be rebuilt from the durable `ocr_documents` rows (which hold
the raw sha256 under tenant RLS — the registry itself cannot be re-keyed in
place; that is the point of HMAC keying).

1. **Confirm the PRE-DEPLOY gate stays satisfied** (counsel lawful-basis
   confirmation covers the registry as such; a pure re-key does not reopen
   it, but record the rotation in the ops log).
2. Generate the new secret; stage it as `OCR_REGISTRY_HMAC_SECRET` for the
   next ocr-service rollout. Do NOT reuse any other service's secret.
3. Roll ocr-service with the new secret. From this moment the LIVE path
   writes new-key fingerprints while old-key rows remain — mixed-key state
   would double-count, so proceed to step 4 in the same maintenance window.
4. **Wipe the derived registry state** (it is a pure projection of
   `ocr_documents`): as a platform operator on the ocr DB, with the GUC
   escape set, delete all memberships then all parent rows:
   ```sql
   SET app.ocr_registry = 'on';
   DELETE FROM ocr_doc_registry_tenants;
   DELETE FROM ocr_doc_registry;
   ```
5. **Re-key backfill:** run §7.2 to completion (dry_run first, then page to
   `next_cursor = null`). This rebuilds every fingerprint under the new key
   in true (created_at, id) chronology.
6. **Expected re-alerting:** because memberships were rebuilt, the later
   tenant of each still-present cross-tenant duplicate is re-emitted ONCE;
   D9's MERGE dedup lands on the SAME `alert_id`, refreshing open alerts in
   place rather than duplicating them. Closed alerts stay closed.
7. Verify: `registry_keys_total` / `registry_matches_total` metrics match
   the dry_run sizing; spot-check that
   `SELECT count(*) FROM ocr_doc_registry` equals the dry_run `upserted`
   total; confirm no tenant-visible artifact carries anything but the
   tenant's own ids + `tenant_count`.
8. Destroy the old secret material everywhere (it can no longer verify
   anything once step 4 lands).

### 7.4 Verification battery

`services/ocr-service/tests/test_w67_zero_disclosure.py` (zero-disclosure,
timing/oracle invariance, idempotency incl. same-hash race, erasure
consistency, backfill auth + idempotent re-run, DDL schema allowlist, GUC
grep gate, analytics-pipeline unknown-type tolerance) and
`services/fraud-engine/tests/test_w67_s4_disclosure.py` (S4
zero-disclosure evidence schema, dedup/alert-id pins, graph-sync payload
allowlist grep, no-Postgres-DSN grep). Manual e2e script:
`tests/e2e/w67_cross_tenant_registry.md`.
