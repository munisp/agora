# W44 — Phase-0 Ground-Truth Maps + F16 Build/Deploy/Environment Sweep

Date: 2026-08-24. Author: W44 sweep S3 (auditor+scribe). Subject: opendesk mirror @ current HEAD
(fresh-copied to /tmp/w44-clone for all builds; the mirror itself was only read + this document written).
Charter: /mnt/agents/output/W44-gap-analysis.md §3 (Phase-0 map coverage) and §6 work order S3.

Method notes / tooling honesty:
- Go builds: go1.23.4 (/tmp copy of /mnt/agents/output/go-toolchain), GOPROXY=goproxy.cn (proxy.golang.org
  unreachable from sandbox — first-pass "failures" were network i/o timeouts, retried; no code change made).
- Rust builds: rust 1.98.0 (2026-08-20 stable, installed to /tmp/rust from rsproxy.cn mirror; rustup's own
  installer channel was unreachable). crates.io direct was blocked; rsproxy sparse index used.
- Python: python3 `compileall` (stdlib; no third-party installs) per CI's own py_compile posture.
- Node: node 20.20.2 / npm 11.19.0 available; admin-web was actually built (npm ci + next build).
- `docker` CLI is NOT present in the sandbox, so `docker compose config -q` / image builds are NOT-RUN.

## 1. Fresh-clone build matrix (Task 1)

| Service | Lang | Command | Result |
|---|---|---|---|
| services/identity-service | Go | go build ./... | BUILD OK |
| services/booking-service | Go | go build ./... | BUILD OK |
| services/notification-worker | Go | go build ./... | BUILD OK |
| services/crm-sync-service | Go | go build ./... | BUILD OK |
| services/kyc-service | Go | go build ./... | BUILD OK |
| services/messaging-gateway | Go | go build ./... | BUILD OK |
| services/graph-sync | Go | go build ./... | BUILD OK |
| services/gateway-edge | Rust | cargo check (default features) | BUILD OK (needs `cmake` for rdkafka-sys static librdkafka; first attempt FAIL was sandbox-only missing cmake) |
| services/gateway-edge | Rust | cargo build --locked | BUILD OK |
| services/billing-engine | Rust | cargo check / cargo build --locked | BUILD OK / BUILD OK (lock consistent) |
| services/payments-service | Rust | cargo check (default features) / cargo build --locked | BUILD OK / BUILD OK — Cargo.lock CONSISTENT at snapshot time (FIN-P's lock regeneration had already landed in the mirror; note: `tb-live` feature = `dep:tigerbeetle-unofficial`, Cargo.toml:7-11, is NON-default and owned by another agent — NOT-RUN here per charter) |
| services/analytics-pipeline | Py | python -m compileall | BUILD OK |
| services/avatar-renderer | Py | compileall | BUILD OK |
| services/conversation-service | Py | compileall | BUILD OK |
| services/credit-bureau | Py | compileall | BUILD OK |
| services/fraud-engine | Py | compileall | BUILD OK |
| services/graph-ml | Py | compileall | BUILD OK |
| services/graph-service | Py | compileall | BUILD OK |
| services/knowledge-service | Py | compileall | BUILD OK |
| services/mms-tts | Py | compileall | BUILD OK |
| services/model-registry | Py | compileall | BUILD OK |
| services/voice-agent-runtime | Py | compileall | BUILD OK |
| services/xtts-tts | Py | compileall | BUILD OK |
| apps/admin-web | Node | npm ci && npm run build | BUILD OK (next build completed; lockfileVersion 3 present) |
| apps/mobile | Node | — | NOT-RUN (Expo RN app; CI lane is npm ci + typecheck only — no build step exists; package-lock.json v3 present) |
| apps/field-pwa | JS (dep-free) | node --check app.js sw.js (CI lane) | NOT-RUN here (no build step; dependency-free static PWA; CI gate is syntax-only) |
| apps/marketing | static | — | NOT-RUN (static HTML/JS/CSS, no package.json — nothing to build) |

Python note: pytest --collect-only was not run because third-party test deps (fastapi/asyncpg/torch…) are not
installed in the sandbox; py_compile is the dependency-free equivalent the repo's own CI uses for 9 of 12
Python services (ci.yml python matrix), so this matches the repo's declared bar.

Lockfile consistency (cargo build --locked):
- gateway-edge: Cargo.lock consistent (EXIT=0).
- billing-engine: `cargo build --locked` EXIT=0 — lock consistent with Cargo.toml.
- payments-service: `cargo build --locked` EXIT=0 — lock consistent at snapshot time. The W43 in-wave
  lock drift (found by H, owned by FIN-P) is RESOLVED in the current mirror; recorded as
  known-in-flight-closed, not a new finding. Warning only: sqlx-postgres 0.7.4 future-incompat notice.

CI cross-note: ci.yml's rust lane runs `cargo check --locked || cargo check` with `continue-on-error: true`
(.github/workflows/ci.yml, rust job) — i.e. CI can never fail on lock drift; the local-only parked status of
ci.yml is DOCUMENTED (EXTERNAL_BLOCKED, workflow-scope 404) and is recorded, not re-reported.

## 2. Dockerfile-only / scaffold check + port-map divergence (Task 2)

Every `services/*/Dockerfile` corresponds to real, compilable code (see matrix). FROM lines verified for all
22 service Dockerfiles + apps/admin-web. No scaffold-only Dockerfiles found. Scaffold findings instead are
at the *deployment wiring* layer:

### 2.1 Wiring coverage matrix

| Service (port) | root docker-compose.yml | infra/docker-compose.* / infra/compose/* | apisix upstream | deploy/k3s |
|---|---|---|---|---|
| identity (7001) | yes | core | identity:7001 | identity-deployment.yaml |
| booking (7002) | yes | core | booking:7002 | booking-deployment.yaml |
| notification (7003) | yes | core | notification:7003 | notification-deployment.yaml |
| payments (7004) | yes | — | payments:7004 | NO (README: not yet) |
| edge (7005) | yes | — | edge:7005 | NO |
| voice (7006) | yes | — | voice:7006 | voice-worker.yaml (worker only) |
| conversation (7007) | yes | — | conversation:7007 | NO |
| knowledge (7008) | yes | — | knowledge:7008 | NO |
| analytics (7009) | yes | lakehouse | none (batch worker — OK) | NO |
| crm-sync (7010) | yes | crm | none (event consumer — OK) | NO |
| messaging-gateway (7011) | yes | — | messaging-gateway:7011 | NO |
| billing-engine (7012) | yes | — | billing:7012 | NO |
| kyc (7013) | yes | — | none (internal; booking reaches it via LENDING_KYC_URL / KYC events — OK) | NO |
| graph-service (7014) | NO | graph only | graph-service:7014 | NO |
| graph-sync (7015) | NO | graph only | none (consumer — OK) | NO |
| graph-ml (7016) | NO | graph only | none (batch — OK) | NO |
| fraud-engine (7017) | NO | graph only | none (consumer — OK) | NO |
| model-registry (7019) | NO | core + observability | none (internal — OK) | NO |
| credit-bureau (7022) | NO | NO | none | NO — **ORPHAN (Finding F16-1)** |
| mms-tts (5800) | NO | voices.compose.yml override only | none (internal to voice — OK) | NO |
| xtts-tts (5810) | NO | voices.compose.yml override only | none (internal to voice — OK) | NO |
| piper sidecar (5500) | yes (build ./services/voice-agent-runtime/sidecar) | — | none — OK | NO |
| avatar-renderer (no HTTP port; LiveKit publisher) | NO | avatar.compose.yml override (profile voice) | none — OK | NO |
| admin-web (3001) | yes | core | web:3001 | NO |
| twenty-api (3000) | external image (infra/twenty) | crm | twenty-api:3000 | NO |
| geolibre (8085) | external image | geolibre.compose.yml | geolibre:8085 | NO |
| livekit (7880/7881/7882) | external image | — | none (WS direct) | livekit-server.yaml |

### 2.2 Port-map divergence

- Code-default ports == compose PORT == apisix upstream port for all 13 routed services
  (verified source: identity config.go:36, booking config.go:125, notification config.go:90,
  payments config.rs:118, edge config.rs:58, voice config.py:182, conversation config.py:142,
  knowledge config.py:14, analytics config.py:77, crm-sync config.go:45, messaging-gateway
  config.go:70, billing config.rs:128, kyc config.go:33, graph-service config.py:27,
  graph-sync config.go:71, graph-ml config.py:25, fraud-engine config.py:38,
  model-registry config.py:50, credit-bureau config.py:33, mms-tts main.py:119, xtts-tts main.py:227,
  piper piper_server.py:27, admin-web Dockerfile:22 PORT=3001).
- k3s containerPort/PORT values match (booking 7002, identity 7001, notification 7003).
- No numeric port divergence found. The divergences are *env-name* level (see Findings F16-2/F16-3).

## 3. Phase-0 ground-truth maps (Task 4)

### 3.1 Service map
Table in §2.1 IS the service map: 22 in-repo services + 4 in-repo apps, each with its real listen port
from source (file:line above), its deployment wiring, and its route exposure.
Routes source of truth: `infra/apisix/apisix.yaml` (edge routes, per-route plugins listed in §3.4) fan out
to upstreams `<service>:<port>`; in-service routers are: Go chi muxes in `internal/httpapi/server.go`,
Rust axum in `src/routes.rs` (payments routes.rs:234-243, billing routes.rs:247-257), FastAPI routers per
`app/main.py`. Internal (non-gateway) traffic: Dapr sidecar invoke (DAPR_HTTP_PORT 3500), direct HTTP
(*_URL vars), Kafka topics, Temporal task queue `opendesk-main`.
Coverage: 100% of services with a Dockerfile are mapped. Not covered: exact per-handler route tables of
Go/Python services (only payments/billing enumerated handler-by-handler); dapr pub/sub subscription
manifests (infra/dapr) were not enumerated line-by-line.

### 3.2 Money map

| Surface | Store / codes | Mutations (routes) | Status enums |
|---|---|---|---|
| payments-service | TigerBeetle cluster (TB_CLUSTER_ID, LEDGER_IMPL=tigerbeetle) or sim; accounts `tenant:{id}:deposits` (code 10), `tenant:{id}:revenue` (20), `platform:fees` (30), `platform:clearing` (31), `platform:payouts` (32); transfer codes 100 deposit-hold(+post/void legs), 101 capture, 102 refund, 103 no-show-fee, 104 payout (src/ledger/mod.rs:46-60); no-overdraft on deposits/revenue (mod.rs no_overdraft) | POST /v1/deposits (hold), /v1/deposits/:id/capture, /v1/refunds, /v1/no-show-fee, /v1/payouts, /v1/internal/accounts/provision, /activities/hold-deposit, /activities/void-hold (routes.rs:235-243) | TransferState pending/posted/voided (mod.rs); payout attempts durable in PG (payout_attempts, sqlx) |
| billing-engine | PG: invoices, rate_cards, plan_presets, usage_records, ledger_accounts, ledger_transfers, billing_outbox, processed_events (migrations 0001-0005); BILLING_LEDGER_IMPL=postgres | /v1/invoices generate/issue/void/payment-link, /v1/rate-cards PUT, /webhooks/paystack (routes.rs:249-257); dunning cron DUNNING_INTERVAL_S | invoice FSM (draft→issued→paid/void; hardened W43 B-02) |
| booking-service | PG: commission_ledger, commission_payouts, commission_rules, referrals, loyalty_wallets/loyalty_ledger/loyalty_programs, loan_applications/loan_accounts/loan_products/lending_ledger/repayments, promo_codes/promo_redemptions, campaign_spend (bootstrap DDL in internal/**/store|ledger.go) | commission payout + recon workflows (cmd/server/main.go:195-202 PAYOUT_PROVIDER / PAYOUT_MOCK), referral payouts, lending disbursement consumer (internal/consumer/lending_disbursements.go — rail-first TB bridge LENDING_TB_BRIDGE_URL or ALLOW_MOCK_RAILS mock), promo redeem /v1/promo/redeem, mark-no-show | booking FSM (reserve→confirm→complete/cancel/no-show); stranded-pending sweeper BOOKING_SWEEPER_* |
| credit-bureau | PG (model_registry DSNs? own DB) — score read model only | /score read (booking lending score_client.go:101) | n/a |
| fees/pricing | PLATFORM_FEE_BPS (payments config.rs:111, default 250), PAYOUT_MIN_NGN (booking config.go:176), INVOICE_DUE_DAYS, rate_cards | — | — |

Money representation: integer minor units on the TB/ledger paths; W43 F13 fixed flutterwave float cents,
currency exponent, mixed-currency subtotals, fee_bps overflow; residual DATA#11 int4 price_cents is a
documented limit. NGN-only guard (P-13).
Coverage: payments + billing 100% of money routes enumerated; booking money tables enumerated from
bootstrap DDL (all CREATE TABLE names captured) but not every handler traced line-by-line. Not covered:
per-column type audit of every money column; admin-web displayed money (W44 S2's scope).

### 3.3 Trust-boundary map

External egress (real third-party calls):
- Rails/money: Paystack (billing-engine webhooks+payment links; booking PAYOUT_PROVIDER paystack default),
  Flutterwave (payments-service src/flutterwave.rs), Mojaloop (payments src/mojaloop.rs; sim opt-in).
- Comms: Termii / Africa's Talking / eBulkSMS (messaging-gateway SMS_PROVIDER_CHAIN), Meta WhatsApp Cloud
  (messaging-gateway + notification-worker), Telegram (messaging-gateway), FCM + APNS push
  (notification-worker), SMTP/Twilio bindings (notification MESSAGING_CHANNELS).
- CRM/identity: Twenty API (crm-sync TWENTY_API_URL), Keycloak admin (identity KEYCLOAK_ADMIN_*).
- Voice/AI: LiveKit (voice, avatar), Ollama/LLM (conversation, voice, graph-service, graph-sync),
  ElevenLabs/Tavus/Spitch (voice/avatar optional providers), Whisper/XTTS/MMS/piper (TTS/STT sidecars).
- Geo/data: Nominatim (booking GEOCODE_ENABLED), S3/MinIO exports+lakehouse, OpenSearch, TigerBeetle,
  FalkorDB, Fluvio, Temporal, Permify.

Webhook/event ingress (unauthenticated at gateway BY DESIGN, must authenticate at service):
- /webhooks/* → messaging-gateway (Meta HMAC WHATSAPP_APP_SECRET / verify token, Telegram secret token,
  Termii/AT callbacks; WHATSAPP_MOCK=1 unsigned intake is a documented dev opt-in).
- /webhooks/paystack → billing-engine (Paystack signature verify).
- /api/webhooks/flutterwave → payments (FLUTTERWAVE_SECRET_HASH verify).
- Twenty → crm-sync webhook (TWENTY_WEBHOOK_SECRET HMAC, constant-time N-09).
- Incident webhooks → booking /v1/incidents/ingest (INCIDENT_WEBHOOK_SECRETS).
- Kafka topics (all consumers), Temporal workflows, Dapr pub/sub.

Admin/control procedures: APISIX `uri-blocker` on /internal + /activities + /dev for every routed service;
internal-token gates (§3.4); backup/restore (infra/backups, fail-loud post-G-04); seed + drift gate
(scripts/seeds, Makefile seed-drift); model-registry admin DSNs; Keycloak admin client; k3s secret.yaml
(dev-only DB credentials, GF12-labelled).
Coverage: all env-named external endpoints (every *_URL/*_BASE_URL var in §4 config map) cross-checked
against route table. Not covered: per-call TLS verification audit of each egress client; Dapr component
yaml trust (infra/dapr) not enumerated.

### 3.4 Gate map (declared control → where evaluated)

| Control | Declared at | Evaluated at |
|---|---|---|
| Keycloak OIDC on user APIs | apisix openid-connect plugin on /api/*, /crm, /gis, /* (apisix.yaml routes) | APISIX edge; admin-web middleware.ts; edge service JWT verify vs KEYCLOAK_JWKS_URL (gateway-edge config.rs:63-71) |
| Block /internal,/activities,/dev at edge | apisix uri-blocker routes (apisix.yaml, 12 blocker routes) | APISIX edge |
| payments internal token | PAYMENTS_INTERNAL_TOKEN (compose ${...:-} empty → **gate OFF when unset**) | payments src/auth.rs:5,57,65 (X-Internal-Token), OPENDESK_TRUST_DIRECT_TENANT |
| billing internal token | BILLING_INTERNAL_TOKEN | billing-engine config.rs:77-90 — env_required, FAILS CLOSED when unset (RS-002) |
| Permify authz | PERMIFY_* | booking config.go:130 PERMIFY_URL + AUTHZ_DISABLED=false/AUTHZ_OUTAGE_POLICY=fail_closed (config.go:157-158); identity config.go:42 |
| Tenant isolation | X-Tenant-Slug middleware + RLS | identity tenant middleware; billing 0002_rls.sql; per-service tenant scoping |
| Webhook HMAC | provider secrets | messaging-gateway/billing/payments/crm-sync verify at handler |
| DND / quiet hours | DND_ENFORCEMENT=true, QUIET_HOURS_* | notification-worker config.go:138 + activities |
| Webhook signing (notification outbound) | WEBHOOK_SIGNING_REQUIRED (default FALSE) | notification-worker httpapi/webhooks.go:111, webhooks/sign.go:28 |
| KYC provider real/mock | KYC_MOCK default 0 | kyc-service config.go:44; compose KYC_MOCK=${KYC_MOCK:-0} |
| Voice phone confirmation | PHONE_CONFIRMATION_REQUIRED default true | voice-agent-runtime config.py:239 |
| Mock money rails | ALLOW_MOCK_RAILS/PAYOUT_MOCK/SOCIAL_MOCK code-default OFF, compose-default ON | booking main.go:195-202,447,595; lending_disbursements.go:243 fail-closed |
| Ledger selection | LEDGER_IMPL | payments config.rs:65-85 fail-closed when unset (W43 P-10) |
| Mojaloop sim | MOJALOOP_ALLOW_SIM default false in code, ${...:-1} in compose | payments config.rs:87-100 fail-closed |
| Rate limits | apisix limit-count all routes; CIVIC_PUBLIC_RATE_* | apisix + booking config |
| SQL injection guard | sqlguard | notification-worker internal (Y-02) |

Coverage: every control surfaced by W43 F3 sweep + every *_ENFORCEMENT/*_REQUIRED/*_MOCK var in the env
extraction is mapped. Not covered: per-endpoint RBAC matrix inside each service (S1's top-20 table covers
money mutations); Permify schema-vs-enforcement parity.

## 4. Config / env map (Task 3)

Extraction: grep of `os.Getenv|envStr|envInt|envBool|envDur` (Go), `env::var|env_str|env_parse|env_bool`
(Rust), `os.environ.get|os.getenv|_env|_int|_env_int` (Python), `process.env.` (TS/JS) across services/,
apps/, infra/, scripts/ — 445 unique vars, 76 documented in one of the three .env.example files
(root .env.example, services/booking-service/.env.example, apps/admin-web/.env.example), i.e.
**only 17% documented; 369/445 undocumented**. Full machine-generated table: §4.1 below.

### Flagged vars (dangerous defaults / mock-by-default / secret-with-fallback)

| var | default | where | flag |
|---|---|---|---|
| PORTAL_SECRET | `opendesk-dev-portal-secret-change-in-prod` | booking config.go:164; compose :141 same default; k3s does NOT set it | secret-with-fallback (public repo value signs portal JWTs) |
| MMS_MOCK | `1` (mock ON) | mms-tts main.py:55 | code default contradicts voices.compose.yml "mocks default OFF (SIM-013)"; only compose `${MMS_MOCK:-0}` turns it off |
| XTTS_MOCK | `1` (mock ON) | xtts-tts main.py:137 | same as above |
| LIVEKIT_API_KEY/SECRET | `devkey`/`secret` | avatar-renderer main.py:41-42 | well-known livekit --dev creds (documented in avatar.compose.yml) |
| S3_ACCESS_KEY/SECRET_KEY | `minioadmin` | notification config.go:130-131; analytics/lakehouse | dev-cred fallback |
| AWS_ACCESS_KEY_ID/SECRET | `minioadmin` | infra/lakehouse spark graph_enrichment.py:655-659 | dev-cred fallback |
| PG_DSN (analytics) | `postgres://opendesk:opendesk@postgres:5432` | analytics config.py:93 | superuser DSN fallback (documented bootstrap-template policy, compose x-app-env:26-34) |
| LLM_API_KEY/OLLAMA_API_KEY | `ollama` | conversation config.py:159, graph-service config.py:87 | placeholder non-secret |
| PUBLIC_BASE_URL | `http://localhost:9080` | notification config.go:111 | wrong outside local compose (signed portal links) |
| PAYMENTS_INTERNAL_TOKEN | unset/empty ⇒ gate OFF (auth.rs Option) | payments config.rs:134; compose sets `${PAYMENTS_INTERNAL_TOKEN:-}` (EMPTY) | auth gate silently disabled in default compose |
| INTERNAL_TOKEN (graph) | unset/empty | graph-service config.py:59 | check enforcement semantics (S1 cross-check) |
| WEBHOOK_SIGNING_REQUIRED | `false` | notification config.go:125 | outbound webhooks unsigned unless opted in |
| COMPOSE dev mock rails | ALLOW_MOCK_RAILS/PAYOUT_MOCK/SOCIAL_MOCK/FCM_MOCK/WHATSAPP_MOCK all `${...:-1}` | docker-compose.yml:152-158, 227-232, 289-293 | documented DEV ONLY inline; default `make up` = mock payout rail + simulated sends |

### Undocumented-but-required (fail-closed at boot; must be discovered from source)
LEDGER_IMPL (payments config.rs:71-85), BILLING_INTERNAL_TOKEN (billing config.rs:87 env_required),
MOJALOOP_ENDPOINT (required unless MOJALOOP_ALLOW_SIM), BILLING_STATIC_ACCOUNT (billing config.rs:107/184,
compose default `OPENDESK/0123456789` — demo account number), plus 360+ optional vars (§4.1 doc column=N).

Config-map coverage: 100% of env reads matched by the four patterns above. Not covered: env reads via
dynamic names (fmt-built keys), Docker secrets files, Keycloak/permify/temporal container envs (third-party
images), and the `infra/` shell scripts' positional config.

### 4.1 Full env-var table (auto-extracted; read sites in services/ + infra, tests excluded)

| var | service(s) | default(s) | mock/sandbox-default | secret-with-fallback | in .env.example | example read site |
|---|---|---|---|---|---|---|
| AGENTS_CACHE_TTL_S | voice-agent-runtime | `30` | N | N | N | services/voice-agent-runtime/app/config.py:250 |
| AGENTS_REGISTRY_URL | voice-agent-runtime | `http://conversation:7007` | N | N | N | services/voice-agent-runtime/app/config.py:249 |
| AGENT_BACKEND | voice-agent-runtime | `livekit` | N | N | N | services/voice-agent-runtime/app/config.py:217 |
| AGENT_IDLE_PROCESSES | voice-agent-runtime | `2` | N | N | N | services/voice-agent-runtime/app/config.py:235 |
| ALERTS_TOPIC | model-registry | `ops.alerts` | N | N | N | services/model-registry/model_registry/config.py:55 |
| APNS_KEY_ID | notification-worker | (none) | N | N | N | services/notification-worker/internal/config/config.go:150 |
| APNS_KEY_P8 | notification-worker | (none) | N | N | N | services/notification-worker/internal/config/config.go:152 |
| APNS_TEAM_ID | notification-worker | (none) | N | N | N | services/notification-worker/internal/config/config.go:151 |
| APNS_TOPIC | notification-worker | (none) | N | N | N | services/notification-worker/internal/config/config.go:153 |
| APPS_LIFECYCLE_TOPIC | identity-service | `opendesk.apps.lifecycle.v1` | N | N | Y | services/identity-service/internal/config/config.go:49 |
| APP_GATE_CACHE_TTL_SECONDS | booking-service | `60` | N | N | N | services/booking-service/internal/config/config.go:185 |
| APP_GATE_ENABLED | booking-service | `false` | N | N | N | services/booking-service/internal/config/config.go:184 |
| ASK_ROW_CAP | graph-service | `100` | N | N | N | services/graph-service/app/config.py:98 |
| AT_API_KEY | messaging-gateway | (none) | N | N | Y | services/messaging-gateway/internal/config/config.go:74 |
| AT_BASE_URL | messaging-gateway | `https://api.africastalking.com` | N | N | N | services/messaging-gateway/internal/config/config.go:76 |
| AT_FROM | messaging-gateway | (none) | N | N | Y | services/messaging-gateway/internal/config/config.go:77 |
| AT_USERNAME | messaging-gateway | (none) | N | N | Y | services/messaging-gateway/internal/config/config.go:75 |
| AUDIENCE_TRAJECTORY_TOPIC | notification-worker | (none) | N | N | N | services/notification-worker/internal/activities/audience_intake.go:317 |
| AUTHZ_DISABLED | booking-service | `false` | N | N | Y | services/booking-service/internal/config/config.go:157 |
| AUTHZ_OUTAGE_POLICY | booking-service | `fail_closed` | N | N | Y | services/booking-service/internal/config/config.go:158 |
| AVATAR_DISCOVERY_INTERVAL_S | avatar-renderer | `5` | N | N | N | services/avatar-renderer/app/main.py:45 |
| AVATAR_FPS | avatar-renderer | `15` | N | N | N | services/avatar-renderer/app/main.py:49 |
| AVATAR_FRAME_HEIGHT | avatar-renderer | `240` | N | N | N | services/avatar-renderer/app/main.py:48 |
| AVATAR_FRAME_WIDTH | avatar-renderer | `320` | N | N | N | services/avatar-renderer/app/main.py:47 |
| AVATAR_PROVIDER | voice-agent-runtime | `none` | N | N | N | services/voice-agent-runtime/app/config.py:220 |
| AVATAR_RENDERER | voice-agent-runtime | `disabled` | N | N | N | services/voice-agent-runtime/app/config.py:224 |
| AVATAR_RENDERER_MODE | avatar-renderer, voice-agent-runtime | `mock` | Y | N | N | services/avatar-renderer/app/main.py:43 |
| AVATAR_ROOM_PREFIX | avatar-renderer | `site-` | N | N | N | services/avatar-renderer/app/main.py:44 |
| AWS_ACCESS_KEY_ID | analytics-pipeline, infra | `minioadmin` | N | Y | N | infra/lakehouse/spark/jobs/graph_enrichment.py:655 |
| AWS_ENDPOINT_URL | analytics-pipeline | `http://minio:9000` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:69 |
| AWS_REGION | analytics-pipeline | `us-east-1` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:71 |
| AWS_SECRET_ACCESS_KEY | analytics-pipeline, infra | `minioadmin` | N | Y | N | infra/lakehouse/spark/jobs/graph_enrichment.py:659 |
| AZURE_SPEECH_KEY | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:256 |
| AZURE_SPEECH_REGION | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:257 |
| BATCH_SIZE | analytics-pipeline | `500` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:50 |
| BOOKING_APP_ID | analytics-pipeline, crm-sync-service, notification-worker… | `booking` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:107 |
| BOOKING_COMMANDS_GROUP | booking-service | `booking-service-commands` | N | N | Y | services/booking-service/internal/config/config.go:146 |
| BOOKING_COMMANDS_TOPIC | booking-service, voice-agent-runtime | `opendesk.booking.commands` | N | N | Y | services/booking-service/internal/config/config.go:145 |
| BOOKING_EVENTS_TOPIC | booking-service, crm-sync-service, notification-worker | `opendesk.booking.events` | N | N | Y | services/booking-service/internal/config/config.go:134 |
| BOOKING_SWEEPER_ENABLED | booking-service | `true` | N | N | N | services/booking-service/internal/config/config.go:161 |
| BOOKING_SWEEPER_INTERVAL_SECONDS | booking-service | `120` | N | N | N | services/booking-service/internal/config/config.go:162 |
| BOOKING_URL | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:95 |
| CACHE_STALE_TTL_SECONDS | booking-service | `900` | N | N | Y | services/booking-service/internal/config/config.go:152 |
| CACHE_TTL_SECONDS | booking-service | `120` | N | N | Y | services/booking-service/internal/config/config.go:151 |
| CAC_EVENTS_GROUP | analytics-pipeline | `analytics-cac` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:86 |
| CAC_EVENTS_TABLE | infra | `iceberg.bronze.cac_events` | N | N | N | infra/lakehouse/spark/jobs/cac_analytics.py:99 |
| CAC_EVENTS_TOPIC | analytics-pipeline, booking-service | `cac.events` | N | N | Y | services/analytics-pipeline/analytics_pipeline/config.py:83 |
| CAC_H3_RESOLUTION | infra | `8` | N | N | N | infra/lakehouse/spark/jobs/cac_analytics.py:104 |
| CAC_LGA_BOUNDARIES_FORMAT | infra | `parquet` | N | N | N | infra/lakehouse/spark/jobs/cac_analytics.py:111 |
| CAPTURES_TOPIC | crm-sync-service | `opendesk.conversation.captures` | N | N | N | services/crm-sync-service/internal/config/config.go:59 |
| CAPTURE_CONSUMER_GROUP | crm-sync-service | `crm-sync-capture` | N | N | N | services/crm-sync-service/internal/config/config.go:60 |
| CAPTURE_GROUP | conversation-service | `conversation-capture` | N | N | N | services/conversation-service/app/config.py:181 |
| CAPTURE_LOOKBACK_HOURS | fraud-engine | `24` | N | N | N | services/fraud-engine/fraud_engine/config.py:89 |
| CAPTURE_SUSTAINED_WINDOWS | fraud-engine | `3` | N | N | N | services/fraud-engine/fraud_engine/config.py:87 |
| CAPTURE_TOPIC | conversation-service | `opendesk.conversation.captures` | N | N | N | services/conversation-service/app/config.py:180 |
| CAPTURE_VELOCITY_MAX | fraud-engine | `30` | N | N | N | services/fraud-engine/fraud_engine/config.py:84 |
| CAPTURE_WINDOW_MIN | fraud-engine | `60` | N | N | N | services/fraud-engine/fraud_engine/config.py:85 |
| CHANNEL_SITE_MAP | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:90 |
| CIVIC_COORD_CASE_THRESHOLD | fraud-engine | `3` | N | N | N | services/fraud-engine/fraud_engine/config.py:114 |
| CIVIC_COORD_WINDOW_HOURS | fraud-engine | `24` | N | N | N | services/fraud-engine/fraud_engine/config.py:120 |
| CIVIC_ESCALATION_TOPIC | notification-worker | `opendesk.civic.events.v1` | N | N | N | services/notification-worker/internal/config/config.go:122 |
| CIVIC_EVENTS_GROUP | notification-worker | `notification-civic` | N | N | N | services/notification-worker/internal/config/config.go:121 |
| CIVIC_EVENTS_TOPIC | booking-service, notification-worker | `opendesk.civic.events.v1` | N | N | N | services/booking-service/internal/config/config.go:180 |
| CIVIC_PUBLIC_RATE_PER_DAY | booking-service | `50` | N | N | N | services/booking-service/internal/config/config.go:182 |
| CIVIC_PUBLIC_RATE_PER_HOUR | booking-service | `10` | N | N | N | services/booking-service/internal/config/config.go:181 |
| CIVIC_REPORT_LOOKBACK_DAYS | fraud-engine | `7` | N | N | N | services/fraud-engine/fraud_engine/config.py:108 |
| CIVIC_REPORT_MAX_PER_DAY | fraud-engine | `5` | N | N | N | services/fraud-engine/fraud_engine/config.py:105 |
| CIVIC_STATUS_CHANNEL | notification-worker | `sms` | N | N | N | services/notification-worker/internal/config/config.go:124 |
| COMMISSIONS_ENABLED | booking-service | `true` | N | N | Y | services/booking-service/cmd/server/main.go:82 |
| CONSENT_ERASURE_TOPIC | identity-service | `opendesk.consent.erasure.v1` | N | N | N | services/identity-service/internal/config/config.go:48 |
| CONSUMER_ENABLED | booking-service, crm-sync-service, graph-sync | `true` | N | N | Y | services/booking-service/internal/config/config.go:159 |
| CONSUMER_GROUP | crm-sync-service | `crm-sync` | N | N | N | services/crm-sync-service/internal/config/config.go:63 |
| CONVERSATIONS_INDEX | conversation-service | `conversations` | N | N | N | services/conversation-service/app/config.py:163 |
| CONVERSATION_APP_ID | notification-worker | `conversation` | N | N | N | services/notification-worker/internal/config/config.go:126 |
| CONVERSATION_EVENTS_TOPIC | crm-sync-service, notification-worker | `opendesk.conversation.events` | N | N | N | services/crm-sync-service/internal/config/config.go:57 |
| CONVERSATION_URL | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:91 |
| COPILOT_MODE | voice-agent-runtime | `true` | N | N | N | services/voice-agent-runtime/app/config.py:241 |
| CREDIT_BUREAU_TENANT_ID | booking-service | (none) | N | N | N | services/booking-service/internal/lending/score_client.go:116 |
| CREDIT_BUREAU_URL | booking-service | (none) | N | N | N | services/booking-service/internal/lending/score_client.go:101 |
| CRM360_EVENTS_TOPIC | booking-service | `opendesk.crm.events.v1` | N | N | N | services/booking-service/internal/config/config.go:204 |
| CRM_EVENTS_TOPIC | crm-sync-service, notification-worker | `opendesk.crm.events` | N | N | N | services/crm-sync-service/internal/config/config.go:61 |
| CRM_SYNC_APP_ID | notification-worker | `crm-sync` | N | N | N | services/notification-worker/internal/config/config.go:100 |
| DAPR_HOST | analytics-pipeline, booking-service, conversation-service… | `daprd-analytics`, `daprd-booking`, `daprd-conversation`, `daprd-crm-sync`, `daprd-identity`, `daprd-kyc`, `daprd-notification`, `daprd-voice` | N | N | Y | services/analytics-pipeline/analytics_pipeline/config.py:103 |
| DAPR_HTTP_PORT | analytics-pipeline, booking-service, conversation-service… | `3500` | N | N | Y | services/analytics-pipeline/analytics_pipeline/config.py:105 |
| DAPR_PUBSUB_NAME | booking-service, conversation-service, crm-sync-service… | `pubsub-kafka` | N | N | Y | services/booking-service/internal/config/config.go:133 |
| DATABASE_URL | booking-service, crm-sync-service, identity-service… | (none) | N | N | Y | scripts/seeds/_lib.py:229 |
| DLQ_TOPIC | booking-service, crm-sync-service | `opendesk.dlq` | N | N | Y | services/booking-service/internal/config/config.go:147 |
| DND_ENFORCEMENT | notification-worker | `true` | N | N | N | services/notification-worker/internal/config/config.go:138 |
| DRIFT_INTERVAL_MINUTES | model-registry | `15` | N | N | N | services/model-registry/model_registry/config.py:56 |
| DRIFT_MANIFEST_DIR | model-registry | `/data/manifests` | N | N | N | services/model-registry/model_registry/config.py:58 |
| DRIFT_PSI_THRESHOLD | model-registry | `0.25` | N | N | N | services/model-registry/model_registry/config.py:57 |
| DUNNING_INTERVAL_S | billing-engine | `3600` | N | N | N | services/billing-engine/src/config.rs:152 |
| EBULK_API_KEY | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:96 |
| EBULK_BASE_URL | messaging-gateway | `https://api.ebulksms.com` | N | N | N | services/messaging-gateway/internal/config/config.go:99 |
| EBULK_SENDER | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:98 |
| EBULK_USERNAME | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:97 |
| ELEVENLABS_AGENT_ID | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:219 |
| ELEVENLABS_API_KEY | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:218 |
| ENRICHED_TOPIC | conversation-service | `opendesk.conversation.enriched` | N | N | N | services/conversation-service/app/config.py:161 |
| EVAL_PERSONA_OVERRIDE | voice-agent-runtime | `false` | N | N | N | services/voice-agent-runtime/app/config.py:262 |
| FALKORDB_ADDR | graph-sync | `graph-db:6379` | N | N | N | services/graph-sync/internal/config/config.go:83 |
| FALKORDB_GRAPH | fraud-engine, graph-service, graph-sync | `agora_tenants`, `opendesk` | N | N | N | services/fraud-engine/fraud_engine/config.py:45 |
| FALKORDB_HOST | fraud-engine, graph-service | `localhost` | Y | N | N | services/fraud-engine/fraud_engine/config.py:43 |
| FALKORDB_PASSWORD | fraud-engine, graph-service | (none) | N | N | N | services/fraud-engine/fraud_engine/config.py:47 |
| FALKORDB_PORT | fraud-engine, graph-ml, graph-service | `6379` | N | N | N | services/fraud-engine/fraud_engine/config.py:44 |
| FALKORDB_USERNAME | fraud-engine, graph-service | (none) | N | N | N | services/fraud-engine/fraud_engine/config.py:46 |
| FCM_BASE_URL | notification-worker | (none) | N | N | N | services/notification-worker/internal/config/config.go:149 |
| FCM_CREDENTIALS_JSON | notification-worker | (none) | N | N | N | services/notification-worker/internal/config/config.go:147 |
| FCM_MOCK | notification-worker | `false` | N | N | N | services/notification-worker/internal/config/config.go:145 |
| FCM_PROJECT_ID | notification-worker | (none) | N | N | N | services/notification-worker/internal/config/config.go:148 |
| FCM_SERVER_KEY | notification-worker | (none) | N | N | N | services/notification-worker/internal/config/config.go:146 |
| FIELD_CAPTURE_BATCH_LIMIT | booking-service | `100` | N | N | Y | services/booking-service/internal/config/config.go:178 |
| FLUSH_INTERVAL | analytics-pipeline | `15` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:52 |
| FLUTTERWAVE_BASE_URL | payments-service | (none) | N | N | N | services/payments-service/src/flutterwave.rs:103 |
| FLUTTERWAVE_REDIRECT_URL | payments-service | (none) | N | N | N | services/payments-service/src/flutterwave.rs:109 |
| FLUTTERWAVE_SECRET_HASH | payments-service | (none) | N | N | N | services/payments-service/src/flutterwave.rs:106 |
| FLUTTERWAVE_SECRET_KEY | payments-service | (none) | N | N | N | services/payments-service/src/flutterwave.rs:105 |
| FLUVIO_PARTITIONS | gateway-edge | `6` | N | N | N | services/gateway-edge/src/config.rs:78 |
| FLUVIO_TOPIC | conversation-service | `opendesk.transcripts-raw` | N | N | N | services/conversation-service/app/config.py:154 |
| FRAUD_ALERTS_TOPIC | fraud-engine, graph-service | `opendesk.fraud.alerts.v1` | N | N | N | services/fraud-engine/fraud_engine/config.py:64 |
| FRAUD_KAFKA_GROUP | fraud-engine | `fraud-engine` | N | N | N | services/fraud-engine/fraud_engine/config.py:54 |
| FRAUD_ML_REGISTRY_DIR | fraud-engine | (none) | N | N | N | services/fraud-engine/fraud_engine/config.py:140 |
| FRAUD_SWEEP_MINUTES | fraud-engine | `15` | N | N | N | services/fraud-engine/fraud_engine/config.py:68 |
| GEOCODE_BASE_URL | booking-service | `https://nominatim.openstreetmap.org` | N | N | Y | services/booking-service/internal/config/config.go:167 |
| GEOCODE_ENABLED | booking-service | `false` | N | N | Y | services/booking-service/internal/config/config.go:166 |
| GEO_CAMPAIGN_BATCH | booking-service | `50` | N | N | Y | services/booking-service/internal/config/config.go:168 |
| GEO_H3_RESOLUTION | infra | `8` | N | N | N | infra/lakehouse/spark/jobs/geo_analytics.py:69 |
| GEO_LOOKBACK_HOURS | fraud-engine | `24` | N | N | N | services/fraud-engine/fraud_engine/config.py:93 |
| GEO_SERVICE_AREAS_FORMAT | infra | `parquet` | N | N | N | infra/lakehouse/spark/jobs/geo_analytics.py:76 |
| GHOST_LOOKBACK_DAYS | fraud-engine | `7` | N | N | N | services/fraud-engine/fraud_engine/config.py:98 |
| GHOST_MIN | fraud-engine | `3` | N | N | N | services/fraud-engine/fraud_engine/config.py:96 |
| GHOST_WINDOW_MIN | fraud-engine | `10` | N | N | N | services/fraud-engine/fraud_engine/config.py:97 |
| GIT_SHA | credit-bureau, model-registry | `unknown` | N | N | N | services/credit-bureau/credit_bureau/ml/train.py:117 |
| GNN_HEAD | graph-ml | `link` | N | N | N | services/graph-ml/graph_ml/gnn_train.py:508 |
| GNN_HEAD_PATIENCE | graph-ml | `20` | N | N | N | services/graph-ml/graph_ml/config.py:62 |
| GNN_HEAD_VAL_FRACTION | graph-ml | (none) | N | N | N | services/graph-ml/graph_ml/config.py:65 |
| GRAPH_ASK_MODEL | graph-service | `qwen2.5:7b-instruct` | N | N | N | services/graph-service/app/config.py:85 |
| GRAPH_BACKEND | graph-service | `falkordb` | N | N | N | services/graph-service/app/config.py:32 |
| GRAPH_ENRICH_CAC_WINDOW_DAYS | infra | `30` | N | N | N | infra/lakehouse/spark/jobs/graph_enrichment.py:125 |
| GRAPH_ENRICH_COMMISSION_WINDOW_DAYS | infra | `30` | N | N | N | infra/lakehouse/spark/jobs/graph_enrichment.py:126 |
| GRAPH_ENRICH_LEADS_TABLE | infra | `iceberg.bronze.cac_events` | N | N | N | infra/lakehouse/spark/jobs/graph_enrichment.py:112 |
| GRAPH_ENRICH_OUTPUT | infra | `kafka` | N | N | N | infra/lakehouse/spark/jobs/graph_enrichment.py:120 |
| GRAPH_ERASURE_DONE_TOPIC | graph-sync | `opendesk.graph.erasure.done.v1` | N | N | N | services/graph-sync/internal/config/config.go:82 |
| GRAPH_EXPORT_EDGES_PATH | infra | `s3://lake/extracts/graph_edges/` | N | N | N | infra/lakehouse/spark/jobs/graph_export.py:95 |
| GRAPH_EXPORT_NODES_PATH | infra | `s3://lake/extracts/graph_nodes/` | N | N | N | infra/lakehouse/spark/jobs/graph_export.py:94 |
| GRAPH_EXPORT_TRAJ_TABLE | infra | `iceberg.bronze.usage_events` | N | N | N | infra/lakehouse/spark/jobs/graph_export.py:96 |
| GRAPH_JWT_AUDIENCE | graph-service | (none) | N | N | N | services/graph-service/app/config.py:54 |
| GRAPH_JWT_ISSUER | graph-service | (none) | N | N | N | services/graph-service/app/config.py:53 |
| GRAPH_ML_EPOCHS | graph-ml | `200` | N | N | N | services/graph-ml/graph_ml/config.py:51 |
| GRAPH_ML_GNN_MIN_EDGES | graph-ml | `30` | N | N | N | services/graph-ml/graph_ml/config.py:56 |
| GRAPH_ML_GNN_MIN_PERSONS | graph-ml | `20` | N | N | N | services/graph-ml/graph_ml/config.py:54 |
| GRAPH_ML_HIDDEN_DIM | graph-ml | `64` | N | N | N | services/graph-ml/graph_ml/config.py:52 |
| GRAPH_ML_MODEL_DIR | graph-ml | `./models` | N | N | N | services/graph-ml/graph_ml/gnn_train.py:514 |
| GRAPH_ML_SEED | graph-ml | `42` | N | N | N | services/graph-ml/graph_ml/config.py:50 |
| GRAPH_ML_TOP_K | graph-ml | `5` | N | N | N | services/graph-ml/graph_ml/config.py:73 |
| GRAPH_ML_TRAIN_INTERVAL_MINUTES | graph-ml | `0` | N | N | N | services/graph-ml/graph_ml/config.py:69 |
| GRAPH_SERVICE_URL | notification-worker | (none) | N | N | N | services/notification-worker/internal/activities/audience_intake.go:313 |
| GRAPH_SYNC_BOOKING_TOPIC | graph-sync | `opendesk.booking.events` | N | N | N | services/graph-sync/internal/config/config.go:74 |
| GRAPH_SYNC_CAC_TOPIC | graph-sync | (none) | N | N | N | services/graph-sync/internal/config/config.go:78 |
| GRAPH_SYNC_DLQ_TOPIC | graph-sync | `opendesk.dlq` | N | N | N | services/graph-sync/internal/config/config.go:81 |
| GRAPH_SYNC_ENRICHMENT_TOPIC | graph-sync | `opendesk.graph.enrichment.v1` | N | N | N | services/graph-sync/internal/config/config.go:79 |
| GRAPH_SYNC_ERASURE_TOPIC | graph-sync | `opendesk.consent.erasure.v1` | N | N | N | services/graph-sync/internal/config/config.go:77 |
| GRAPH_SYNC_GROUP | graph-sync | `graph-sync` | N | N | N | services/graph-sync/internal/config/config.go:73 |
| GRAPH_SYNC_IDENTITY_TOPIC | graph-sync | `opendesk.identity.events` | N | N | N | services/graph-sync/internal/config/config.go:75 |
| GRAPH_SYNC_TRANSCRIPTS_TOPIC | graph-sync | `opendesk.conversation.transcripts` | N | N | N | services/graph-sync/internal/config/config.go:76 |
| HELPDESK_DB_MAX_CONNS | booking-service | `4` | N | N | N | services/booking-service/internal/config/config.go:191 |
| HELPDESK_EVENTS_TOPIC | booking-service | `opendesk.helpdesk.events.v1` | N | N | N | services/booking-service/internal/config/config.go:189 |
| HELPDESK_USAGE_TOPIC | booking-service | `opendesk.usage.events` | N | N | N | services/booking-service/internal/config/config.go:190 |
| HOST | analytics-pipeline, fraud-engine, graph-service | `0.0.0.0` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:78 |
| HTTP_TIMEOUT_S | voice-agent-runtime | `15` | N | N | N | services/voice-agent-runtime/app/config.py:233 |
| ICEBERG_REST_URI | analytics-pipeline, infra | `http://iceberg-rest:8181` | N | N | N | infra/lakehouse/spark/jobs/graph_enrichment.py:644 |
| ICEBERG_WAREHOUSE | analytics-pipeline | `s3://lake/warehouse` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:60 |
| IDENTITY_APP_ID | analytics-pipeline, booking-service, conversation-service… | `identity` | N | N | Y | services/analytics-pipeline/analytics_pipeline/config.py:110 |
| IDENTITY_BASE_URL | booking-service, kyc-service | (none) | N | N | N | services/booking-service/internal/config/config.go:140 |
| IDENTITY_EVENTS_TOPIC | crm-sync-service, identity-service | `opendesk.identity.events` | N | N | N | services/crm-sync-service/internal/config/config.go:55 |
| IDENTITY_URL | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:101 |
| INCIDENTS_GROUP | booking-service | `booking-incidents` | N | N | Y | services/booking-service/internal/config/config.go:170 |
| INCIDENTS_TOPIC | booking-service, conversation-service | `opendesk.incidents` | N | N | Y | services/booking-service/internal/config/config.go:169 |
| INCIDENT_AUTO_DISPATCH | booking-service | `true` | N | N | Y | services/booking-service/internal/config/config.go:171 |
| INCIDENT_ENABLED | conversation-service | `true` | N | N | N | services/conversation-service/app/config.py:189 |
| INCIDENT_MIN_SCORE | conversation-service | `0.6` | N | N | N | services/conversation-service/app/config.py:190 |
| INCIDENT_RETRY_SECONDS | conversation-service | `30` | N | N | N | services/conversation-service/app/config.py:194 |
| INCIDENT_WEBHOOK_SECRETS | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:94 |
| INDEXER_BULK_FLUSH_SECONDS | conversation-service | `2` | N | N | N | services/conversation-service/app/config.py:167 |
| INDEXER_BULK_SIZE | conversation-service | `100` | N | N | N | services/conversation-service/app/config.py:166 |
| INDEXER_ENABLED | conversation-service | `true` | N | N | N | services/conversation-service/app/config.py:164 |
| INDEXER_GROUP | conversation-service | `conversation-service-indexer` | N | N | N | services/conversation-service/app/config.py:165 |
| INDUSTRIES_DIR | identity-service, notification-worker | `/industries` | N | N | N | services/identity-service/internal/config/config.go:50 |
| INTEL_LLM | conversation-service | `off` | N | N | N | services/conversation-service/app/config.py:156 |
| INTEL_LLM_API_KEY | conversation-service | (none) | N | N | N | services/conversation-service/app/config.py:159 |
| INTEL_LLM_BASE_URL | conversation-service | (none) | N | N | N | services/conversation-service/app/config.py:157 |
| INTEL_LLM_MODEL | conversation-service | (none) | N | N | N | services/conversation-service/app/config.py:158 |
| INTEL_LLM_TIMEOUT_S | conversation-service | `3` | N | N | N | services/conversation-service/app/config.py:160 |
| INTERNAL_DATABASE_URL | billing-engine | (none) | N | N | N | services/billing-engine/src/config.rs:133 |
| INTERNAL_TOKEN | graph-service | (none) | N | N | N | services/graph-service/app/config.py:59 |
| INVOICE_DUE_DAYS | billing-engine | `14` | N | N | N | services/billing-engine/src/config.rs:153 |
| JWKS_CACHE_TTL_SECS | gateway-edge | `300` | N | N | N | services/gateway-edge/src/config.rs:71 |
| JWT_ALGORITHM | graph-service | `RS256` | N | N | N | services/graph-service/app/config.py:50 |
| JWT_PUBLIC_KEY | graph-service | (none) | N | N | N | services/graph-service/app/config.py:49 |
| KAFKA_BOOTSTRAP | scripts | `localhost:9092` | Y | N | N | scripts/seeds/seed_events.py:197 |
| KAFKA_BOOTSTRAP_SERVERS | analytics-pipeline, fraud-engine, graph-service… | `kafka:9092`, `localhost:9092` | Y | N | N | services/analytics-pipeline/analytics_pipeline/config.py:31 |
| KAFKA_BROKERS | booking-service, conversation-service, crm-sync-service… | `kafka:9092` | N | N | Y | infra/lakehouse/spark/jobs/graph_enrichment.py:117 |
| KAFKA_CONSUMER_ENABLED | billing-engine, payments-service | `true` | N | N | N | services/billing-engine/src/config.rs:139 |
| KAFKA_GROUP_ID | analytics-pipeline | `analytics-pipeline` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:34 |
| KEYCLOAK_ADMIN_CLIENT_ID | identity-service | (none) | N | N | N | services/identity-service/internal/config/config.go:40 |
| KEYCLOAK_ADMIN_CLIENT_SECRET | identity-service | (none) | N | N | Y | services/identity-service/internal/config/config.go:41 |
| KEYCLOAK_AUDIENCE | gateway-edge | (none) | N | N | N | services/gateway-edge/src/config.rs:68 |
| KEYCLOAK_REALM | identity-service | `opendesk` | N | Y | N | services/identity-service/internal/config/config.go:39 |
| KEYCLOAK_URL | identity-service | `http://keycloak:8080` | N | Y | N | services/identity-service/internal/config/config.go:38 |
| KNOWLEDGE_APP_ID | notification-worker, voice-agent-runtime | `knowledge` | N | N | N | services/notification-worker/internal/config/config.go:99 |
| KNOWLEDGE_QUERY | voice-agent-runtime | `opening hours services pricing` | N | N | N | services/voice-agent-runtime/app/config.py:232 |
| KNOWLEDGE_SNIPPET_COUNT | voice-agent-runtime | `3` | N | N | N | services/voice-agent-runtime/app/config.py:231 |
| KYC_EVENTS_TOPIC | kyc-service | `opendesk.kyc.resolved.v1` | N | N | N | services/kyc-service/internal/config/config.go:38 |
| KYC_MOCK | kyc-service | `false` | N | N | N | services/kyc-service/internal/config/config.go:44 |
| KYC_PROVIDER_API_KEY | kyc-service | (none) | N | N | N | services/kyc-service/internal/config/config.go:46 |
| KYC_PROVIDER_URL | kyc-service | (none) | N | N | N | services/kyc-service/internal/config/config.go:45 |
| KYC_RESOLVE_TIMEOUT_SECONDS | kyc-service | `8` | N | N | N | services/kyc-service/internal/config/config.go:47 |
| LEAD_ATTRIBUTION_FIRST_TOUCH_ONLY | booking-service | `true` | N | N | Y | services/booking-service/internal/config/config.go:173 |
| LEDGER_IMPL | payments-service | (none) | N | N | N | services/payments-service/src/config.rs:72 |
| LENDING_DISBURSEMENTS_GROUP | booking-service | (none) | N | N | N | services/booking-service/cmd/server/main.go:603 |
| LENDING_EVENTS_TOPIC | booking-service | `opendesk.lending.events.v1` | N | N | N | services/booking-service/internal/config/config.go:209 |
| LENDING_KYC_URL | booking-service | (none) | N | N | N | services/booking-service/internal/config/config.go:210 |
| LIVEKIT_API_KEY | avatar-renderer, voice-agent-runtime | `devkey` | N | Y | Y | services/avatar-renderer/app/main.py:41 |
| LIVEKIT_API_SECRET | avatar-renderer, voice-agent-runtime | `secret` | Y | Y | Y | services/avatar-renderer/app/main.py:42 |
| LIVEKIT_URL | avatar-renderer, voice-agent-runtime | `ws://livekit:7880` | N | N | N | services/avatar-renderer/app/main.py:40 |
| LLM_API_KEY | conversation-service, voice-agent-runtime | `ollama` | N | Y | N | services/conversation-service/app/config.py:159 |
| LLM_BASE_URL | conversation-service, voice-agent-runtime | `http://ollama:11434/v1` | N | N | Y | services/conversation-service/app/config.py:157 |
| LLM_CB_COOLDOWN_S | voice-agent-runtime | `60` | N | N | N | services/voice-agent-runtime/app/config.py:207 |
| LLM_CB_FAILURES | voice-agent-runtime | `3` | N | N | N | services/voice-agent-runtime/app/config.py:206 |
| LLM_FALLBACK_API_KEY | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:205 |
| LLM_FALLBACK_BASE_URL | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:203 |
| LLM_FALLBACK_MODEL | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:204 |
| LLM_MODEL | conversation-service, voice-agent-runtime | `qwen3:8b` | N | N | Y | services/conversation-service/app/config.py:158 |
| LLM_TIMEOUT | voice-agent-runtime | `20` | N | N | N | services/voice-agent-runtime/app/config.py:202 |
| LOAD_THRESHOLD | voice-agent-runtime | `0.7` | N | N | N | services/voice-agent-runtime/app/config.py:236 |
| LOG_LEVEL | avatar-renderer, fraud-engine, graph-service… | `INFO`, `info` | N | N | N | services/avatar-renderer/app/main.py:228 |
| LOYALTY_EVENTS_TOPIC | booking-service | `opendesk.loyalty.events.v1` | N | N | N | services/booking-service/internal/config/config.go:195 |
| MCP_SERVERS | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/mcp_client.py:166 |
| MESSAGING_CHANNELS | notification-worker | `email:smtp,sms:twilio` | N | N | N | services/notification-worker/internal/config/config.go:106 |
| META_MOCK | booking-service | `0` | N | N | Y | services/booking-service/internal/config/config.go:219 |
| MMS_LANGS | mms-tts | `eng,pcm` | N | N | N | services/mms-tts/app/main.py:56 |
| MMS_MOCK | mms-tts | `1` | N | N | N | services/mms-tts/app/main.py:55 |
| MMS_TTS_URL | voice-agent-runtime | `http://mms-tts:5800` | N | N | N | services/voice-agent-runtime/app/config.py:254 |
| MODEL_REGISTRY_INTERNAL_DSN | model-registry | (none) | N | N | N | services/model-registry/model_registry/config.py:52 |
| MODEL_REGISTRY_PG_DSN | model-registry | (none) | N | N | N | services/model-registry/model_registry/config.py:51 |
| MOJALOOP_ALLOW_SIM | payments-service | `false` | N | N | N | services/payments-service/src/config.rs:90 |
| MOJALOOP_ENDPOINT | payments-service | (none) | N | N | N | services/payments-service/src/config.rs:91 |
| NOTIFICATIONS_OUTBOX_GROUP | notification-worker | `notification-outbox` | N | N | N | services/notification-worker/internal/config/config.go:119 |
| NOTIFICATIONS_OUTBOX_TOPIC | notification-worker | `opendesk.notifications.outbox` | N | N | N | services/notification-worker/internal/config/config.go:118 |
| NOTIFICATIONS_TOPIC | booking-service | `opendesk.notifications.outbox` | N | N | Y | services/booking-service/internal/config/config.go:165 |
| NOTIFICATION_APP_ID | identity-service | `notification` | N | N | N | services/identity-service/internal/config/config.go:47 |
| OLLAMA_API_KEY | graph-service | `ollama` | N | Y | N | services/graph-service/app/config.py:87 |
| OLLAMA_BASE_URL | graph-service, graph-sync | `http://localhost:11434/v1` | Y | N | N | services/graph-service/app/config.py:82 |
| OLLAMA_EMBED_MODEL | graph-sync | `nomic-embed-text` | N | N | N | services/graph-sync/internal/config/config.go:87 |
| OLLAMA_TIMEOUT_S | graph-service | `30` | N | N | N | services/graph-service/app/config.py:89 |
| OPENDESK_TRUST_DIRECT_TENANT | conversation-service, payments-service | `false`, `off` | N | N | N | services/conversation-service/app/config.py:192 |
| OPENSEARCH_ADDR | conversation-service | `http://opensearch:9200` | N | N | N | services/conversation-service/app/config.py:162 |
| OPENSEARCH_URL | notification-worker | `http://opensearch:9200` | N | N | N | services/notification-worker/internal/config/config.go:110 |
| OPS_ALERTS_TOPIC | notification-worker | `opendesk.ops.alerts` | N | N | N | services/notification-worker/internal/config/config.go:123 |
| OUTBOUND_BURST | notification-worker | `3` | N | N | N | services/notification-worker/internal/config/config.go:134 |
| OUTBOX_POLL_INTERVAL_SECONDS | booking-service | `2` | N | N | Y | services/booking-service/internal/config/config.go:155 |
| OUTBOX_RELAY_BATCH | conversation-service | `100` | N | N | N | services/conversation-service/app/config.py:197 |
| OUTBOX_RELAY_ENABLED | conversation-service | `true` | N | N | N | services/conversation-service/app/config.py:195 |
| OUTBOX_RELAY_SECONDS | conversation-service | `5` | N | N | N | services/conversation-service/app/config.py:196 |
| PACER_BACKEND | notification-worker | `redis` | N | N | N | services/notification-worker/internal/config/config.go:135 |
| PAYMENTS_APP_ID | notification-worker | `payments` | N | N | N | services/notification-worker/internal/config/config.go:97 |
| PAYMENTS_INTERNAL_TOKEN | payments-service | (none) | N | N | N | services/payments-service/src/config.rs:134 |
| PAYMENTS_TEST_DATABASE_URL | payments-service | (none) | N | N | N | services/payments-service/src/payouts.rs:611 |
| PAYOUT_MIN_NGN | booking-service | `100` | N | N | Y | services/booking-service/internal/config/config.go:176 |
| PAYOUT_PROVIDER | booking-service | `paystack` | N | N | Y | services/booking-service/internal/config/config.go:175 |
| PAYOUT_RECONCILER_INTERVAL_SECS | payments-service | `30` | N | N | N | services/payments-service/src/config.rs:141 |
| PAYSTACK_SECRET_KEY | billing-engine | (none) | N | N | N | services/billing-engine/src/config.rs:83 |
| PERMIFY_URL | booking-service, identity-service | `http://permify:3476` | N | N | Y | services/booking-service/internal/config/config.go:130 |
| PG_DATABASE | analytics-pipeline, booking-service, conversation-service | `analytics_meta`, `booking`, `conversation` | N | N | Y | services/analytics-pipeline/analytics_pipeline/config.py:96 |
| PG_DSN | analytics-pipeline, booking-service, conversation-service | `postgres://opendesk:opendesk@postgres:5432` | N | Y | Y | services/analytics-pipeline/analytics_pipeline/config.py:93 |
| PG_MAX_CONNS | booking-service | `20` | N | N | Y | services/booking-service/internal/config/config.go:129 |
| PG_MAX_SIZE | analytics-pipeline, conversation-service | `10`, `4` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:99 |
| PG_MIN_SIZE | analytics-pipeline, conversation-service | `1` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:98 |
| PG_PASS | booking-service | (none) | N | N | Y | services/booking-service/internal/config/config.go:243 |
| PG_USER | booking-service | (none) | N | N | Y | services/booking-service/internal/config/config.go:242 |
| PHONE_CONFIRMATION_REQUIRED | voice-agent-runtime | `true` | N | N | N | services/voice-agent-runtime/app/config.py:239 |
| PHONE_HASH_SALT | graph-service, graph-sync | (none) | N | N | N | services/graph-service/app/config.py:65 |
| PIPER_BIN | voice-agent-runtime | `piper` | N | N | N | services/voice-agent-runtime/app/config.py:214 |
| PIPER_HTTP_URL | voice-agent-runtime | `http://piper:5500` | N | N | N | services/voice-agent-runtime/app/config.py:212 |
| PIPER_MODE | voice-agent-runtime | `http` | N | N | N | services/voice-agent-runtime/app/config.py:211 |
| PIPER_MODEL_DIR | voice-agent-runtime | `/voices` | N | N | N | services/voice-agent-runtime/app/config.py:215 |
| PIPER_SAMPLE_RATE | voice-agent-runtime | `22050` | N | N | N | services/voice-agent-runtime/app/config.py:216 |
| PIPER_TIMEOUT_S | voice-agent-runtime | `30` | N | N | N | services/voice-agent-runtime/sidecar/piper_server.py:28 |
| PIPER_VOICE | voice-agent-runtime | `en_US-lessac-medium` | N | N | N | services/voice-agent-runtime/app/config.py:213 |
| PIPER_VOICE_MAP | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:251 |
| PLATFORM_FEE_BPS | payments-service | `250` | N | N | N | services/payments-service/src/config.rs:111 |
| PORT | analytics-pipeline, billing-engine, booking-service… | `5500`, `5800`, `5810`, `7001`, `7002`, `7003`, `7004`, `7005`, `7006`, `7007`, `7009`, `7010`, `7011`, `7012`, `7013`, `7014`, `7015`, `7016`, `7017`, `7019`, `7022` | N | N | Y | services/analytics-pipeline/analytics_pipeline/config.py:77 |
| PORTAL_SECRET | booking-service | `opendesk-dev-portal-secret-change-in-prod` | Y | Y | Y | services/booking-service/internal/config/config.go:164 |
| PRELOAD_MODELS | voice-agent-runtime | `true` | N | N | N | services/voice-agent-runtime/app/config.py:234 |
| PRIVACY_ENABLED | conversation-service | `true` | N | N | N | services/conversation-service/app/config.py:182 |
| PRIVACY_EVENTS_GROUP | booking-service, conversation-service | `booking-service-privacy`, `conversation-service-privacy` | N | N | Y | services/booking-service/internal/config/config.go:149 |
| PRIVACY_EVENTS_TOPIC | booking-service, conversation-service, crm-sync-service… | `opendesk.privacy.events` | N | N | Y | services/booking-service/internal/config/config.go:148 |
| PUBLIC_BASE_URL | notification-worker | `http://localhost:9080` | Y | N | N | services/notification-worker/internal/config/config.go:111 |
| QUALITY_ENRICH_ENABLED | conversation-service | `true` | N | N | N | services/conversation-service/app/config.py:168 |
| QUALITY_ENRICH_GROUP | conversation-service | `conversation-sentiment` | N | N | N | services/conversation-service/app/config.py:173 |
| QUALITY_EVENTS_TOPIC | conversation-service, crm-sync-service | `opendesk.conversation.quality` | N | N | N | services/conversation-service/app/config.py:172 |
| QUERY_ROW_CAP | graph-service | `100` | N | N | N | services/graph-service/app/config.py:99 |
| QUIET_HOURS_DEFAULT | booking-service, notification-worker | `20:00-08:00` | N | N | N | services/booking-service/internal/campaignstudio/handlers.go:494 |
| QUIET_HOURS_OVERRIDES | booking-service, notification-worker | (none) | N | N | N | services/booking-service/internal/campaignstudio/handlers.go:488 |
| RAY_ADDRESS | graph-ml | (none) | N | N | N | services/graph-ml/graph_ml/ray_train.py:375 |
| RAY_DATASETS_ROOT | graph-ml | `./datasets` | N | N | N | services/graph-ml/graph_ml/ray_train.py:348 |
| RECON_CRON | booking-service | `30 2 * * *` | N | N | Y | services/booking-service/cmd/server/main.go:227 |
| RECO_WINDOW_DAYS | infra | `30` | N | N | N | infra/lakehouse/spark/jobs/revenue_intelligence.py:44 |
| REDIS_ADDR | booking-service, notification-worker | `redis:6379` | N | N | Y | services/booking-service/internal/config/config.go:150 |
| REFERRAL_CYCLE_MAX_HOPS | fraud-engine | `4` | N | N | N | services/fraud-engine/fraud_engine/config.py:73 |
| REFERRAL_CYCLE_MIN_HOPS | fraud-engine | `2` | N | N | N | services/fraud-engine/fraud_engine/config.py:72 |
| REGISTRY_SYNC_DIR | infra | (none) | N | N | N | infra/lakehouse/spark/jobs/training_snapshot.py:900 |
| RETENTION_BATCH_SIZE | conversation-service | `1000` | N | N | N | services/conversation-service/app/config.py:188 |
| RETENTION_DAYS | conversation-service | `365` | N | N | N | services/conversation-service/app/config.py:186 |
| RETENTION_ENABLED | conversation-service | `true` | N | N | N | services/conversation-service/app/config.py:185 |
| RETENTION_SWEEP_SECONDS | conversation-service | `3600` | N | N | N | services/conversation-service/app/config.py:187 |
| REVERSE_CONSUMER_GROUP | crm-sync-service | `crm-sync-reverse` | N | N | N | services/crm-sync-service/internal/config/config.go:64 |
| REVERSE_ECHO_WINDOW_SECONDS | crm-sync-service | `10` | N | N | N | services/crm-sync-service/internal/config/config.go:66 |
| S3_ACCESS_KEY | notification-worker | `minioadmin` | N | Y | N | services/notification-worker/internal/config/config.go:130 |
| S3_ENDPOINT | infra, notification-worker | `http://minio:9000` | N | N | N | infra/lakehouse/spark/jobs/graph_enrichment.py:650 |
| S3_EXPORTS_BUCKET | notification-worker | `exports` | N | N | N | services/notification-worker/internal/config/config.go:132 |
| S3_REGION | notification-worker | `us-east-1` | N | N | N | services/notification-worker/internal/config/config.go:129 |
| S3_SECRET_KEY | notification-worker | `minioadmin` | N | Y | N | services/notification-worker/internal/config/config.go:131 |
| SCORE_INTERVAL_MINUTES | graph-ml | `60` | N | N | N | services/graph-ml/graph_ml/config.py:75 |
| SEED | credit-bureau, fraud-engine, scripts | `42` | N | N | N | scripts/seeds/naija_transactions.py:1025 |
| SEED_COVERAGE_TABLE | infra | `iceberg.cac_silver.coverage` | N | N | N | infra/lakehouse/spark/jobs/seed_coverage.py:74 |
| SEED_EVENTS_OUTBOX | scripts | (none) | N | N | N | scripts/seeds/seed_events.py:231 |
| SEED_GEO_POINTS_TABLE | infra | `iceberg.cac_silver.geo_points` | N | N | N | infra/lakehouse/spark/jobs/seed_geo_points.py:80 |
| SEED_GOLD_COSTS_TABLE | infra | `iceberg.cac_gold.channel_unit_costs` | N | N | N | infra/lakehouse/spark/jobs/seed_gold_load.py:77 |
| SEED_GOLD_FX_TABLE | infra | `iceberg.cac_gold.usd_shadow_prices` | N | N | N | infra/lakehouse/spark/jobs/seed_gold_load.py:78 |
| SEED_GOLD_RUN_LOG_TABLE | infra | `iceberg.cac_gold.seed_run_log` | N | N | N | infra/lakehouse/spark/jobs/seed_gold_load.py:79 |
| SEED_H3_RESOLUTION | infra | `8` | N | N | N | infra/lakehouse/spark/jobs/seed_geo_points.py:91 |
| SEED_KAFKA | scripts | `off` | N | N | Y | scripts/seeds/_lib.py:208 |
| SEED_KAFKA_BOOTSTRAP | scripts | `localhost:9092` | Y | N | N | scripts/seeds/_lib.py:182 |
| SEED_LGA_GEOJSON_PATH | infra | (none) | N | N | N | infra/lakehouse/spark/jobs/seed_geo_points.py:87 |
| SEED_MANIFEST_DIR | infra, scripts | `/var/tmp/seed_manifests` | N | N | Y | infra/lakehouse/spark/jobs/seed_gold_load.py:68 |
| SEED_REPORT_PATH | scripts | (none) | N | N | Y | scripts/seeds/_lib.py:216 |
| SEED_RUNNER_ID | infra | (none) | N | N | N | infra/lakehouse/spark/jobs/seed_gold_load.py:332 |
| SEED_SALT | infra, scripts | `opendesk-dev-seed-salt-change-in-prod` | Y | Y | Y | infra/lakehouse/spark/jobs/seed_coverage.py:71 |
| SEED_SCALE | infra, scripts | `1.0` | N | N | Y | infra/lakehouse/spark/jobs/seed_coverage.py:72 |
| SEGMENT_ROW_CAP | graph-service | `10000` | N | N | N | services/graph-service/app/config.py:100 |
| SEGMENT_STORE_DIR | graph-service | `./data/graph-service` | N | N | N | services/graph-service/app/config.py:94 |
| SHUTDOWN_TIMEOUT_SECONDS | booking-service, crm-sync-service, graph-sync… | `15`, `20` | N | N | Y | services/booking-service/internal/config/config.go:156 |
| SIGNAL_GROUP | notification-worker | `notification-signals` | N | N | N | services/notification-worker/internal/config/config.go:114 |
| SIP_DEFAULT_SITE | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:248 |
| SMS_PROVIDER_CHAIN | messaging-gateway | `africastalking,termii,ebulksms` | N | N | N | services/messaging-gateway/internal/config/config.go:100 |
| SMTP_BINDING | notification-worker | `bindings-smtp` | N | N | N | services/notification-worker/internal/config/config.go:104 |
| SMTP_FROM | notification-worker | `no-reply@opendesk.local` | N | N | N | services/notification-worker/internal/config/config.go:108 |
| SOCIAL_EVENTS_TOPIC | booking-service | `opendesk.social.events.v1` | N | N | Y | services/booking-service/internal/config/config.go:217 |
| SOCIAL_MOCK | booking-service | `0` | N | N | Y | services/booking-service/internal/config/config.go:218 |
| SPARK_JARS_PACKAGES | infra | (none) | N | N | N | infra/lakehouse/spark/jobs/graph_enrichment.py:624 |
| SPITCH_API_KEY | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:258 |
| SPITCH_BASE_URL | voice-agent-runtime | `https://api.spitch.app` | N | N | N | services/voice-agent-runtime/app/config.py:259 |
| STARTUP_MAX_ATTEMPTS | analytics-pipeline | `60` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:125 |
| STARTUP_RETRY_SECONDS | analytics-pipeline | `5` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:123 |
| STUDIO_DATABASE_URL | booking-service | (none) | N | N | N | services/booking-service/internal/config/config.go:196 |
| STUDIO_EVENTS_TOPIC | booking-service | `opendesk.studio.events.v1` | N | N | N | services/booking-service/internal/config/config.go:198 |
| STUDIO_STEP_BATCH | booking-service | `200` | N | N | N | services/booking-service/internal/config/config.go:197 |
| SURVEYS_DATABASE_URL | booking-service | (none) | N | N | N | services/booking-service/internal/config/config.go:208 |
| SURVEYS_EVENTS_TOPIC | booking-service | `opendesk.surveys.events.v1` | N | N | N | services/booking-service/internal/config/config.go:205 |
| SURVEYS_NOTIFICATIONS_TOPIC | booking-service | `opendesk.notifications.outbox` | N | N | N | services/booking-service/internal/config/config.go:206 |
| SURVEYS_PUBLIC_BASE_URL | booking-service | `https://app.opendesk.ng/s` | N | N | N | services/booking-service/internal/config/config.go:207 |
| SYBIL_HIGH_SIZE | fraud-engine | `5` | N | N | N | services/fraud-engine/fraud_engine/config.py:80 |
| SYBIL_LOOKBACK_HOURS | fraud-engine | `24` | N | N | N | services/fraud-engine/fraud_engine/config.py:81 |
| SYBIL_WINDOW_MIN | fraud-engine | `60` | N | N | N | services/fraud-engine/fraud_engine/config.py:76 |
| TAVUS_API_KEY | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:221 |
| TAVUS_PERSONA_ID | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:223 |
| TAVUS_REPLICA_ID | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:222 |
| TB_CLUSTER_ID | payments-service | `0` | N | N | N | services/payments-service/src/config.rs:121 |
| TELEGRAM_BASE_URL | messaging-gateway | `https://api.telegram.org` | N | N | N | services/messaging-gateway/internal/config/config.go:89 |
| TELEGRAM_BOT_TOKEN | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:86 |
| TELEGRAM_BOT_USERNAME | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:87 |
| TELEGRAM_WEBHOOK_SECRET | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:88 |
| TEMPORAL_HOST_PORT | booking-service, notification-worker | `temporal:7233` | N | N | Y | services/booking-service/internal/config/config.go:141 |
| TEMPORAL_NAMESPACE | booking-service, notification-worker | `opendesk` | N | N | Y | services/booking-service/internal/config/config.go:142 |
| TEMPORAL_TASK_QUEUE | booking-service, notification-worker | `opendesk-main` | N | N | Y | services/booking-service/internal/config/config.go:143 |
| TENANT_CACHE_TTL_SECONDS | analytics-pipeline, booking-service, conversation-service | `300` | N | N | Y | services/analytics-pipeline/analytics_pipeline/config.py:113 |
| TENANT_CHANNEL_MAP | notification-worker | (none) | N | N | N | services/notification-worker/internal/config/config.go:107 |
| TENANT_CONCURRENCY | graph-ml | `4` | N | N | N | services/graph-ml/graph_ml/config.py:77 |
| TENANT_PHONE_MAP | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:247 |
| TERMII_API_KEY | messaging-gateway | (none) | N | N | Y | services/messaging-gateway/internal/config/config.go:71 |
| TERMII_BASE_URL | messaging-gateway | `https://v2.api.termii.com` | N | N | N | services/messaging-gateway/internal/config/config.go:73 |
| TERMII_SENDER_ID | messaging-gateway | `OpenDesk` | N | N | Y | services/messaging-gateway/internal/config/config.go:72 |
| TIKTOK_MOCK | booking-service | `0` | N | N | Y | services/booking-service/internal/config/config.go:220 |
| TOOL_ACK_GRACE_MS | voice-agent-runtime | `400` | N | N | N | services/voice-agent-runtime/app/config.py:238 |
| TOOL_TIMEOUT_SECONDS | voice-agent-runtime | `4` | N | N | N | services/voice-agent-runtime/app/config.py:237 |
| TOPIC_BOOKING_EVENTS | analytics-pipeline | `opendesk.booking.events` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:37 |
| TOPIC_PAYMENT_EVENTS | analytics-pipeline | `opendesk.payments.events` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:40 |
| TOPIC_TRANSCRIPTS | analytics-pipeline | `opendesk.conversation.transcripts` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:43 |
| TOPIC_USAGE_EVENTS | analytics-pipeline | `opendesk.usage.events` | N | N | N | services/analytics-pipeline/analytics_pipeline/config.py:46 |
| TRAINING_BASE_PATH | infra | `s3://lake/training/` | N | N | N | infra/lakehouse/spark/jobs/training_snapshot.py:101 |
| TRAINING_CAC_TABLE | infra | `iceberg.bronze.cac_events` | N | N | N | infra/lakehouse/spark/jobs/training_snapshot.py:107 |
| TRAINING_EDGES_TABLE | infra | `iceberg.gold.graph_edge_features` | N | N | N | infra/lakehouse/spark/jobs/training_snapshot.py:106 |
| TRAINING_LABELS_PATH | infra | `s3://lake/extracts/labels/` | N | N | N | infra/lakehouse/spark/jobs/training_snapshot.py:108 |
| TRAINING_NODES_TABLE | infra | `iceberg.gold.graph_node_features` | N | N | N | infra/lakehouse/spark/jobs/training_snapshot.py:105 |
| TRAINING_SEED | infra | `42` | N | N | N | infra/lakehouse/spark/jobs/training_snapshot.py:103 |
| TRAINING_SNAPSHOT_DATE | infra | (none) | N | N | N | infra/lakehouse/spark/jobs/training_snapshot.py:102 |
| TRAIN_AUCPR_TOLERANCE | model-registry | `0.02` | N | N | N | services/model-registry/model_registry/config.py:62 |
| TRAIN_BRIER_MAX | model-registry | `0.20` | N | N | N | services/model-registry/model_registry/config.py:61 |
| TRAIN_CRON_HOUR | model-registry | `2` | N | N | N | services/model-registry/model_registry/config.py:59 |
| TRANSCRIPTS_TOPIC | conversation-service | `opendesk.conversation.transcripts` | N | N | N | services/conversation-service/app/config.py:152 |
| TRANSCRIPT_SINK | conversation-service | `kafka` | N | N | N | services/conversation-service/app/config.py:153 |
| TTS_CB_COOLDOWN_S | voice-agent-runtime | `60` | N | N | N | services/voice-agent-runtime/app/config.py:261 |
| TTS_CB_FAILURES | voice-agent-runtime | `3` | N | N | N | services/voice-agent-runtime/app/config.py:260 |
| TTS_PROVIDER_CHAIN | voice-agent-runtime | `piper` | N | N | N | services/voice-agent-runtime/app/config.py:252 |
| TTS_VOICE_MAP | voice-agent-runtime | (none) | N | N | N | services/voice-agent-runtime/app/config.py:253 |
| TWENTY_API_KEY | crm-sync-service | (none) | N | N | Y | services/crm-sync-service/internal/config/config.go:51 |
| TWENTY_API_URL | crm-sync-service | `http://twenty-api:3000` | N | N | N | services/crm-sync-service/internal/config/config.go:50 |
| TWENTY_RATE_PER_MIN | crm-sync-service | `90` | N | N | N | services/crm-sync-service/internal/config/config.go:53 |
| TWENTY_WEBHOOK_SECRET | crm-sync-service | (none) | N | N | Y | services/crm-sync-service/internal/config/config.go:52 |
| TWILIO_BINDING | notification-worker | `bindings-twilio` | N | N | N | services/notification-worker/internal/config/config.go:105 |
| TWILIO_FROM | notification-worker | `+10000000000` | N | N | N | services/notification-worker/internal/config/config.go:109 |
| USAGE_EVENTS_TOPIC | booking-service | `opendesk.usage.events` | N | N | Y | services/booking-service/internal/config/config.go:153 |
| USER | scripts | (none) | N | N | N | scripts/seeds/_lib.py:137 |
| USERNAME | scripts | (none) | N | N | N | scripts/seeds/_lib.py:137 |
| USSD_ENABLED | conversation-service | `true` | N | N | N | services/conversation-service/app/config.py:198 |
| USSD_SESSION_BACKEND | messaging-gateway | `memory` | N | N | N | services/messaging-gateway/internal/config/config.go:102 |
| USSD_SESSION_TTL_SECONDS | messaging-gateway | `180` | N | N | N | services/messaging-gateway/internal/config/config.go:104 |
| USSD_STATE_STORE | messaging-gateway | `statestore` | N | N | N | services/messaging-gateway/internal/config/config.go:103 |
| VOICEPRINTS | voice-agent-runtime | `off` | N | N | N | services/voice-agent-runtime/app/config.py:245 |
| VOICEPRINT_THRESHOLD | voice-agent-runtime | `0.75` | N | N | N | services/voice-agent-runtime/app/config.py:246 |
| VOICES_DIR | xtts-tts | `/data` | N | N | N | services/xtts-tts/app/main.py:138 |
| VOICE_RUNTIME_URL | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:92 |
| WEBHOOK_GROUP | notification-worker | `notification-webhooks` | N | N | N | services/notification-worker/internal/config/config.go:117 |
| WEBHOOK_SIGNING_REQUIRED | notification-worker | `false` | N | N | N | services/notification-worker/internal/config/config.go:125 |
| WHATSAPP_APP_SECRET | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:82 |
| WHATSAPP_BASE_URL | messaging-gateway | `https://graph.facebook.com/v21.0` | N | N | N | services/messaging-gateway/internal/config/config.go:80 |
| WHATSAPP_BUSINESS_ACCOUNT_ID | notification-worker | (none) | N | N | Y | services/notification-worker/internal/provider/whatsapp.go:107 |
| WHATSAPP_CLOUD_API_BASE_URL | notification-worker | (none) | N | N | Y | services/notification-worker/internal/provider/whatsapp.go:108 |
| WHATSAPP_CLOUD_API_TOKEN | notification-worker | (none) | N | N | Y | services/notification-worker/internal/provider/whatsapp.go:105 |
| WHATSAPP_MOCK | messaging-gateway, notification-worker | `false` | N | N | Y | services/messaging-gateway/internal/config/config.go:85 |
| WHATSAPP_PHONE_NUMBER_ID | messaging-gateway, notification-worker | (none) | N | N | Y | services/messaging-gateway/internal/config/config.go:79 |
| WHATSAPP_TOKEN | messaging-gateway | (none) | N | N | Y | services/messaging-gateway/internal/config/config.go:78 |
| WHATSAPP_VERIFY_TOKEN | messaging-gateway | (none) | N | N | N | services/messaging-gateway/internal/config/config.go:81 |
| WHISPER_COMPUTE_TYPE | voice-agent-runtime | `int8` | N | N | N | services/voice-agent-runtime/app/config.py:210 |
| WHISPER_DEVICE | voice-agent-runtime | `auto` | N | N | N | services/voice-agent-runtime/app/config.py:209 |
| WHISPER_MODEL | voice-agent-runtime | `base` | N | N | N | services/voice-agent-runtime/app/config.py:208 |
| WORKFORCE_EVENTS_TOPIC | booking-service | `opendesk.workforce.events.v1` | N | N | N | services/booking-service/internal/config/config.go:211 |
| WORKORDERS_FSM_EVENTS_TOPIC | booking-service | `opendesk.fsm.events.v1` | N | N | N | services/booking-service/internal/config/config.go:194 |
| WORKORDERS_NOTIFICATIONS_TOPIC | booking-service | `opendesk.notifications.outbox` | N | N | N | services/booking-service/internal/config/config.go:192 |
| WORKORDERS_USAGE_TOPIC | booking-service | `opendesk.usage.events` | N | N | N | services/booking-service/internal/config/config.go:193 |
| WS_CHANNEL_CAPACITY | gateway-edge | `256` | N | N | N | services/gateway-edge/src/config.rs:72 |
| XTTS_MOCK | xtts-tts | `1` | N | N | N | services/xtts-tts/app/main.py:137 |
| XTTS_TTS_URL | voice-agent-runtime | `http://xtts-tts:5810` | N | N | N | services/voice-agent-runtime/app/config.py:255 |
| X_MOCK | booking-service | `0` | N | N | Y | services/booking-service/internal/config/config.go:221 |

## 5. CI / deploy fiction (Task 5)

- `.github/workflows/ci.yml` is parked LOCAL-ONLY (workflow-scope 404 on push, W39/W41) — DOCUMENTED
  EXTERNAL_BLOCKED, recorded here per charter, not re-reported. Additional observation (new, low):
  the rust lane is `cargo check --locked || cargo check` + `continue-on-error: true` — structurally
  incapable of failing on Cargo.lock drift even once the workflow is unblocked.
- deploy/k3s vs compose divergence: k3s base carries only 3 of 13 app services (identity, booking,
  notification) + livekit/voice-worker; kustomization.yaml + README state this HONESTLY ("MVP",
  "remaining 7 app services are not included"). Not fiction. Real divergences found:
  * k3s sets `TEMPORAL_ADDRESS` (booking:73, identity:65, notification:55) — no code reads that name
    (Go services read TEMPORAL_HOST_PORT, booking config.go:141 / notification config.go:91); the
    default happens to equal the intended value, so this is dead config that silently stops working the
    day someone customizes it. Same dead var in root compose x-app-env:37 (9 services inherit it).
    Only temporal-ui legitimately consumes TEMPORAL_ADDRESS (infra/docker-compose.core.yml:191).
  * k3s sets `PERMIFY_HTTP` (booking:79, identity:63) and compose sets it 10x (docker-compose.yml:36,74,
    125,194,322,372,408,474,519,553) — no code reads PERMIFY_HTTP; Go services read PERMIFY_URL
    (booking config.go:130, identity config.go:42). Dead config, masked by identical default.
  * k3s does not set PORTAL_SECRET for booking → falls back to the repo-public dev default
    (config.go:164). k3s also has no Dapr sidecars while booking/notification default
    DAPR_HOST=daprd-booking/daprd-notification (config.go:131) → Dapr invoke/pubsub calls have no
    sidecar to reach in k3s; README admits "middleware is not in this base" but does not mention the
    sidecar gap (SUSPECTED runtime breakage for Dapr-dependent paths in k3s).
  * k3s images `opendesk-<svc>:latest` have no registry/build pipeline in-repo (README honestly says
    build-on-appliance / kind load). Not fiction, but "deployable" only by hand.
- Compose vs code mock posture is documented inline (W39 contract comments at docker-compose.yml:152-158
  etc.) — the honest inversion is the two TTS code defaults (Finding F16-4).
- Nothing in README/Makefile claims production deployability beyond the compose stack; Makefile `up` is
  explicitly the full local platform. Backup/restore fail-loud contract verified present
  (infra/backups/backup.sh header, post-W43 G-04); seed drift gate present (Makefile:63-66 seed-drift).

## 6. Findings (file:line evidence; CONFIRMED vs SUSPECTED)

| # | Status | Severity | Finding | Evidence | User-facing lie |
|---|---|---|---|---|---|
| F16-1 | CONFIRMED | MEDIUM | credit-bureau (port 7022) is an orphan: real code + Dockerfile + tests, but absent from every compose file, apisix upstream, and k3s. booking's CREDIT_BUREAU_URL integration (score_client.go:101) would point at a nonexistent deployment; unset → silent fallback to local Score() | services/credit-bureau/ exists; zero hits in docker-compose.yml, infra/docker-compose.*, infra/compose/*, infra/apisix/apisix.yaml (only a comment at :284), deploy/k3s/ | "Credit scoring service" exists as deployable capability; standard deploy has no such service, lending silently uses local scoring |
| F16-2 | CONFIRMED | MEDIUM | PERMIFY_HTTP set in 2 k3s manifests + 10 compose service blocks; nothing reads it (code reads PERMIFY_URL). Dead config masked by identical default | deploy/k3s/booking-deployment.yaml:79, identity-deployment.yaml:63; docker-compose.yml:36 et al.; booking config.go:130, identity config.go:42 | "Authorization service URL is configured" — the configured value is ignored; any non-default value silently does nothing |
| F16-3 | CONFIRMED | MEDIUM | TEMPORAL_ADDRESS set for 9 compose services + 3 k3s deployments; no app code reads it (Go reads TEMPORAL_HOST_PORT; only temporal-ui uses it legitimately) | docker-compose.yml:37,75,126,195,323,373,409,475,520,554; deploy/k3s/*.yaml:73/65/55; booking config.go:141, notification config.go:91 | "Temporal endpoint is configured per deployment" — ignored env; changing it changes nothing |
| F16-4 | CONFIRMED | MEDIUM | MMS_MOCK / XTTS_MOCK code defaults are "1" (mock ON), contradicting the voices.compose.yml header claim "Both services run REAL inference unless the mock is EXPLICITLY opted into (W39 SIM-013 — mocks default OFF)". Only the compose override ${MMS_MOCK:-0}/${XTTS_MOCK:-0} makes the claim true; a bare container run mocks | services/mms-tts/app/main.py:55, services/xtts-tts/app/main.py:137, infra/compose/voices.compose.yml header | "Real TTS inference by default" — bare image produces deterministic sine waves |
| F16-5 | CONFIRMED | HIGH (config) | PORTAL_SECRET has a repo-public fallback both in code and compose, and k3s omits it entirely; portal JWTs (15-min HS256, sub=contact_id) are then signed with a published key | booking config.go:164; docker-compose.yml:141; deploy/k3s/booking-deployment.yaml (absent); booking httpapi/portal.go:18 | "Portal links prove customer identity" — anyone with the repo can mint any customer's portal JWT on a default-configured deploy |
| F16-6 | CONFIRMED | HIGH (config) | PAYMENTS_INTERNAL_TOKEN is empty in default compose (${PAYMENTS_INTERNAL_TOKEN:-}); payments auth.rs treats empty as gate OFF. The W43 P-09 internal-token gate is therefore not active in the reference deployment; only the apisix uri-blocker stands between the internet and /v1/payouts | docker-compose.yml (payments env); services/payments-service/src/auth.rs:5,57,65; config.rs:134 | "Payouts require internal-token auth" — false in the shipped compose |
| F16-7 | CONFIRMED | LOW | 369/445 env vars (83%) undocumented in any .env.example, incl. required boot-blocking vars (LEDGER_IMPL, BILLING_INTERNAL_TOKEN, MOJALOOP_ENDPOINT) and money tunables (PLATFORM_FEE_BPS=250, PAYOUT_MIN_NGN=100, TB_CLUSTER_ID, TB_ADDRESSES) | §4.1 doc column; .env.example coverage check | "Configure via .env.example" — the file documents a sixth of the real surface |
| F16-8 | CONFIRMED | LOW | ci.yml rust lane `cargo check --locked \|\| cargo check` + continue-on-error structurally cannot fail on lock drift (payments Cargo.lock drift currently in-flight with FIN-P — recorded, not re-reported as new) | .github/workflows/ci.yml rust job; /tmp cargo results §1 | n/a (CI parked anyway; recorded for the unblock day) |
| F16-9 | SUSPECTED | MEDIUM | k3s-deployed booking/notification have no Dapr sidecar but default DAPR_HOST=daprd-*; Dapr invoke/pubsub paths (outbox relay, notifications outbox) would fail at runtime on the appliance. README's "middleware not in this base" covers brokers, not sidecars | booking config.go:131; deploy/k3s/booking-deployment.yaml (no sidecar container) | "k3s appliance runs booking+notification" — event-driven paths likely dead there |
| F16-10 | CONFIRMED (documented residual) | MEDIUM | payments/edge/voice/analytics still run on the superuser `opendesk:opendesk` PG_DSN in compose (x-app-env:34, payments :320, edge :370, voice :406, analytics :551); per-service app_* roles cover the other 8 services (05-app-roles.sql). Compose :26-34 honestly labels this "BOOTSTRAP/MIGRATION TEMPLATE ONLY … Extend 05-app-roles.sql before migrating these" — so this is a KNOWN residual, recorded for the money-map blast radius (payments holds payout_attempts) | docker-compose.yml:26-34,320 | n/a (documented); residual risk: money-adjacent service with superuser DSN |
| F16-11 | CONFIRMED | LOW | BILLING_STATIC_ACCOUNT default `OPENDESK/0123456789` / BILLING_MERCHANT_NAME `OPENDESK DEMO` in compose — a demo bank account is the default settlement target for payment links | docker-compose.yml billing-engine env; billing config.rs:107-114 | Invoices' "pay to" account is a placeholder unless overridden |

## 7. Map coverage summary

| Map | Coverage | Not covered |
|---|---|---|
| Service map | 100% of Dockerfile'd services + apps (22+4) with source-verified ports | per-handler route tables for Go/Python (Rust enumerated); dapr subscription manifests |
| Money map | payments + billing 100% routes/tables/codes; booking tables 100% from bootstrap DDL | per-column money types; admin-web displayed figures (S2); lending/loyalty handler-level trace |
| Trust-boundary map | 100% of env-named egress endpoints + all gateway webhook ingress routes | per-client TLS verification audit; dapr component yamls |
| Gate map | 100% of W43-F3 controls + every *_MOCK/*_REQUIRED/*_ENFORCEMENT var | per-endpoint RBAC matrices (S1 owns top-20 money-mutation table); Permify schema parity |
| Config map | 445/445 env reads (4 extractor patterns, tests excluded) | dynamically-named envs; third-party container envs |

