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
| Tenant dashboard | http://localhost:3001/app/acme | Keycloak login `admin` / `admin123` |
| Public booking page | http://localhost:3001/p/acme or http://localhost:9080/p/acme | via web or via APISIX |
| APISIX gateway (proxy) | http://localhost:9080 | all `/api/*`, `/ws/*`, `/voice/*` traffic |
| APISIX admin | http://localhost:9180 | |
| Keycloak | http://localhost:8080 | admin console `admin` / `admin` (realm `master`); app realm is `opendesk` |
| Temporal UI | http://localhost:8233 | namespace `opendesk` |
| OpenSearch Dashboards | http://localhost:5601 | |
| MinIO console | http://localhost:9001 | |
| Spark master UI | http://localhost:8081 | |
| Trino | `make trino` | CLI: `iceberg.gold` |

Service health endpoints (direct): `:7001` identity, `:7002` booking,
`:7003` notification, `:7004` payments, `:7005` edge, `:7006` voice,
`:7007` conversation, `:7008` knowledge, `:7009` analytics — all `GET /healthz`.

## 3. Getting a Keycloak token for protected `/api/*` calls

Realm: `opendesk`. Token endpoint:
`http://localhost:8080/realms/opendesk/protocol/openid-connect/token`.
Demo user `admin` / `admin123` (member of group `/tenants/acme`, realm role
`owner`; tokens carry the `tenant_slugs` claim via the group mapper).

**Option A — password grant (dev convenience).** The `admin-web` client is a
public PKCE client with Direct Access Grants **disabled** by default. In dev,
enable it once: Keycloak admin console → realm `opendesk` → Clients →
`admin-web` → *Capability config* → enable **Direct access grants** → Save.
Then:

```bash
TOKEN=$(curl -sf -X POST \
  http://localhost:8080/realms/opendesk/protocol/openid-connect/token \
  -d grant_type=password -d client_id=admin-web \
  -d username=admin -d password=admin123 | jq -r .access_token)

curl -sf http://localhost:9080/api/bookings/v1/bookings \
  -H "Authorization: Bearer $TOKEN" -H "X-Tenant-Slug: acme" | jq .
```

**Option B — client credentials (works out of the box)** for
service-to-service style calls, using the confidential `service-accounts`
client (secret from `infra/keycloak/realm-opendesk.json`):

```bash
TOKEN=$(curl -sf -X POST \
  http://localhost:8080/realms/opendesk/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=service-accounts \
  -d client_secret=opendesk-service-secret | jq -r .access_token)
```

Note: booking-service runs with `AUTHZ_DISABLED=true` in dev (root compose),
so direct calls to `localhost:7002` only need `X-Tenant-Slug: acme` — no
token. The token path is only required when going through the gateway's
`openid-connect` / `jwt-auth` plugins.

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
| 401 from booking writes via direct port | `AUTHZ_DISABLED` not set and no JWT `sub` | Dev default sets `AUTHZ_DISABLED=true` in root compose; otherwise pass a token or `X-User-Id` |
| `invalid_client` on token request | Wrong client secret for `service-accounts`, or password grant against `admin-web` without enabling Direct Access Grants | Secret is `opendesk-service-secret` (realm import); see §3 |
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
