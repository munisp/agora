# Agora — Open-Source AI Receptionist Platform (SOTA)

> Agora (formerly OpenDesk) — internal module paths, Kafka topics, and env vars retain the opendesk identifier; see SPEC-W23.md.

A fully open-source, multi-tenant **AI receptionist / front-desk SaaS**: every appointment-based business gets a branded public booking page with a **voice + text AI concierge** that answers questions and books, reschedules, and cancels appointments live — plus a tenant dashboard for staff.

Agora is a state-of-the-art superset of the YouTube demo
[`sonnysangha/AI-Receptionist-Live-YouTube-Demo`](https://github.com/sonnysangha/AI-Receptionist-Live-YouTube-Demo)
(Next.js + Clerk + Convex + ElevenLabs). Same product, zero proprietary dependencies, plus an enterprise middleware backbone: **Kafka · Dapr · Fluvio · Temporal · Postgres · Keycloak · Permify · Redis · Mojaloop · OpenSearch · OpenAppSec · APISIX · TigerBeetle · Lakehouse (Iceberg/MinIO/Spark/Trino/dbt)**.

Services are written in **Go** (identity, booking, notification), **Rust** (payments/ledger, WS edge, Fluvio smart module), and **Python** (voice agent runtime, conversation, knowledge/RAG, analytics), with a Next.js web app.

## Why it supersedes the baseline

| Baseline | Agora |
|---|---|
| Clerk auth/orgs/billing | Keycloak OIDC + Permify ReBAC + TigerBeetle ledger + Mojaloop rails |
| Convex black-box reactivity | Postgres (RLS) + Kafka events + Rust WebSocket edge |
| ElevenLabs-only voice | Self-hosted LiveKit + whisper + Piper + Ollama/vLLM (ElevenLabs adapter optional) |
| Single shared agent, no durability | Temporal sagas with compensation; idempotent commands; DLQ |
| No analytics | Lakehouse bronze/silver/gold marts, Trino SQL, containment & revenue metrics |
| No edge security | APISIX + OpenAppSec WAF + rate limits |
| No CRM | Self-hosted Twenty CRM with bidirectional CRM sync (crm-sync): forward event sync + reverse webhook worker with echo suppression |
| One-size-fits-all flows | Industry workflow packs (salon, clinic, consultancy, support-desk): terminology, booking policy, persona + Temporal workflow variants |

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full design.

## Quick start

```bash
git clone <this repo> && cd opendesk
make config        # validate compose
make up            # build & start everything (~15 images, first build takes a while)
make seed          # demo tenant "acme" with catalog + knowledge
./scripts/seed-industries.sh   # 4 demo tenants, one per industry pack (acme-salon, acme-clinic, acme-consult, acme-support)
make smoke         # end-to-end checks through the gateway
```

Then open:

| UI | URL |
|---|---|
| Public booking page | http://localhost:9080/p/acme (or http://localhost:3001/p/acme) |
| Tenant dashboard | http://localhost:3001/app/acme (Keycloak login: `admin` / `admin123`) |
| CRM UI (Twenty) | http://localhost:3100 |
| Grafana (observability profile) | http://localhost:3002 |
| Keycloak admin | http://localhost:8080 |
| Temporal UI | http://localhost:8233 |
| OpenSearch Dashboards | http://localhost:5601 |
| APISIX admin | http://localhost:9180 |
| MinIO console | http://localhost:9001 |
| Trino | `make trino` |

Voice mode with local models: `make up-voice` (pulls Ollama `qwen3:8b` and a Piper voice — see `services/voice-agent-runtime/README.md` for the model-routing table, including the MiniMax-M2 long-context option).

Observability profile: `docker compose -f infra/docker-compose.observability.yml up` brings up Prometheus, Grafana (pre-provisioned dashboards for platform overview, Temporal/saga latency and AI/voice), an OTel collector and Loki. Backups: `infra/backups/backup.sh` (pg_dump + MinIO mirror + TigerBeetle data copy with rotation; `restore.sh` alongside).

## Wave 3 feature highlights

| Feature | Where |
|---|---|
| Streaming chat (SSE) | Public page chat widget streams LLM tokens from `/voice/chat` (`stream: true`), with request/response fallback |
| Theme editor + live preview | Dashboard → Public Site: primary colour, logo, hero title/subtitle, template — saved to `theme` jsonb with a live `/p/{slug}` preview pane |
| Embeddable widget | `<script src="…/embed.js" data-site="{slug}">` iframe loader + chromeless `/embed/{slug}` page — see [docs/embedding.md](docs/embedding.md) |
| Staff self-service | Dashboard → My Schedule (`GET /v1/bookings?mine=true`, resolved from the JWT email) |
| KB review queue | Dashboard → Knowledge → Review queue: approve/reject auto-drafted answers to unanswered questions |
| Conversational analytics | Dashboard → Analytics: plain-language questions over the lakehouse gold marts (guarded text-to-SQL, result table + SQL audit) |
| Pricing recommendations | Dashboard → Billing: lakehouse-computed peak multipliers / deposit % — human-review only, never auto-applied |
| Warm handoff | Voice runtime escalates to a LiveKit room; dashboard toast lets staff join the call (`EscalationRequested` over `/ws`) |
| Backups & observability | `infra/backups/` scripts + Prometheus/Grafana/OTel/Loki compose profile |
| Nigeria messaging channels | Termii / Africa's Talking / WhatsApp Cloud API via the `messaging-gateway` service + Dapr HTTP bindings, with per-tenant channel routing in the notification-worker — see [docs/integrations/messaging-channels.md](docs/integrations/messaging-channels.md) |
| Geospatial | PostGIS contact locations + service areas, Sedona gold geo marts, MapLibre/GeoLibre maps and geo-targeted campaigns — see [docs/geospatial.md](docs/geospatial.md) |

## Repository layout

```
infra/          middleware stacks (compose + configs): apisix, openappsec, keycloak, permify,
                kafka, fluvio, temporal, postgres, redis, tigerbeetle, mojaloop, opensearch, lakehouse
services/       Go / Rust / Python microservices (each with Dockerfile + README)
apps/admin-web/ Next.js dashboard + public booking page
docs/           ARCHITECTURE.md, ADRs, OpenAPI specs, runbooks
scripts/        seed + smoke test
SPEC.md         (../SPEC.md) the platform contract
```

## The 6 agent tools (parity with baseline, hardened)

`get_business_info` · `get_availability` · `book_appointment` · `lookup_appointment` · `reschedule_appointment` · `cancel_appointment`

All tools are executed server-side via Dapr service invocation; the tenant is resolved from the public site slug — the model can never supply an organization ID. Booking mutations require a confirmed contact phone number (browser sessions have no caller ID) enforced both in the agent and in the booking command validator.

## Development

Each service is independently runnable; see its README. Integration contracts (ports, topics, CloudEvents envelope, schemas) are defined in `../SPEC.md` — treat it as sacred.

## License

Apache-2.0
