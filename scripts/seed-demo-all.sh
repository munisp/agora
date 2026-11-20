#!/usr/bin/env bash
# seed-demo-all.sh — I17 seed parity: ONE command seeds the four demo
# industries THROUGH THE APISIX GATEWAY, each fully demo-ready:
#
#   demo-salon        (salon pack)        stylist / appointment / client
#   demo-clinic       (clinic pack)       practitioner / visit / patient
#   demo-consultancy  (consultancy pack)  consultant / session / prospect
#   demo-logistics    (logistics pack)    rider / slot / customer
#
# Per tenant: offerings parsed from the industry pack YAML, a team member
# with Mon–Sat 09:00–17:00 availability, a PUBLISHED site, a knowledge
# document, and BOOKINGS_PER_TENANT simulated bookings created through the
# PUBLIC booking path (booking outbox -> Kafka -> lakehouse, so the
# lakehouse/dbt path has data on day one).
#
# Auth posture (W34 GF4 + SPEC-W44 K1, same as seed-demo.sh): every call
# goes through the gateway with a Keycloak JWT; the gateway STRIPS
# client-supplied X-Tenant-Slug and the booking tenant middleware resolves
# the FIRST tenant_slugs claim when the header is absent. A multi-tenant
# JWT would therefore seed every tenant into the first claim — so this
# script seeds ONE TENANT PER TOKEN ROUND: join the tenant group, refetch
# the token, assert tenant_slugs[0] == the slug, seed, then leave the
# group (all demo groups are best-effort re-joined at the end so the
# dashboard can browse every demo tenant). TOKEN passthrough mode cannot
# refetch, so it ASSERTS tenant_slugs[0] == slug per tenant and dies on
# mismatch — never a silent cross-tenant write.
#
# Re-runnable: existing tenants (409), offerings, team members and
# knowledge docs (matched by name/title) are skipped, and bookings carry
# deterministic idempotency keys (demo-{slug}-{n}) so re-runs never
# duplicate.
#
# Prereqs: as seed-demo.sh (make up; realm user with Direct Access Grants,
# or pass TOKEN=...). jq + python3 (stdlib only) required.
#
# Env:
#   GATEWAY / KEYCLOAK / REALM / CLIENT_ID   as seed-demo.sh
#   SEED_USER / SEED_PASSWORD                password grant (or TOKEN=...)
#   TOKEN             pre-fetched access token — skips the password grant
#   DEMO_TENANTS      "demo-salon demo-clinic demo-consultancy demo-logistics"
#   BOOKINGS_PER_TENANT  3
#   PACK_DIR          <repo>/industries

set -euo pipefail

GATEWAY="${GATEWAY:-http://localhost:9080}"
KEYCLOAK="${KEYCLOAK:-http://localhost:8080}"
REALM="${REALM:-opendesk}"
CLIENT_ID="${CLIENT_ID:-admin-web}"
SEED_USER="${SEED_USER:-}"
SEED_PASSWORD="${SEED_PASSWORD:-}"
TOKEN="${TOKEN:-}"
TOKEN_SUPPLIED=0
[ -n "$TOKEN" ] && TOKEN_SUPPLIED=1
DEMO_TENANTS="${DEMO_TENANTS:-demo-salon demo-clinic demo-consultancy demo-logistics}"
BOOKINGS_PER_TENANT="${BOOKINGS_PER_TENANT:-3}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
PACK_DIR="${PACK_DIR:-$ROOT/industries}"

command -v jq >/dev/null 2>&1 || { echo "seed-demo-all: jq is required" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "seed-demo-all: python3 is required" >&2; exit 1; }

die() { echo "seed-demo-all: ERROR: $*" >&2; exit 1; }

fetch_token() { # password grant (local-dev.md §3 Option A)
    [ -n "$SEED_USER" ] && [ -n "$SEED_PASSWORD" ] || die \
        "SEED_USER/SEED_PASSWORD are not set (or pass TOKEN=...). Create a realm user first — docs/runbooks/local-dev.md §3."
    curl -sf -X POST \
        "$KEYCLOAK/realms/$REALM/protocol/openid-connect/token" \
        -d grant_type=password -d client_id="$CLIENT_ID" \
        -d username="$SEED_USER" -d password="$SEED_PASSWORD" \
        | jq -r .access_token
}

jwt_payload() { # jwt_payload <jwt> — decoded payload JSON (unverified; the
    # gateway verifies the signature — this is local claim inspection only)
    local payload
    payload="$(printf '%s' "$1" | cut -d. -f2 | tr '_-' '/+')"
    case $(( ${#payload} % 4 )) in 2) payload="${payload}==";; 3) payload="${payload}=";; esac
    printf '%s' "$payload" | base64 -d 2>/dev/null
}

token_first_tenant() { # token_first_tenant <jwt> — tenant_slugs[0] or ""
    jwt_payload "$1" | jq -r '.tenant_slugs[0] // ""' 2>/dev/null
}

kcadm_cmd() {
    [ -n "$SEED_USER" ] || { echo "seed-demo-all: SEED_USER unset — cannot run kcadm" >&2; return 1; }
    [ -n "${KC_BOOTSTRAP_ADMIN_PASSWORD:-}" ] || { echo "seed-demo-all: KC_BOOTSTRAP_ADMIN_PASSWORD unset — cannot run kcadm" >&2; return 1; }
    command -v docker >/dev/null 2>&1 || { echo "seed-demo-all: docker not available for kcadm" >&2; return 1; }
    (cd "$ROOT" && docker compose exec -T keycloak /opt/keycloak/bin/kcadm.sh "$@")
}

kc_group_id() { # kc_group_id <slug> — group id of /tenants/<slug> (poll 30s)
    local slug="$1" gid tries=0
    while [ $tries -lt 15 ]; do
        gid="$(kcadm_cmd get groups -r "$REALM" -q search="$slug" \
            --fields id,path --format csv --noquotes 2>/dev/null \
            | awk -F, -v p="/tenants/$slug" '$2==p {print $1; exit}')"
        [ -n "$gid" ] && { printf '%s' "$gid"; return 0; }
        tries=$((tries + 1)); sleep 2
    done
    return 1
}

kc_user_id() {
    kcadm_cmd get users -r "$REALM" -q username="$SEED_USER" \
        --fields id,username --format csv --noquotes 2>/dev/null \
        | awk -F, -v u="$SEED_USER" '$2==u {print $1; exit}'
}

join_tenant_group() { # join_tenant_group <slug>
    local slug="$1" uid gid
    kcadm_cmd config credentials --server http://localhost:8080 \
        --realm master --user admin --password "$KC_BOOTSTRAP_ADMIN_PASSWORD" >/dev/null \
        || return 1
    uid="$(kc_user_id)"; [ -n "$uid" ] || return 1
    gid="$(kc_group_id "$slug")" || return 1
    kcadm_cmd update "users/$uid/groups/$gid" -r "$REALM" \
        -s realm="$REALM" -s userId="$uid" -s groupId="$gid" -n >/dev/null || return 1
    echo "seed-demo-all: added $SEED_USER to /tenants/$slug"
}

leave_tenant_group() { # leave_tenant_group <slug> — best-effort
    local slug="$1" uid gid
    uid="$(kc_user_id)" || return 1
    gid="$(kc_group_id "$slug")" || return 1
    kcadm_cmd delete "users/$uid/groups/$gid" -r "$REALM" >/dev/null 2>&1 || return 1
    echo "seed-demo-all: removed $SEED_USER from /tenants/$slug (re-joined at the end)"
}

pack_of() { printf '%s' "${1#demo-}"; }

display_name_of() { # display_name_of <slug>
    case "$1" in
        demo-salon)       echo "Demo Salon & Barbershop" ;;
        demo-clinic)      echo "Demo Medical Clinic" ;;
        demo-consultancy) echo "Demo Consultancy" ;;
        demo-logistics)   echo "Demo Logistics" ;;
        *)                echo "Demo ${1#demo-}" ;;
    esac
}

pack_offerings_json() { # pack_offerings_json <pack-id> — stdlib-only extract
    python3 - "$PACK_DIR/$1.yaml" <<'PYEOF'
import json
import re
import sys

path = sys.argv[1]
try:
    lines = open(path, encoding="utf-8").read().splitlines()
except OSError:
    print("[]")
    sys.exit(0)

offerings = []
in_block = False
current = None
for raw in lines:
    if re.match(r"^offerings:\s*$", raw):
        in_block = True
        continue
    if not in_block:
        continue
    # A new top-level key ends the block.
    if re.match(r"^[a-zA-Z_]", raw):
        break
    m = re.match(r"^- name:\s*(.+?)\s*$", raw)
    if m:
        if current is not None:
            offerings.append(current)
        current = {"name": m.group(1).strip("\"'")}
        continue
    m = re.match(r"^\s+(duration_min|buffer_min|price_cents|capacity):\s*(\d+)", raw)
    if m and current is not None:
        current[m.group(1)] = int(m.group(2))
if current is not None:
    offerings.append(current)
print(json.dumps([o for o in offerings if "name" in o]))
PYEOF
}

pack_terminology_json() { # pack_terminology_json <pack-id>
    python3 - "$PACK_DIR/$1.yaml" <<'PYEOF'
import json
import re
import sys

path = sys.argv[1]
try:
    lines = open(path, encoding="utf-8").read().splitlines()
except OSError:
    print("{}")
    sys.exit(0)

terms = {}
in_block = False
for raw in lines:
    if re.match(r"^terminology:\s*$", raw):
        in_block = True
        continue
    if in_block and re.match(r"^[a-zA-Z_]", raw):
        break
    if in_block:
        m = re.match(r"^\s+(\w+):\s*(.+?)\s*$", raw)
        if m:
            terms[m.group(1)] = m.group(2).strip("\"'")
print(json.dumps(terms))
PYEOF
}

# --------------------------------------------------------------------------
# Per-tenant seed (requires $TOKEN whose tenant_slugs[0] == $slug)
# --------------------------------------------------------------------------
seed_tenant() { # seed_tenant <slug>
    local slug="$1" pack terms role offering_count
    local auth="Authorization: Bearer $TOKEN"
    pack="$(pack_of "$slug")"
    terms="$(pack_terminology_json "$pack")"
    role="$(printf '%s' "$terms" | jq -r '.team_member // "team member"')"

    echo "== $slug: tenant (industry: $pack) =="
    local body code
    body="$(jq -nc --arg slug "$slug" --arg name "$(display_name_of "$slug")" \
        --arg industry "$pack" --argjson terms "$terms" '{
        slug: $slug, name: $name, timezone: "Africa/Lagos",
        currency: "NGN", locale: "en-NG", industry: $industry,
        terminology: $terms
    }')"
    code="$(curl -s -o "/tmp/seed-demo-all-tenant-$slug.json" -w '%{http_code}' \
        -X POST "$GATEWAY/api/identity/v1/tenants" \
        -H "$auth" -H 'content-type: application/json' -d "$body")"
    case "$code" in
        2*) echo "tenant $slug created" ;;
        409) echo "tenant $slug already exists — reusing" ;;
        401) die "401 from identity — token missing/expired" ;;
        *) cat "/tmp/seed-demo-all-tenant-$slug.json" >&2; die "tenant $slug create failed (HTTP $code)" ;;
    esac

    echo "== $slug: offerings =="
    local offerings_json existing i name
    offerings_json="$(pack_offerings_json "$pack")"
    offering_count="$(printf '%s' "$offerings_json" | jq 'length')"
    [ "$offering_count" -gt 0 ] || die "pack $pack has no parseable offerings ($PACK_DIR/$pack.yaml)"
    existing="$(curl -sf "$GATEWAY/api/bookings/v1/offerings" -H "$auth" \
        | jq -r '.[].name // empty' 2>/dev/null || true)"
    for i in $(seq 0 $(( offering_count - 1 ))); do
        name="$(printf '%s' "$offerings_json" | jq -r ".[$i].name")"
        if printf '%s\n' "$existing" | grep -qxF "$name"; then
            echo "offering '$name' exists — skipping"
            continue
        fi
        printf '%s' "$offerings_json" | jq -c ".[$i] + {currency: \"NGN\", bookable: true}" \
            | curl -sf -X POST "$GATEWAY/api/bookings/v1/offerings" \
                -H "$auth" -H 'content-type: application/json' -d @- >/dev/null \
            || die "$slug: offering '$name' create failed"
        echo "offering '$name' created"
    done

    echo "== $slug: team member + availability =="
    local member_name="Demo ${role^}" member_id
    member_id="$(curl -sf "$GATEWAY/api/bookings/v1/team-members" -H "$auth" \
        | jq -r --arg n "$member_name" '.[] | select(.name == $n) | .id' | head -1)"
    if [ -z "$member_id" ]; then
        member_id="$(jq -nc --arg n "$member_name" \
            '{name: $n, email: "demo@example.com", role: $n, active: true}' \
            | curl -sf -X POST "$GATEWAY/api/bookings/v1/team-members" \
                -H "$auth" -H 'content-type: application/json' -d @- | jq -r .id)" \
            || die "$slug: team member create failed"
        echo "team member '$member_name' created"
    else
        echo "team member '$member_name' exists — reusing"
    fi
    [ -n "$member_id" ] && [ "$member_id" != "null" ] || die "$slug: no team member id"
    # Mon–Sat (1..6) 09:00–17:00 in minutes.
    jq -nc '{rules: [range(1;7) | {weekday: ., start_min: 540, end_min: 1020}]}' \
        | curl -sf -X PUT "$GATEWAY/api/bookings/v1/team-members/$member_id/availability" \
            -H "$auth" -H 'content-type: application/json' -d @- >/dev/null \
        || die "$slug: availability write failed"

    echo "== $slug: publish site =="
    jq -nc --arg n "$(display_name_of "$slug")" '{display_name: $n, published: true}' \
        | curl -sf -X PUT "$GATEWAY/api/bookings/v1/site" \
            -H "$auth" -H 'content-type: application/json' -d @- >/dev/null \
        || die "$slug: site publish failed"

    echo "== $slug: knowledge document =="
    local doc_title="Opening hours & policies"
    if ! curl -sf "$GATEWAY/api/knowledge/v1/documents?tenant=$slug" -H "$auth" \
        | jq -e --arg t "$doc_title" '.[] | select(.title == $t)' >/dev/null 2>&1; then
        jq -nc --arg slug "$slug" --arg title "$doc_title" \
            --arg body "$(display_name_of "$slug") — open Mon-Sat 09:00-17:00 (Africa/Lagos). Cancellations free up to 24h before the appointment; late cancellations may incur a no-show fee. Prices are fixed — no discounts." \
            '{tenant: $slug, title: $title, body: $body}' \
            | curl -sf -X POST "$GATEWAY/api/knowledge/v1/documents" \
                -H "$auth" -H 'content-type: application/json' -d @- >/dev/null \
            || die "$slug: knowledge doc create failed"
        echo "knowledge doc created"
    else
        echo "knowledge doc exists — skipping"
    fi

    echo "== $slug: $BOOKINGS_PER_TENANT simulated bookings (public path) =="
    local first_offering
    first_offering="$(curl -sf "$GATEWAY/api/bookings/v1/offerings" -H "$auth" \
        | jq -r '.[0].id // empty')"
    [ -n "$first_offering" ] || die "$slug: no offering id for booking seed"
    local n day slot starts member booked=0 date
    for n in $(seq 1 "$BOOKINGS_PER_TENANT"); do
        slot=""
        for day in $(seq 1 7); do
            date="$(date -u -d "+$day day" +%F 2>/dev/null || date -u -v+"$day"d +%F)"
            slot="$(curl -sf "$GATEWAY/api/bookings/public/sites/$slug/availability?offering_id=$first_offering&date=$date" \
                | jq -c '.slots[0] // .items[0] // empty' 2>/dev/null || true)"
            [ -n "$slot" ] && break
        done
        if [ -z "$slot" ]; then
            echo "seed-demo-all: WARNING: $slug — no public slots in the next 7 days; skipping booking $n" >&2
            continue
        fi
        starts="$(printf '%s' "$slot" | jq -r .starts_at)"
        member="$(printf '%s' "$slot" | jq -r .team_member_id)"
        jq -nc \
            --arg oid "$first_offering" --arg mid "$member" --arg starts "$starts" \
            --arg name "Demo Customer $n" --arg phone "+23480100000$n" \
            --arg email "demo$n@example.com" --arg ikey "demo-$slug-$n" '{
                offering_id: $oid, team_member_id: $mid, starts_at: $starts,
                contact: {name: $name, phone: $phone, email: $email},
                idempotency_key: $ikey, source: "demo-seed"
            }' \
            | curl -sf -X POST "$GATEWAY/api/bookings/public/sites/$slug/bookings" \
                -H 'content-type: application/json' -d @- >/dev/null \
            || die "$slug: booking $n create failed"
        booked=$((booked + 1))
    done
    echo "$slug: $booked/$BOOKINGS_PER_TENANT bookings seeded"

    curl -sf "$GATEWAY/api/bookings/public/sites/$slug/context" \
        | jq -e '.site.name // .tenant.slug' >/dev/null \
        || die "$slug: public context check failed"
    echo "$slug OK — booking page: $GATEWAY/p/$slug"
}

# --------------------------------------------------------------------------
# Main: one tenant per token round (see the header for the auth model)
# --------------------------------------------------------------------------
if [ "$TOKEN_SUPPLIED" = "0" ]; then
    TOKEN="$(fetch_token)" || die "token grant failed (is Direct Access Grants enabled on $CLIENT_ID? is $KEYCLOAK up?)"
fi
[ -n "$TOKEN" ] && [ "$TOKEN" != "null" ] || die "empty access token"

for slug in $DEMO_TENANTS; do
    if [ "$TOKEN_SUPPLIED" = "1" ]; then
        first="$(token_first_tenant "$TOKEN")"
        [ "$first" = "$slug" ] || die \
            "TOKEN tenant_slugs[0] is '$first', not '$slug' — the gateway strips X-Tenant-Slug and the first JWT claim wins, so one token can only seed its FIRST tenant safely. Pass a token scoped to $slug (or use SEED_USER/SEED_PASSWORD and let the script rotate group membership)."
    else
        if [ "$(token_first_tenant "$TOKEN")" != "$slug" ]; then
            echo "== $slug: rotating group membership for a scoped token =="
            join_tenant_group "$slug" || die \
                "could not auto-join /tenants/$slug. Manually: Keycloak admin console → Groups → /tenants/$slug → add $SEED_USER, then re-run (docs/runbooks/local-dev.md §3)."
            TOKEN="$(fetch_token)" || die "token refresh after group join failed"
            [ "$(token_first_tenant "$TOKEN")" = "$slug" ] || die \
                "token refetched but tenant_slugs[0] is still not '$slug' — is $SEED_USER a member of other tenant groups that sort first? Remove them or pass a scoped TOKEN."
        fi
    fi
    seed_tenant "$slug"
    if [ "$TOKEN_SUPPLIED" = "0" ]; then
        leave_tenant_group "$slug" || echo "seed-demo-all: WARNING: could not leave /tenants/$slug (continuing)" >&2
    fi
done

if [ "$TOKEN_SUPPLIED" = "0" ]; then
    echo "== Re-joining demo groups for dashboard browsing (best-effort) =="
    for slug in $DEMO_TENANTS; do
        join_tenant_group "$slug" >/dev/null 2>&1 || true
    done
fi

echo ""
echo "Demo seed complete. Tenants: $DEMO_TENANTS"
for slug in $DEMO_TENANTS; do
    echo "  $GATEWAY/p/$slug"
done
