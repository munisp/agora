# SPEC-W34 — Hardening wave from the adapted external prompt battery

Origin: W34 probe battery (8 themes, T1–T8) executed 2026-08-07. This wave fixes the
verified FAIL/CRITICAL/HIGH findings and the feasible MEDIUMs. Everything not fixed
here is explicitly deferred in the W34 execution report (no silent drops).

## Fixes and acceptance gates

- **GF1 registry internal-access GUC → role** (T2#1, T8-N1 CRITICAL). In
  `infra/postgres/init-scripts/30-model-registry.sql`: drop every
  `app.registry_internal` reference from policies; add NOLOGIN role
  `app_model_registry_internal` and LOGIN role `app_model_registry_batch`
  (member of the former; dev password documented); policies gate internal access
  via `pg_has_role(current_user, 'app_model_registry_internal')`.
  `model_registry/store.py`: internal transactions connect with
  `MODEL_REGISTRY_INTERNAL_DSN` (the batch role); the plain DSN role can no
  longer flip anything. GATE: adversarial re-probe — as
  `app_model_registry_login`, `set_config('app.registry_internal','on',false)`
  followed by cross-tenant SELECT/UPDATE/DELETE all return 0 rows; batch role
  sees all tenants; shipped suite green (updated).

- **GF2 drift manifest contract** (T5-P3 HIGH). `training_snapshot.py` emits the
  exact contract `drift.py` consumes: top-level `schema:
  "opendesk/training-manifest/v1"`, `features.<name>.histogram.{edges,counts}`
  (computed from the snapshot data), `score_baseline`, `manifest_hash`; legacy
  keys kept for backcompat. A `--registry-sync DIR` mode writes
  `<DIR>/<registry-family>.json` using an explicit mapping
  (fraud_features→fraud-ml, credit_features→credit-ml, gnn_export→graphsage).
  GATE: generated manifest → `DirectoryManifestProvider` → DriftJob on embedded
  PG performs >0 feature PSI checks; shifted window alerts; snapshot tests green.

- **GF3 PII redaction before transcript publish** (T5-P4 HIGH).
  conversation-service redacts phone/email patterns (mirroring
  `infra/fluvio/pii-redact` semantics) before the Dapr publish to
  `opendesk.conversation.transcripts`; event carries `redacted: true`. GATE:
  pytest proves raw phone/email never reaches the published payload.

- **GF4 gateway header spoofing + port exposure** (T1-P2 CRITICAL/HIGH).
  `infra/apisix/apisix.yaml`: strip client-supplied `x-user-*`/`x-tenant-*`
  headers before proxying (serverless-pre-function on a global rule).
  Compose: unpublish all backend service host ports (inter-service traffic uses
  the docker network); keep only the gateway data port and Keycloak (dev login),
  remove published APISIX admin 9180. GATE: `docker compose config` parses; grep
  proof no backend `ports:` remain; header-strip rule present on all routes.

- **GF5 Keycloak hardening** (T1-P3 HIGH). realm: `sslRequired=external`,
  `bruteForceProtected=true`, `revokeRefreshToken=true` +
  `refreshTokenMaxReuse`, seeded `admin/admin123` user removed; committed client
  secret replaced with a clearly-marked dev placeholder documented in
  infra/keycloak/README; compose bootstrap admin password env-driven.

- **GF6 billing RLS** (T2#2 HIGH). New migration `0002_rls.sql`: ENABLE+FORCE
  RLS + `app.tenant_id` policies on invoices, rate_cards, usage_records,
  processed_events, plan_presets. billing-engine sets `app.tenant_id`
  transaction-locally from the (now gateway-stripped) tenant context. GATE:
  pgserver-backed test — cross-tenant SELECT 0 rows, GUC-unset 0 rows,
  FORCE-vs-owner holds; `cargo test` green.

- **GF7 kyc_audit immutability** (T4#2 HIGH). Append-only: REVOKE UPDATE/DELETE
  from the app role + BEFORE UPDATE/DELETE trigger raising. GATE: pgserver test
  proves UPDATE and DELETE both fail; kyc Go suite green.

- **GF8 KYC_MOCK default** (T4#1 HIGH). Default flips to false; startup logs a
  loud CRITICAL warning when mock is enabled. GATE: Go suite green; config test
  asserts the safe default.

- **GF9 fraud non-finite evasion** (T8-N2 MED). `build_feature_vector` never
  emits NaN/Inf (clamp/sanitize); `score_vector` guards `math.isfinite` → rule
  fallback; published alert JSON is strict-JSON safe (no bare NaN). GATE:
  adversarial pytest (inf/NaN amounts, NaN vectors) — no crash, no NaN score,
  no suppressed severity upgrade path, valid JSON.

- **GF10 registry input hardening** (T8-N7 LOW). pydantic: UUID tenant/
  experiment ids, `version ge=1 le 2**31-1`, `predicted_score ge=0 le=1
  allow_inf_nan=False`, artifact_uri length cap. GATE: prior 500-cases now 422;
  suite green.

- **GF11 payments money path** (T6#1/#2/#4 HIGH/MED). consumer.rs: commit Kafka
  offset ONLY on success; failure → publish to `opendesk.dlq` + error metric.
  tigerbeetle.rs: `submit()` treats TB `exists` as idempotent success; capture
  issued as ONE atomic batch. config.rs: `LEDGER_IMPL` must be explicit in
  non-dev profiles (fail-closed, no silent in-memory sim). GATE: cargo test
  green incl. a failure-injection test on the sim ledger; tb-live-only code
  verified by inspection (cannot build offline — documented).

- **GF12 k3s zero-trust baseline** (T3-P4/P5 HIGH). deploy/k3s: default-deny
  NetworkPolicy + explicit allows (DNS, gateway ingress); `securityContext`
  (runAsNonRoot, readOnlyRootFilesystem, drop ALL caps) on every deployment;
  voice-worker probes; livekit liveness probe; plaintext DB password →
  secretKeyRef + dev Secret manifest; `AUTHZ_DISABLED` removed. GATE:
  `kustomize build` passes; every Deployment has probes+securityContext.

- **GF13 observability truth** (T5-P2 HIGH). New
  `infra/observability/prometheus/rules.yml` (service-down `up==0`,
  analytics consumer lag, drift `opendesk_model_drift_psi > 0.25`, flush error
  rate) wired via `rule_files`; model-registry scrape job added. Dead dashboard
  panels repointed to metrics services actually emit (or removed with a note).
  GATE: every panel expr's metric exists in code (grep-verified) or the panel is
  gone; all YAML/JSON parse.

- **GF14 billing QR test vector** (T6#5 LOW). Fix
  `payments_qr.rs` expected-vector construction; `cargo test` green.

- **GF15 Permify per-tenant schema** (T2#3 HIGH). identity-service writes the
  schema to the per-tenant Permify tenant inside `CreateTenant` (schema embedded
  in the service). GATE: Go test with a mock Permify server asserts
  `schemas/write` is called with the schema on tenant creation; suite green.

- **GF16 saga compensation exhaustion** (T6#3 MED). notification-worker:
  compensation that exhausts retries emits a CRITICAL alert event (ops.alerts
  pattern / structured log + metric) instead of silently marking compensated.
  GATE: Go test proves the alert fires on permanent compensation failure.

## Deferred (documented in the W34 report, not silent): chaos harness,
GitOps, frontend test infra, npm lockfile registry regeneration, self-hosted
basemap tiles, TLS termination certs, booking-service JWKS signature
verification (defense-in-depth; practical bypass closed by GF4), multi-region
replication/quorum fencing (ADR-0008 design only), lending live-rail endpoint
mismatch (default-off MockRail), e2e suites requiring a live stack.

## Push protocol (unchanged): coders own disjoint file sets, work in $HOME,
rsync additively to /mnt, md5 double-read; orchestrator pushes in ≤15-file
≤40KB text-only batches via GitHub MCP with per-file blob-SHA verification;
full-tree audit closes the wave.
