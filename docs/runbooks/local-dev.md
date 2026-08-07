# Runbook — Local development bring-up

Audience: developers running the full Agora stack on a laptop.
Prereqs: Docker with compose v2 (`docker compose version`), `make`, `curl`, `jq`.
First build pulls/builds ~15 images — expect 10–20 minutes.

## 1. Bring-up sequence

```bash
cd opendesk

make config     # validate the merged compose config (root docker-compose.yml
                # includes infra/docker-compose.{core,edge,lakehouse}.yml)

make up         # docker compose up -d --build — middleware + services + web

make seed       # scripts/seed-demo.sh — creates demo tenant "acme"
                # (Europe/London, GBP, pro), two offerings and a knowledge doc.
                # Hits services DIRECTLY on :7001/:7002/:7008 — gateway /api/*
                # routes require a JWT (see §3).

make smoke      # scripts/smoke-test.sh — health of all 9 services, public
                # context + availability through the gateway, voice text turn,
                # ledger balance, knowledge search, trino/iceberg reachability.
```

Useful follow-ups:

```bash
make ps         # container states (look for "unhealthy" / restarts)
make logs       # tail all logs
make topics     # re-run the idempotent Kafka topic init (SPEC §4 topics)
make down       # stop;  make clean = stop + DELETE volumes (fresh state)
```

## 2. Where each UI lives

| UI | URL | Notes |
|---|---|---|
| Tenant dashboard | http://localhost:9080/app/acme | Keycloak login — create a dev realm user first (§3; the seeded `admin`/`admin123` was removed in W34 GF5) |
| Public booking page | http://localhost:9080/p/acme | via APISIX catch-all → web upstream (web :3001 is no longer host-published, W34 GF4) |
| APISIX gateway (proxy) | http://localhost:9080 | all `/api/*`, `/ws/*`, `/voice/*` traffic |
| APISIX admin | http://127.0.0.1:9180 | loopback-only bind since W34 GF4 |
| Keycloak | http://localhost:8080 | admin console `admin` / `$KC_BOOTSTRAP_ADMIN_PASSWORD` (must be exported before `make up` — no default since W34 GF5); app realm is `opendesk` |
| Temporal UI | http://localhost:8233 | namespace `opendesk` |
| OpenSearch Dashboards | http://localhost:5601 | |
| MinIO console | http://localhost:9001 | |
| Spark master UI | http://localhost:8081 | |
| Trino | `make trino` | CLI: `iceberg.gold` |

Service health endpoints: the per-service ports (`:7001` identity … `:7009`
analytics) are **no longer host-published** (W34 GF4). Probe them inside the
compose network instead, e.g.
`docker compose exec apisix curl -sf http://identity:7001/healthz` —
all services answer `GET /healthz`.

## 3. Getting a Keycloak token for protected `/api/*` calls

Realm: `opendesk`. Token endpoint:
`http://localhost:8080/realms/opendesk/protocol/openid-connect/token`.
No realm user ships out of the box — W34 GF5 removed the seeded
`admin`/`admin123`. Create a dev user per `infra/keycloak/README.md`
(kcadm `create users` + `set-password` + `add-roles --rolename owner`, then
add to group `/tenants/acme` in the admin console so tokens carry the
`tenant_slugs` claim). The examples below assume user `dev-admin`.

**Option A — password grant (dev convenience).** The `admin-web` client is a
public PKCE client with Direct Access Grants **disabled** by default. In dev,
enable it once: Keycloak admin console → realm `opendesk` → Clients →
`admin-web` → *Capability config* → enable **Direct access grants** → Save.
Then:

```bash
TOKEN=$(curl -sf -X POST \
  http://localhost:8080/realms/opendesk/protocol/openid-connect/token \
  -d grant_type=password -d client_id=admin-web \
  -d username=dev-admin -d password='<dev-only-password>' | jq -r .access_token)

curl -sf http://localhost:9080/api/bookings/v1/bookings \
  -H "Authorization: Bearer $TOKEN" -H "X-Tenant-Slug: acme" | jq .
```

**Option B — client credentials** for service-to-service style calls, using
the confidential `service-accounts` client. The shipped realm carries the
`CHANGE_ME_DEV_ONLY` placeholder, **not** a working secret — set a real one
first (kcadm flow in `infra/keycloak/README.md`) and export it via
`KEYCLOAK_ADMIN_CLIENT_SECRET` in `.env`:

```bash
TOKEN=$(curl -sf -X POST \
  http://localhost:8080/realms/opendesk/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=service-accounts \
  -d client_secret="$KEYCLOAK_ADMIN_CLIENT_SECRET" | jq -r .access_token)
```

Note: service ports are not host-published (W34 GF4), so all local traffic
goes through the gateway's `openid-connect` / `jwt-auth` plugins with a
token. The old dev bypass (`AUTHZ_DISABLED=true` + direct `:7002` calls) was
removed in W34 (GF4/GF12) — the gateway also strips client-supplied
`x-user-*` / `x-tenant-*` headers before proxying.

## 4. Voice profile

```bash
make up-voice   # docker compose --profile voice up -d --build
```

Adds three `voice`-profile containers plus the LiveKit worker:

* `ollama` (:11434) + `ollama-init`, which pulls `llama3.1:8b` on first run
  (multi-GB download — the voice agent will fail tool-less replies until the
  pull finishes; watch `docker compose logs -f ollama-init`).
* `piper` TTS sidecar (:5500, built from `services/voice-agent-runtime/sidecar`).
* `voice-worker` (`python -m app.livekit_worker`) joining LiveKit (:7880).

Without the profile, the voice runtime still serves `/voice/chat` text turns
but expects an external OpenAI-compatible LLM endpoint.

## 5. Common failure table

| Symptom | Likely cause | Fix |
|---|---|---|
| `keycloak` crash loop / 502 on :8080 | Postgres not ready yet, or `keycloak` DB missing (init scripts only run on an **empty** postgres volume) | `docker compose logs keycloak`; if the DB is missing: `make clean && make up` (destroys volumes), or `docker exec postgres createdb -U opendesk keycloak` |
| `postgres` init scripts not applied | Volume existed from a previous run | `make clean` then `make up` |
| `permify` crash loop | Postgres backend unavailable, or schema migrations pending | `docker compose logs permify`; ensure `postgres` healthy; restart `permify` |
| `permify-schema-loader` / `kafka-topics` / `fluvio-topics` show `Exited` | They are one-shot init jobs — **normal** | `docker compose logs kafka-topics` should list all SPEC §4 topics |
| `temporal` crash loop | `temporal` DB missing or postgres not ready | Same fix as keycloak; check `docker compose logs temporal` |
| Services 500 on publish/subscribe | Wrong Dapr pubsub **name**: the Kafka pubsub component is named `pubsub-kafka` — if you see publish failures, verify the component name is `pubsub-kafka` | Check the service's `DAPR_PUBSUB` env in root compose matches `infra/dapr/components/pubsub.kafka.yaml` metadata.name (`pubsub-kafka`) |
| 401 from booking writes via the gateway | Missing/expired bearer token, or client-supplied `x-user-*` headers (stripped at the gateway since W34 GF4) | Get a token per §3 and call `http://localhost:9080/api/bookings/...`; never inject identity headers client-side |
| `invalid_client` on token request | Wrong client secret for `service-accounts`, or password grant against `admin-web` without enabling Direct Access Grants | Secret comes from `.env` `KEYCLOAK_ADMIN_CLIENT_SECRET` and must match the realm (shipped value is the `CHANGE_ME_DEV_ONLY` placeholder — set a real one per `infra/keycloak/README.md`); see §3 |
| Voice agent replies time out | Ollama model still downloading (`ollama-init` one-shot) | `docker compose logs -f ollama-init`; wait for `llama3.1:8b` pull; Piper voice download likewise on first start |
| `mojaloop` unhealthy on `make ps` | Simulator first-boot slowness; healthcheck retries cover it | Usually recovers; `docker compose logs mojaloop` |
| `make config` fails | YAML edit broke a compose fragment | Run `docker compose config` (no `-q`) for the exact error; all fragments are plain YAML |
| Fluvio container exits | `fluvio-run` flags drifted on `:latest` image bump | See `infra/fluvio/README.md`; pin the image tag or adjust `start-cluster.sh` |
| Booking POST returns 422 | Phone-confirmation policy: contact has no phone | Include `contact.phone` in the create payload (by design, SPEC §1) |

## 6. Ports cheat-sheet

Full matrix in SPEC §3. Most used: gateway 9080, web 3001, keycloak 8080,
temporal-ui 8233, postgres 5432, kafka 9092, redis 6379, permify 3476/3478,
tigerbeetle 3000, mojaloop 8444, opensearch 9200, minio 9000/9001,
iceberg-rest 8181, trino 8088, livekit 7880.
