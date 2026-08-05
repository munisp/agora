# ADR-0007: Edge gateway & ledger simplifications (dev stack)

Status: Accepted
Date: 2025-07-21
Scope: APISIX deployment mode, OpenAppSec attachment pattern, TigerBeetle client
strategy in payments-service, Iceberg REST catalog image choice.

---

## Context

SPEC §12/§9/§13 mandate an APISIX + OpenAppSec edge, a TigerBeetle ledger behind a Rust
payments-service, and an Iceberg-based lakehouse. Three of these integrations have
version-sensitive or immature glue that cannot be made fully "production grade" inside a
runnable dev compose stack. This ADR records exactly what was built, what was simplified,
and what changes in prod — no silent placeholders.

---

## Decision 1 — APISIX deployment mode: standalone file-driven (etcd kept, unused)

`infra/apisix/config.yaml` sets `deployment.role: data_plane` with
`role_data_plane.config_provider: yaml`. All routes/upstreams/consumers live in
`infra/apisix/apisix.yaml`, hot-reloaded by APISIX ~1s after save.

- **Why not etcd-backed Admin API?** Declarative config reviewed in Git beats imperative
  Admin-API calls for a dev stack: the whole gateway state is reproducible from the repo,
  and `docker compose config`-style validation is possible offline. SPEC §12 itself
  flags the tension ("standalone etcd-less? use etcd").
- **etcd still ships** (`apisix-etcd`, bitnami/etcd:3.5) because SPEC §3 reserves the
  container; APISIX does not read from it. Migration path to traditional/decoupled mode
  is a config-only change (switch `role`/`config_provider`, seed via Admin API).
- The Admin API on 9180 stays enabled in read-mostly fashion (inspect loaded config);
  keys in `config.yaml` are dev-only.

## Decision 2 — OpenAppSec attachment: image-level IPC attachment, not an APISIX plugin

There is no `open-appsec` plugin in stock `apache/apisix:3.9.x`, and the "one-line
plugin" integration does not exist. The **officially documented** pattern
(docs.openappsec.io → "APISIX on Docker") is:

1. a nano **agent** container (`ghcr.io/openappsec/agent`, compose service `openappsec`)
   started with `ipc: shareable`, policy from `/ext/appsec/local_policy.yaml`
   (`infra/openappsec/local_policy.yaml` — real v1beta1 policy: web-attack practice,
   API-discovery/schema-validation practice, `detect-learn` mode in dev, JSON stdout
   logging, 403 custom response); and
2. a gateway container built from **`ghcr.io/openappsec/apisix-attachment`** (APISIX with
   the open-appsec attachment compiled in) sharing the agent's IPC namespace
   (`ipc: service:openappsec`). Traffic inspection happens over shared memory — no Lua
   plugin block, no unix socket, no `ext-plugin` runner in the route config.

We ship exactly that as compose service **`apisix-appsec` (profile `appsec`)**, mounting
the same `apisix.yaml` routes. The default `apisix` service is stock
`apache/apisix:3.9.0-debian` so the dev gateway never depends on a pinned `latest` WAF
image; `docker compose --profile appsec up` swaps in the WAF-enforced gateway on the
same ports.

**Limits, honestly:**

- Stock `apache/apisix` (default dev service) runs **without inline WAF**. Detection
  coverage exists only under the `appsec` profile. This is deliberate: dev prioritizes
  deterministic startup over inspect-everything.
- `ext-plugin-pre-req` is enabled in the plugin list as the documented *alternative*
  hook for a future external WAF runner (e.g. a Go/Python plugin-runner implementing
  open-appsec-style inspection), but **no runner is configured** — wiring it without a
  runner would 503 every request, so it is intentionally unbound from all routes.
- `ghcr.io/openappsec/apisix-attachment:latest` is a floating tag and its APISIX base
  version is not pinned to 3.9.x; prod must build a version-pinned image (APISIX 3.9 +
  attachment from source) and switch policy mode from `detect-learn` to `prevent-learn`
  per asset after the ML baseline is learned.
- Agent runs unmanaged (no `AGENT_TOKEN`, no my.openappsec.io SaaS); logs go to stdout
  and `/var/log/nano_agent` volume only.

## Decision 3 — TigerBeetle client: `LedgerClient` trait with `sim` fallback (SPEC §9)

payments-service (Rust/axum) must post deposits/holds/captures/refunds/no-show
fees/payouts to TigerBeetle (cluster 0, replica 3000, transfer codes 100–104). There is
no maintained official Rust client crate for TigerBeetle's binary VSR protocol, and
writing one is out of scope. Therefore:

- The service defines a **`LedgerClient` trait** with the TigerBeetle-shaped API
  (create accounts, pending/post/void transfers by code, idempotent lookup by transfer
  id).
- Two implementations, selected by env **`LEDGER_IMPL`**:
  - `tigerbeetle` — `TigerBeetleClient` over the official `tigerbeetle` crate **if it
    compiles for the pinned toolchain**; the crate tracks TigerBeetle releases and has
    historically been published behind version-locked build scripts, so this arm may be
    cfg-gated out when unavailable;
  - `sim` (default in dev) — `HttpSimClient`/in-process ledger: an embedded, durable
    (append-log) implementation of the same two-phase transfer semantics (pending →
    post/void), enforcing account invariants (no negative posted balances, idempotent
    ids, code-based routing per SPEC §9).
- **Consequences:** the integration contract (accounts `tenant:{id}:deposits|revenue`,
  platform fee account, transfer codes 100–104, idempotency) is exercised end-to-end in
  dev and CI without the Rust client risk. Prod sets `LEDGER_IMPL=tigerbeetle`; the sim
  must never be reachable when `LEDGER_IMPL` is unset in prod builds (fail-closed
  startup check).

## Decision 4 — Iceberg REST catalog: `apache/iceberg-rest-fixture` for dev only

The lakehouse compose uses `apache/iceberg-rest-fixture:latest` — the REST catalog
server the Iceberg project itself uses for integration tests. It is configured with the
**JDBC catalog** (Postgres DB `iceberg`, created idempotently by the
`iceberg-catalog-init` job) and `S3FileIO` against MinIO (`s3://lake/warehouse`), so the
metadata layout and catalog behavior are the *real* JDBC-catalog code paths, not a mock.

- **Why:** it is the only maintained, zero-build image that speaks the Iceberg REST
  OpenAPI spec; tabulario/iceberg-rest is unmaintained and pinned to an old spec.
- **Prod changes:** replace with a supported catalog — Nessie, Polaris, Gravitino, or
  the REST server built from the Iceberg release with pinned version + HA behind a load
  balancer; real Postgres (HA) for the JDBC backend; S3 with versioning + bucket
  policies instead of MinIO; SigV4/IRSA credentials instead of static keys; enable
  catalog auth (`iceberg.rest-catalog.security=OAUTH2` on the Trino side).

---

## Consequences (summary)

- Dev stack boots deterministically from Git-managed declarative config; no runtime
  coupling to floating external services except the explicitly profiled WAF image.
- All simplifications are env/profile switches with documented prod paths; no
  `TODO`-style dead ends in route, policy, or catalog config.
- SPEC contracts (ports 9080/9180/9200/5601/9000/9001/8181/8088/7077/8081, bucket
  `lake`, index names, route table §12) are unchanged by these decisions.
