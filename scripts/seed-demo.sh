#!/usr/bin/env bash
# seed-demo.sh — Seed demo tenant "acme" THROUGH THE APISIX GATEWAY.
#
# Current reality (W45 OOS-22 — this script used to describe a retired dev
# posture):
#   * Service ports (:7001/:7002/:7008 …) are NOT host-published (W34 GF4),
#     so all calls go through the gateway at http://localhost:9080.
#   * AUTHZ_DISABLED was removed (W34 GF12): every /api/* route requires a
#     Keycloak JWT (openid-connect bearer_only). There is no header bypass —
#     the gateway strips client-supplied x-tenant-*/x-user-* headers and the
#     services derive tenancy from the JWT `tenant_slugs` claim.
#   * The tenant plan is server-forced to "free" for anyone without the
#     platform-admin realm role / OPENDESK_PLATFORM_ADMINS allowlist
#     (SPEC-W44 W-I-1) — this script does not request a plan.
#
# Prereqs (docs/runbooks/local-dev.md §1/§3):
#   1. `make up` (gateway :9080, Keycloak :8080, realm `opendesk`).
#   2. A realm user, e.g. `dev-admin`, created via kcadm with realm role
#      `owner` (local-dev.md §3), and Direct Access Grants enabled once on
#      the `admin-web` client (dev console → Clients → admin-web →
#      Capability config) so the password grant below works.
#   3. For the group-join step: `docker compose` usable from this repo and
#      KC_BOOTSTRAP_ADMIN_PASSWORD exported. (The script can also skip this
#      step if your token already carries the `tenant_slugs` claim.)
#
# Env:
#   GATEWAY        http://localhost:9080   APISIX proxy
#   KEYCLOAK       http://localhost:8080   Keycloak base URL
#   REALM          opendesk
#   CLIENT_ID      admin-web               public client used for the grant
#   SEED_USER      realm username for the password grant (no default)
#   SEED_PASSWORD  realm password for the password grant (no default)
#   TOKEN          pre-fetched access token — skips the password grant
#   SLUG           acme                    tenant slug to seed
#
# Re-runnable: an existing tenant (409) is reused and offerings that already
# exist (matched by name) are skipped.

set -euo pipefail

GATEWAY="${GATEWAY:-http://localhost:9080}"
KEYCLOAK="${KEYCLOAK:-http://localhost:8080}"
REALM="${REALM:-opendesk}"
CLIENT_ID="${CLIENT_ID:-admin-web}"
SLUG="${SLUG:-acme}"
SEED_USER="${SEED_USER:-}"
SEED_PASSWORD="${SEED_PASSWORD:-}"
TOKEN="${TOKEN:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

command -v jq >/dev/null 2>&1 || { echo "seed-demo: jq is required" >&2; exit 1; }

die() { echo "seed-demo: ERROR: $*" >&2; exit 1; }

fetch_token() { # password grant (local-dev.md §3 Option A)
    [ -n "$SEED_USER" ] && [ -n "$SEED_PASSWORD" ] || die \
        "SEED_USER/SEED_PASSWORD are not set (or pass TOKEN=...). Create a realm user first — docs/runbooks/local-dev.md §3."
    curl -sf -X POST \
        "$KEYCLOAK/realms/$REALM/protocol/openid-connect/token" \
        -d grant_type=password -d client_id="$CLIENT_ID" \
        -d username="$SEED_USER" -d password="$SEED_PASSWORD" \
        | jq -r .access_token
}

token_has_tenant() { # token_has_tenant <jwt> — grep the claim payload
    local payload
    payload="$(printf '%s' "$1" | cut -d. -f2 | tr '_-' '/+')"
    # pad to a multiple of 4 for base64 -d
    case $(( ${#payload} % 4 )) in 2) payload="${payload}==";; 3) payload="${payload}=";; esac
    printf '%s' "$payload" | base64 -d 2>/dev/null | grep -q "\"$SLUG\""
}

join_tenant_group() { # best-effort: add SEED_USER to /tenants/$SLUG via kcadm
    [ -n "$SEED_USER" ] || { echo "seed-demo: SEED_USER unset — cannot auto-join the tenant group"; return 1; }
    [ -n "${KC_BOOTSTRAP_ADMIN_PASSWORD:-}" ] || { echo "seed-demo: KC_BOOTSTRAP_ADMIN_PASSWORD unset — cannot run kcadm"; return 1; }
    command -v docker >/dev/null 2>&1 || { echo "seed-demo: docker not available for kcadm"; return 1; }

    local KC="docker compose exec -T keycloak /opt/keycloak/bin/kcadm.sh"
    (cd "$ROOT" && $KC config credentials --server http://localhost:8080 \
        --realm master --user admin --password "$KC_BOOTSTRAP_ADMIN_PASSWORD") >/dev/null

    local uid gid tries=0
    uid="$(cd "$ROOT" && $KC get users -r "$REALM" -q username="$SEED_USER" \
        --fields id,username --format csv --noquotes 2>/dev/null \
        | awk -F, -v u="$SEED_USER" '$2==u {print $1; exit}')"
    [ -n "$uid" ] || { echo "seed-demo: realm user '$SEED_USER' not found"; return 1; }

    # The /tenants/$SLUG group is created by identity-service at tenant
    # creation (retried by the onboarding workflow) — poll briefly.
    while [ $tries -lt 15 ]; do
        gid="$(cd "$ROOT" && $KC get groups -r "$REALM" -q search="$SLUG" \
            --fields id,path --format csv --noquotes 2>/dev/null \
            | awk -F, -v p="/tenants/$SLUG" '$2==p {print $1; exit}')"
        [ -n "$gid" ] && break
        tries=$((tries + 1)); sleep 2
    done
    [ -n "$gid" ] || { echo "seed-demo: group /tenants/$SLUG not found after 30s"; return 1; }

    (cd "$ROOT" && $KC update "users/$uid/groups/$gid" -r "$REALM" \
        -s realm="$REALM" -s userId="$uid" -s groupId="$gid" -n) >/dev/null
    echo "seed-demo: added $SEED_USER to /tenants/$SLUG"
}

echo "== 1. Token =="
if [ -z "$TOKEN" ]; then
    TOKEN="$(fetch_token)" || die "token grant failed (is Direct Access Grants enabled on $CLIENT_ID? is $KEYCLOAK up?)"
fi
[ -n "$TOKEN" ] && [ "$TOKEN" != "null" ] || die "empty access token"
AUTH="Authorization: Bearer $TOKEN"
echo "token OK"

echo "== 2. Tenant $SLUG (via gateway; plan is server-forced to free) =="
body="$(jq -nc --arg slug "$SLUG" '{
  slug:$slug, name:"Acme Studio", timezone:"Europe/London",
  currency:"GBP", locale:"en-GB",
  terminology:{offering:"service", team_member:"stylist", booking:"appointment", contact:"client"}
}')"
code="$(curl -s -o /tmp/seed-demo-tenant.json -w '%{http_code}' \
    -X POST "$GATEWAY/api/identity/v1/tenants" \
    -H "$AUTH" -H 'content-type: application/json' -d "$body")"
case "$code" in
    2*) jq . /tmp/seed-demo-tenant.json ;;
    409) echo "tenant $SLUG already exists — reusing" ;;
    401) die "401 from identity — token missing/expired" ;;
    *) cat /tmp/seed-demo-tenant.json >&2; die "tenant create failed (HTTP $code)" ;;
esac

echo "== 3. Tenant group membership (tenant_slugs claim) =="
if token_has_tenant "$TOKEN"; then
    echo "token already carries tenant $SLUG"
else
    echo "joining /tenants/$SLUG so the JWT carries tenant_slugs=[$SLUG] ..."
    if join_tenant_group; then
        if [ -n "$SEED_PASSWORD" ]; then
            TOKEN="$(fetch_token)" || die "token refresh after group join failed"
            AUTH="Authorization: Bearer $TOKEN"
        fi
        token_has_tenant "$TOKEN" || die \
            "group joined but the token still lacks tenant_slugs=[$SLUG] — re-login / re-fetch TOKEN and re-run"
    else
        die "could not auto-join /tenants/$SLUG. Manually: Keycloak admin console → Groups → /tenants/$SLUG → add $SEED_USER, then re-fetch the token and re-run (docs/runbooks/local-dev.md §3)."
    fi
fi

echo "== 4. Offerings =="
existing="$(curl -sf "$GATEWAY/api/bookings/v1/offerings" -H "$AUTH" | jq -r '.[].name // empty' 2>/dev/null || true)"
create_offering() { # create_offering <name> <duration> <buffer> <price_cents>
    if printf '%s\n' "$existing" | grep -qx "$1"; then
        echo "offering '$1' exists — skipping"; return 0
    fi
    curl -sf -X POST "$GATEWAY/api/bookings/v1/offerings" -H "$AUTH" \
        -H 'content-type: application/json' \
        -d "{\"name\":\"$1\",\"duration_min\":$2,\"buffer_min\":$3,\"price_cents\":$4,\"currency\":\"GBP\",\"capacity\":1,\"bookable\":true}" \
        | jq .
}
create_offering "Haircut" 30 10 3500
create_offering "Consultation" 45 15 6000

echo "== 5. Knowledge document =="
curl -sf -X POST "$GATEWAY/api/knowledge/v1/documents" -H "$AUTH" \
    -H 'content-type: application/json' \
    -d "{\"tenant\":\"$SLUG\",\"title\":\"Opening hours & policies\",\"body\":\"Open Mon-Sat 9:00-18:00. Cancellations free up to 24h before the appointment, otherwise a no-show fee applies.\"}" \
    | jq .

echo "== 6. Verify (public context via gateway) =="
curl -sf "$GATEWAY/api/bookings/public/sites/$SLUG/context" \
    | jq -e '.site.name // .tenant.slug' >/dev/null && echo "public context OK"

echo "Seed complete: tenant $SLUG (plan: free) with 2 offerings + 1 knowledge doc."
echo "Dashboard: $GATEWAY/app/$SLUG — booking page: $GATEWAY/p/$SLUG"
