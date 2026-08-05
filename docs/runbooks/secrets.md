# Secrets management runbook

How Agora handles secrets in dev vs production, and how to rotate them.

---

## 1. Where secrets live

| Layer | Dev | Production |
|---|---|---|
| App/service env | `.env` (from `.env.example`, dev defaults) | SOPS-encrypted `.env` or Vault-injected env |
| Dapr component secrets | `secretstores.local.env` (reads sidecar env vars) | `secretstores.kubernetes` or Vault-backed Dapr secret store — components unchanged, only the store type swaps |
| APISIX static secrets | inline in `infra/apisix/apisix.yaml` (marked dev-only) | injected via `apisix.yaml` templating from Vault / sealed secrets |
| Compose `${VAR}` fallbacks | dev defaults inline | never rely on fallbacks; fail closed |

**Rules:** never commit a real secret; `.env` is gitignored (add it if you
create one); every secret in `.env.example` carries a dev default plus a
comment; anything marked `*-change-in-prod` or `dev-` MUST be rotated before
any non-local deployment.

## 2. Inventory (see `.env.example` for dev values)

- **Postgres**: `POSTGRES_PASSWORD` (superuser, infra only),
  `BOOKING_PG_PASS` / `CONVERSATION_PG_PASS` / `KNOWLEDGE_PG_PASS`
  (per-service LOGIN roles, `05-app-roles.sql`).
- **Keycloak**: `KEYCLOAK_ADMIN_CLIENT_SECRET` (`service-accounts` client).
- **LiveKit**: `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET`.
- **LLM**: `LLM_API_KEY` (empty for Ollama; required for hosted providers).
- **Admin web**: `AUTH_SECRET` (NextAuth).
- **Twenty CRM**: `TWENTY_API_KEY`, `TWENTY_WEBHOOK_SECRET`,
  `TWENTY_{ACCESS,LOGIN,REFRESH,FILE}_TOKEN_SECRET`, `TWENTY_APP_SECRET`.
- **MinIO**: `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` (also used by the GDPR
  export uploader as `S3_ACCESS_KEY` / `S3_SECRET_KEY`).
- **Dapr bindings**: `SMTP_PASSWORD`, `TWILIO_AUTH_TOKEN`, `REDIS_PASSWORD`.
- **APISIX inline**: jwt-auth HS256 consumer secret, openid-connect session
  secret (both in `infra/apisix/apisix.yaml`, marked dev-only).

## 3. Production patterns

### SOPS (git-ops friendly)

1. Keep `secrets/prod.env` encrypted with SOPS (age or PGP):

   ```sh
   sops --encrypt --age <recipient> prod.env > secrets/prod.env.sops
   ```

2. CI/CD decrypts at deploy time (`sops --decrypt secrets/prod.env.sops > .env`)
   on the target host; the plaintext never touches the repo.
3. Rotate recipients with `sops updatekeys` when the on-call group changes.

### Vault (dynamic injection)

1. Enable the KV v2 engine: `vault kv put secret/opendesk/prod KEY=...`.
2. Services read at deploy time via the Vault Agent Injector (k8s) or
   `vault kv get -format=json` rendered into env files by the deploy job.
3. Dapr: swap `secretstores.local.env` for `secretstores.hashicorp.vault`
   pointing at the same path — no component wiring changes.
4. Prefer short-lived credentials where supported (Postgres database
   secrets engine issues per-service `app_*_login` creds with TTL).

## 4. Rotation procedures

General order: **issue new → deploy consumers → revoke old**. All rotations
are zero-downtime when done in this order.

- **Postgres role password** (e.g. `app_booking_login`):
  1. `ALTER ROLE app_booking_login PASSWORD '<new>';`
  2. Update `BOOKING_PG_PASS` in the secret store; rolling-restart
     booking-service. Old sessions die with the pods.
- **`KEYCLOAK_ADMIN_CLIENT_SECRET`**: rotate in Keycloak admin → Clients →
  service-accounts → Credentials; update the secret; restart identity.
- **LiveKit key/secret**: add the new pair to LiveKit config (multiple keys
  supported), update voice env, restart, then remove the old pair.
- **`TWENTY_API_KEY`**: create a new API key in Twenty → Settings → APIs;
  update crm-sync env; revoke the old key. Webhook secret: update both the
  Twenty webhook config and `TWENTY_WEBHOOK_SECRET` together (brief webhook
  rejects are acceptable; events retry via DLQ).
- **`AUTH_SECRET`**: update and rolling-restart admin-web; existing user
  sessions are invalidated (users re-login) — schedule off-peak.
- **MinIO root creds**: `mc admin user add` a new admin, update
  `MINIO_ROOT_USER/PASSWORD` + `S3_ACCESS_KEY/SECRET`, restart dependents,
  disable the old user. Prefer per-purpose service accounts over root.
- **APISIX HS256 consumer secret / session secret**: edit
  `infra/apisix/apisix.yaml` (hot-reloads ~1s). HS256 rotation briefly
  rejects service JWTs — issue tokens with the new secret first, then flip.
- **`SMTP_PASSWORD` / `TWILIO_AUTH_TOKEN`**: update the provider, then the
  Dapr secret env; no restart needed for the next binding call beyond the
  sidecar env refresh (restart daprd sidecars in dev).

After any rotation: run `scripts/smoke-test.sh` and check
`docker compose ps` for crash-loops.
