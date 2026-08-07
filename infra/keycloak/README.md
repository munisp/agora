# Keycloak realm `opendesk` — dev import & secrets

`realm-opendesk.json` is imported at container start (`start-dev --import-realm`,
see `infra/docker-compose.core.yml`). Wave W34 (GF5) hardened the realm:

- `sslRequired: external`, `bruteForceProtected: true`, refresh-token
  revocation on (`revokeRefreshToken: true`, `refreshTokenMaxReuse: 0`).
- The seeded `admin` / `admin123` demo user is **removed**. There is no realm
  user out of the box; create one after first boot (below) or manage users via
  your IdP in real environments.
- The `service-accounts` client secret is the placeholder
  `CHANGE_ME_DEV_ONLY`, **not** a working credential.

## Bootstrap admin (required)

The compose service no longer carries a literal bootstrap password. Export it
before `docker compose up` (compose fails fast otherwise):

```sh
export KC_BOOTSTRAP_ADMIN_PASSWORD='pick-a-real-dev-password'
```

This is the Keycloak **master-realm** admin (console at
http://localhost:8080/admin), not a realm user.

## Setting the `service-accounts` client secret

Services authenticate with `KEYCLOAK_ADMIN_CLIENT_ID=service-accounts` /
`KEYCLOAK_ADMIN_CLIENT_SECRET` (compose falls back to the
`CHANGE_ME_DEV_ONLY` placeholder for local parity). To set a real secret:

**Option A — patch before import (fresh container):** edit
`realm-opendesk.json` locally, or generate an override realm and keep the real
file out of git.

**Option B — kcadm against a running realm:**

```sh
docker compose exec keycloak /opt/keycloak/bin/kcadm.sh config credentials \
  --server http://localhost:8080 --realm master \
  --user admin --password "$KC_BOOTSTRAP_ADMIN_PASSWORD"
CID=$(docker compose exec keycloak /opt/keycloak/bin/kcadm.sh get clients \
  -r opendesk -q clientId=service-accounts --fields id --format csv --noquotes)
docker compose exec keycloak /opt/keycloak/bin/kcadm.sh update "clients/$CID" \
  -r opendesk -s secret="$KEYCLOAK_ADMIN_CLIENT_SECRET"
```

Then set the same value in `.env` (`KEYCLOAK_ADMIN_CLIENT_SECRET=...`) so the
services agree with the realm. Never commit a real secret.

## Creating a dev realm user (replacing the removed admin/admin123)

```sh
docker compose exec keycloak /opt/keycloak/bin/kcadm.sh create users -r opendesk \
  -s username=dev-admin -s enabled=true
docker compose exec keycloak /opt/keycloak/bin/kcadm.sh set-password -r opendesk \
  --username dev-admin --new-password '<dev-only-password>'
docker compose exec keycloak /opt/keycloak/bin/kcadm.sh add-roles -r opendesk \
  --uusername dev-admin --rolename owner
```

Add the user to a tenant group (`/tenants/acme`) via the admin console to get
the `tenant_slugs` claim populated.
