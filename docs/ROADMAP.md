# Agora — Gaps, Improvements & Innovation Roadmap

Assessment of the current codebase (Phase 1 + Phase 2). Items are grounded in the actual tree, not generic advice. Priority: **P0** = do before any real deployment, **P1** = production hardening, **P2** = differentiation.

> **Status (Wave 3):** items marked ✅ are implemented per SPEC-W3; notes flag MVP/scaffold scope honestly. Unmarked items remain open.

---

## 1. Known gaps (fix first)

| # | Gap | Evidence in tree | Fix |
|---|---|---|---|
| G1 | **Never runtime-verified** — no `docker compose up` has ever run | built in an environment without Docker | P0: full bring-up cycle; expect image-tag (e.g. `twentycrm/twenty:v1.3.2`) and healthcheck fixes (Twenty healthchecks use `wget`/`pgrep`, absent in node:slim images) |
| G2 ✅ | **Rust services never compiled** | no toolchain was available; `tb-live`/`fluvio-live` feature-gated code paths are unverifiable | ~~P0~~ **done:** `cargo check` runs in CI (`.github/workflows/ci.yml`, applied by maintainer directive in W41 — it was parked local-only in W39 over a token-scope gap); W41 adds proptest property suites for payments-service (ledger invariants) and billing-engine (paystack HMAC) — results: `tests/fuzz/RESULTS.md`. Runtime paths still need a live TigerBeetle/Fluvio to fully verify |
| G3 ✅ | **No CI/CD** | no `.github/workflows` | ~~P0~~ **done:** `.github/workflows/ci.yml` — yaml lint, go build/vet/test, cargo check, py_compile/pytest, web tsc+build, `docker compose config`, smoke shellcheck. **W41:** workflow applied by maintainer directive (parked local-only in W39; push/rejection ledger: engagement-workspace `plan.md` §W41 — that ledger lives outside the repo tree); `workflow_dispatch` trigger added (the `e2e` job was dead code); analytics-pipeline pytest lane fixed to install `-e '.[dev]'`; new **blocking** `funds-e2e` lane (real Postgres via pgserver + real Go/Rust service binaries) — executed evidence: `tests/funds-e2e/RESULTS.md`; booking-service `-race` evidence: `tests/race/RESULTS-booking.md`; backup/restore drill evidence: `tests/restore-drill/RESULTS.md`; perf baseline: `tests/perf/RESULTS.md` |
| G4 ✅ | **No E2E tests** | only unit/contract tests; `scripts/smoke-test.sh` is manual | ~~P1~~ **done:** `tests/e2e/` pytest suite (compose fixtures) covering provision → seed → book → saga → CRM sync → lakehouse |
| G5 ✅ | **Observability gap** — prometheus plugin enabled on APISIX but no Prometheus/Grafana/Tempo/Loki in compose; OTel plugin points nowhere | `infra/apisix/apisix.yaml` global rules | ~~P1~~ **done:** `infra/docker-compose.observability.yml` — Prometheus, Grafana :3002 (provisioned dashboards), OTel collector, Loki + promtail |
| G6 ✅ | **AuthZ enforcement incomplete** — booking store never sets `app.tenant_id` GUC, so RLS is bypassed in dev (superuser connection) | `services/booking-service/internal/store` | ~~P1~~ **done:** `SET LOCAL app.tenant_id` per tx + per-service DB roles (`app_booking` etc., NOINHERIT + FORCE RLS) |
| G7 ✅ | **Secrets in plaintext dev defaults** | compose `*-dev` fallbacks, Keycloak realm import with hardcoded secret | ~~P1~~ **done (dev pattern):** root `.env.example`, compose reads `${}`; prod SOPS/Vault pattern + rotation in `docs/runbooks/secrets.md` |
| G8 | **Voice session state in-memory** — chat history and phone-confirmation state lost on restart | `services/voice-agent-runtime/app/session_state.py` | P1: Dapr Redis state store (component already exists, unused by app code) |
| G9 (partial ✅) | **Dead/unused infrastructure** — `opendesk.notifications.outbox` topic, Fluvio smart module never deployed, `apisix-etcd` deployed-but-unused, Dapr Redis statestore unused | kafka topics, `infra/fluvio/pii-redact`, edge compose | **partial:** Fluvio smart module now has a deploy path (`infra/fluvio/deploy.sh` + connector yaml); Redis statestore / outbox topic still unwired |
| G10 ✅ | **Single-replica everything** — no HA story: 1 Kafka broker (rf=1), 1 Postgres, 1 Temporal node, sim ledger by default | compose files | ~~P1~~ **done (docs):** production topology ADR `docs/ADRs/0008-production-topology.md` (Kafka rf=3, Patroni, TB 3-node, TB 3-node, Temporal multi-node, sizing); dev compose stays single-replica |
| G11 ✅ | **No backup/restore automation** | runbook has procedures, nothing scheduled | ~~P1~~ **done:** `infra/backups/backup.sh` + `restore.sh` with rotation, cron/ofelia sidecar in the observability profile |
| G12 | **ElevenLabs adapter + reverse CRM sync are stubs/thin** | `elevenlabs_adapter.py` untested; `/webhooks/twenty` only logs + emits event, nothing consumes `opendesk.crm.events` | P2: complete or trim |

## 2. Improvements & enhancements (by area)

### Reliability
- I1 — **Outbox relay as a dedicated service** (booking/payments each run in-process dispatchers; a shared Debezium-style or Go relay with lag metrics + alerting is more operable).
- I2 — **Kafka consumer lag alerting** on all 7 consumer groups (expose in /metrics, alert in Grafana) — analytics already computes lag, generalize it.
- I3 — **Temporal workflow versioning discipline** (`workflow.GetVersion`) before packs evolve; add workflow replay tests to CI.
- I4 — **Idempotency audit** — bookings and payments have it; conversation turns rely on advisory locks only; add idempotency keys to `/v1/conversations/{id}/turns`.
- I5 — **Graceful degradation matrix** — document + test behavior when each middleware is down (e.g. Permify down → 503 vs fail-open `AUTHZ_DISABLED`; currently inconsistent).

### Security
- I6 ✅ — **APISIX jwt-auth → OIDC bearer_only everywhere** — `/voice/*` split: public `/voice/chat` + `/voice/session` (anonymous visitors) get strict limit-count (30/min); everything else is bearer_only. `/ws` keeps in-app JWT (documented rationale).
- I7 ✅ — **OpenAppSec detect→prevent** runbook section (learning period → enforce switch) + API-discovery schema upload from `docs/api/openapi/`.
- I8 ✅ — **Rate-limit tiers per plan** — limit-count keyed on the `X-Tenant-Plan` header with a documented plan map.
- I9 — **PII vault** — phone numbers currently live in PG, transcripts, CRM, lakehouse; introduce tokenization or at minimum column-level encryption + lakehouse redaction parity with the Fluvio smart module.
- I10 — **Pen-test harness** — OWASP ZAP baseline scan script against the gateway in CI.

### Performance
- I11 — **Availability engine caching** — slot computation is per-request; cache by (offering, member, day) in Redis with event-driven invalidation on booking events.
- I12 — **OpenSearch circuit breaker + bulk sizing** tuning in knowledge/conversation indexers; index lifecycle already exists (ISM), add rollover for `conversations`.
- I13 — **LLM streaming for the text chat path** (`/voice/chat` is request/response; stream tokens via SSE — big perceived-latency win).
- I14 — **dbt incremental models** for gold marts (currently full-refresh views); add Trino query audit.

### Developer experience
- I15 — **Tilt/Skaffold or compose watch** for hot-reload dev; per-service `make dev-<svc>` targets.
- I16 — **Contract tests** (OpenAPI → schemathesis against services; CloudEvents schemas in a registry — Karapace or JSON Schema files + CI validation on both producer and consumer sides).
- I17 — **Seed parity** — one `make demo` that seeds all 4 industries + knowledge + a published site each + simulated bookings, so the lakehouse/dbt path has data on day one.

### Product completeness (baseline parity gaps)
- I18 ✅ — **Public page theming/builder** — theme editor on the Public Site page (primary colour, logo URL, hero title/subtitle, template select → `theme` jsonb via `PUT /v1/site`) with a live `/p/{slug}` preview iframe.
- I19 ✅ — **Team self-service** — staff "My Schedule" page backed by `GET /v1/bookings?mine=true` (JWT email → team_members lookup). My-availability editing stays admin-driven for now.
- I20 ✅ — **Embeddable widget** — chromeless `/embed/{siteSlug}` booking+chat page, `public/embed.js` iframe loader, `docs/embedding.md`, copy-paste snippet on the Public Site page.

---

## 3. Fifteen innovations (SOTA differentiation)

1. ✅ **Agentic escalation with warm handoff** — implemented: voice runtime `request_human` tool opens a LiveKit `escalation-{conv}` room, publishes `EscalationRequested` (fanned out on `/ws`), mints a staff join token; the dashboard shows a toast with a join link (`/app/{org}/call`). Whisper-copilot posts suggested replies into the room data channel (graceful when LiveKit is absent).
2. ✅ (scaffold) **Voice biometrics for returning callers** — **scaffold only**: enrollment/verify interface in `voice-agent-runtime/app/voiceprint.py` with optional resemblyzer import, consent-gated by `VOICEPRINTS=off` (default). No production verification flow yet.
3. ✅ **Real-time call intelligence sidebar** — per-turn enrichment in conversation-service (lexicon sentiment + optional LLM NER, `INTEL_LLM=off` by default) published to `opendesk.conversation.enriched` and fanned out on `/ws/intel`. Dashboard sidebar UI is the remaining piece.
4. ✅ **Self-improving knowledge loop** — low-RRF questions auto-create `kb_suggestions`; dashboard Knowledge → Review queue approves (creates a real embedded document) or rejects.
5. ✅ **Containment & quality eval harness** — `services/voice-agent-runtime/eval/` replays conversations/scenarios against `/voice/chat` with LLM-as-judge scoring; `make eval`.
6. ✅ (MVP) **Multi-agent specialist crews per industry** — packs gain optional `agents:` sub-personas with intent routing in the voice runtime (prompt-swap per turn). **MVP scope:** Temporal child-workflow orchestration and Permify tool-scoping are not included.
7. ✅ (MVP) **Proactive outbound engine** — waitlist table + `WaitlistBackfillWorkflow`: on cancellation the top 3 waitlisted contacts are notified (email/SMS) with a claim token; first claim wins the slot. **MVP scope:** outbound LiveKit SIP calls and churn-driven re-engagement campaigns are not included.
8. ✅ **Conversational analytics ("talk to your data")** — knowledge-service `POST /v1/analytics/query` (guarded text-to-SQL over Trino gold marts: single SELECT, allowlist, tenant filter injected, LIMIT enforced) + dashboard Analytics page with result table and SQL audit.
9. ✅ **Revenue intelligence** — Spark job `revenue_intelligence.py` writes `gold.reco_pricing`; analytics-pipeline `GET /v1/recommendations` serves it; Billing page shows suggestions with a human-approval-only disclaimer (never auto-applied).
10. ✅ **Edge/telephony ingestion via Fluvio** — `infra/fluvio/deploy.sh` builds/loads the `pii-redact` smart module and applies it via a connector (`transcripts-raw` → Kafka sink). Honest note: requires a reachable Fluvio cluster; not exercised in default dev bring-up.
11. **Mojaloop-powered cross-border payouts marketplace** — multi-region tenant payouts with real FSPIOP quote comparison across DFSPs, fee transparency in the billing page, and ledger reconciliation reports. *(open — not in Wave 3 scope)*
12. ✅ **Digital-twin sandbox per tenant** — identity `POST /internal/tenants/{slug}/twin` spins a `{slug}-twin-{rand}` shadow tenant with synthetic seed (`plan='twin'`); Temporal `TwinCleanupWorkflow` deletes it after 24h.
13. ✅ **Zero-knowledge compliance exports** — Temporal `GdprExportWorkflow`/`GdprEraseWorkflow`; export bundles all stores into MinIO `exports`; erasure publishes tombstones to `opendesk.privacy.events` with consumers anonymizing/deleting across booking, conversation, knowledge and CRM. Endpoints: `POST /v1/privacy/export|erase`.
14. ✅ (MVP) **Offline-first edge appliance mode** — **k3s MVP**: `deploy/k3s/` kustomize base + kind config + MirrorMaker2 store-forward yaml. Single-node only; no automated cloud sync testing yet.
15. ✅ (MVP) **Plugin SDK for custom tools** — **plugins MVP**: pack-declared `customTools:` (name/description/method/url/bodyTemplate) executed via httpx in the voice runtime tool layer. WASM sandbox isolation is documented as phase 2 in `docs/plugins.md`.

---

## Suggested sequencing

| Wave | Contents | Theme |
|---|---|---|
| Wave 1 (2–3 wks) | G1–G4, G6, I13, I17 | Prove it runs; CI + E2E + streaming chat |
| Wave 2 (3–4 wks) | G5, G7, G8, I1–I3, I6–I8, G10 topology docs | Production hardening |
| Wave 3 (4–6 wks) | I18–I20, Innovations 4, 5, 8 | Product parity + analytics activation |
| Wave 4 (ongoing) | Innovations 1–3, 7, 9–15 | SOTA differentiation |

**Progress:** Waves 1–3 are landed (CI, E2E, observability, RLS, secrets, streaming chat, theme editor, staff schedule, embed, innovations 4/5/8) except G1 (full runtime bring-up still unverified), G8 (Dapr statestore session persistence), I17 (seed parity). Wave 4 items 1–3, 7, 9, 10, 12–15 are implemented at the scope noted above; innovation 11 (Mojaloop payouts marketplace) remains open.
