# SPEC-W28 — Tenant Knowledge Graph for Consent-Gated Outreach

Date: 2026-08-05 · Status: contract · Codebase: github.com/munisp/agora @ main
Depends on: W12 (consent registry, erasure topic, channels), W13 (leads, attribution, lakehouse CAC),
W14 (referral engine), W16 (field PWA), W24 (Kafka consumer patterns, idempotency)

---

## 0. Scope ruling (non-negotiable)

The knowledge graph is **per-tenant**. Each tenant's graph contains only that tenant's
contacts/leads/customers. There is no platform-wide user graph and no cross-tenant edges —
that would violate the RLS architecture, NDPA, and the trust model (§7 of the GTM doc).
Platform-level views exist only as aggregates (counts, CAC) that already live in gold tables.

Outreach from the graph is **consent-gated by construction**: no Person leaves the graph in an
outreach audience without a valid `CONSENTED` edge of the right purpose, and erasure via
`opendesk.consent.erasure.v1` propagates to the graph (node + edges deleted).

## 1. Technology decisions (verdict-driven)

| Component | Decision | Rationale |
|---|---|---|
| **FalkorDB** | ADOPT — graph store | Cypher, low-latency, cheap to operate alongside existing Redis ops knowledge; GraphRAG-friendly. Compose service `graph-db` (FalkorDB official image). |
| **Ollama** | ADOPT — local LLM + embeddings | NL→Cypher, entity-resolution embeddings, message personalization. OpenAI-compatible API plugs into the existing `FallbackLLM/build_llm` seam (voice-agent-runtime). Compose service `ollama` (CPU profile; GPU profile documented). Default chat model `qwen2.5:7b-instruct`, embed model `nomic-embed-text`; env-overridable. |
| **Lakehouse bi-direction** | ADOPT | Gold→graph enrichment (read-side); graph features→Iceberg (Phase-2 seam, implemented as an export job now). |
| **GNN** | SEAM NOW, build Phase 3 | Export job produces node/edge feature tables; prediction write-back path (`Person.propensity_*` properties) defined. No GNN training in this wave. |
| **ART** | SEAM NOW, build Phase 4 | Outreach trajectories already flow through Kafka (notification events → outcomes). Add a `trajectories` sink table in this wave so ART has training data later. No RL training in this wave. |
| **CocoIndex** | SKIP | Redundant: outbox→Kafka→consumer IS the incremental indexing pipeline. A second framework = two truths. Revisit only if CDC needs outgrow consumers. |
| **EPR-KGQA** | SKIP (production) | Research-grade model, unmaintained deps; NL-question-over-graph is delivered by GraphRAG (Ollama NL→Cypher over FalkorDB). May be added later as an eval benchmark only. |
| **Neo4j alongside FalkorDB** | SKIP | One graph store. All graph access goes through Cypher via graph-service, so a Neo4j adapter remains a drop-in if an enterprise tenant mandates it. |

## 2. Architecture

```
Postgres outbox → Kafka (opendesk.booking.events, opendesk.cac.events*,
  opendesk.identity.events, opendesk.conversation.transcripts,
  opendesk.consent.erasure.v1, opendesk.dlq)
        │
        ▼
graph-sync (NEW Go service, follows notification-worker consumer patterns)
  - idempotent upserts (event_id dedupe), tenant_id mandatory on every node
  - consent + erasure handling (see §5)
        │
        ▼
FalkorDB (graph-db)  ◄──── Cypher ────►  graph-service (NEW Python FastAPI :7014)
        ▲                                    │
        │                                    ├─ GET  /v1/graph/segments (+POST /v1/graph/segments/{id}/audience)
        │                                    ├─ POST /v1/graph/ask        (NL→Cypher GraphRAG via Ollama)
        │                                    ├─ GET  /v1/graph/persons/{id}
        │                                    └─ POST /v1/graph/cypher     (template-allowlisted only)
        │                                    │
Ollama (ollama) ◄──── OpenAI-compatible ────┘
        ▲
        │ embeddings (entity resolution in graph-sync; dedupe candidates)
        │
Lakehouse: spark graph_enrichment.py (gold→graph nightly) +
           graph_export.py (graph→Iceberg features, GNN/ART seam) +
           trajectories sink (notification sends × outcomes)
        │
        ▼
admin-web: Segment Builder UI + Graph Explorer + Ask box;
  audiences hand off to notification-worker (consent/DND/quiet-hours gates unchanged)
```

* `opendesk.cac.events` used if present in topics config; otherwise funnel events topic from W13-B.
  Consumer must read topic names from env with the documented defaults — never hardcode.

## 3. Graph schema (v1)

Nodes (all carry `tenant_id: string` mandatory + `updated_at`):

- `Person {person_id, tenant_id, phone_hash, name?, channels[], consent_summary, quarantine: bool, propensity_*?: float}`
- `Contact {lead_id, tenant_id, channel_of_first_touch, captured_at, geo?, source}` (field PWA / web / import)
- `Consent {consent_id, tenant_id, purpose, granted_at, revoked_at?, proof_ref?}`
- `Booking {booking_id, tenant_id, status, offering_id, created_at, showed?}`
- `Offering {offering_id, tenant_id, name, price_cents?}`
- `Location {tenant_id, lga?, ward?, lat?, lon?}`
- `Campaign {campaign_id, tenant_id, kind}` (outreach send / referral program / promo)
- `Tenant {tenant_id, slug}` (single node per tenant; anchor for isolation checks)

Edges:

- `(Person)-[:HAS_CONTACT]->(Contact)`
- `(Person)-[:CONSENTED {purpose, at}]->(Consent)`
- `(Person)-[:REFERRED {at, program}]->(Person)`  (referral tree from W14-A)
- `(Person)-[:BOOKED {at}]->(Booking)`; `(Booking)-[:FOR]->(Offering)`
- `(Contact)-[:CAPTURED_AT]->(Location)`
- `(Person)-[:MESSAGED {campaign_id, at, status}]->(Campaign)`
- `(Contact)-[:PART_OF]->(Tenant)` + every node type carries tenant_id (belt and braces)

Phone numbers are stored **hashed** (same SHA-256+salt scheme as leads dedupe). Raw PII
stays in Postgres; the graph is a relationship + metadata layer. Name (if any) is the only
plaintext PII in the graph and is deleted on erasure.

## 4. Workstreams & file ownership

### WS-A — graph-sync consumer (Go) — owns `services/graph-sync/**`
- Kafka consumer groups `graph-sync` over the topics in §2 (env-configured, documented defaults).
- Idempotent upsert by `event_id` (SET processed marker; skip duplicates) — mirrors W24 patterns.
- Entity resolution: on Person create, compute phone_hash; if embedding-similar candidate
  (Ollama `nomic-embed-text`, cosine ≥ 0.92 over name+channel context) AND same tenant, propose
  merge via `MERGE_CANDIDATE` edge — auto-merge only on exact phone_hash match.
- Erasure: consume `opendesk.consent.erasure.v1` → DETACH DELETE Person subgraph; emit
  `opendesk.graph.erasure.done.v1` audit event.
- DLQ to `opendesk.dlq` on poison messages; metrics + health endpoints (workforce conventions).
- Tests: consumer unit tests with in-memory graph fake; dual-TZ safety; idempotency test.

### WS-B — graph-service (Python FastAPI :7014) — owns `services/graph-service/**`
- FalkorDB client; **every query tenant-scoped**: service extracts tenant from JWT
  (workforce auth seam: JWT sub → X-Tenant-Id), injects `tenant_id` filter; raw Cypher
  endpoint is template-allowlisted (no arbitrary user Cypher).
- `POST /v1/graph/segments`: declarative segment DSL (JSON) → compiled Cypher with mandatory
  consent filter, e.g. `{has_consent: "marketing", last_booking_before: "...", lga: "...", not_messaged_since_days: 30}`.
- `POST /v1/graph/segments/{id}/audience`: materialize consent-passing audience → hand off to
  notification-worker audience API (existing send-path gates unchanged: DND, quiet hours).
- `POST /v1/graph/ask`: NL → Cypher via Ollama (schema-prompted, read-only templates, result
  capped, tenant filter injected) → answer with row table + generated Cypher shown.
- Health/metrics per workforce conventions; pytest suite incl. tenant-isolation tests
  (prove tenant A cannot read tenant B via any endpoint).

### WS-C — admin-web UI + notification-worker audience intake — owns `apps/admin-web/app/app/[orgSlug]/segments/**`, `services/notification-worker/internal/activities/audience*.go` (new files only)
- Segment Builder: form → DSL JSON → preview count (consent-passing) → save → "Send campaign"
  → notification-worker.
- Graph Explorer (v1, lightweight): person 360 view (contacts, bookings, consents, referrals,
  messages) + neighborhood table. No heavy graph-viz lib; server-rendered tables + MapLibre
  reuse for geo.
- Ask box on the segments page → `/v1/graph/ask`.
- notification-worker: `POST /v1/audiences` (segment_id, campaign_id) → fetch materialized
  audience from graph-service → normal pacer send (existing consent/DND/quiet-hour guards).
- Trajectory logging: emit send×outcome rows to `opendesk.usage.events` (ART seam).

### WS-D — infra, lakehouse, docs, e2e — owns `infra/**`, `docs/graph.md`, `tests/e2e/test_graph_wave.py` (new files)
- Compose: `graph-db` (FalkorDB), `ollama` (CPU profile default; GPU profile documented),
  env wiring for graph-sync/graph-service; observability dashboards entries.
- Spark `graph_enrichment.py`: nightly gold→graph (CAC aggregates, LTV per Person where
  computable → node properties). Spark `graph_export.py`: graph→Iceberg
  `graph_node_features` / `graph_edge_features` (GNN seam) + `outreach_trajectories` (ART seam).
- docs/graph.md: schema, query cook-book, consent/erasure model, ops (backup = FalkorDB RDB
  snapshot), model pulls (`ollama pull` bootstrap script).
- e2e: capture lead (field path) → appears in graph → consent → segment → audience →
  (mock) send; erasure removes person; tenant isolation negative test.

## 5. Compliance gates (binary, tested)

1. Every node/edge carries `tenant_id`; graph-service injects tenant filter on ALL paths
   (test: cross-tenant read attempts return empty/404).
2. Audience materialization excludes Persons without purpose-matching `CONSENTED` edge
   (test: no-consent person never appears in any audience).
3. Erasure event → Person subgraph gone from FalkorDB within consumer SLA (e2e test).
4. Quarantined (imported, consent-unverified) Persons are query-visible but
   audience-ineligible (test).
5. No raw Cypher from clients; template allowlist only (test: injection attempts rejected).

## 6. Explicitly NOT in this wave

GNN training/inference (Phase 3 — seams delivered), ART training (Phase 4 — trajectory sink
delivered), EPR-KGQA, CocoIndex, Neo4j deployment, cross-tenant analytics beyond existing
aggregate gold tables.

## 7. Acceptance gate

- go build/vet/test green (graph-sync); pytest green (graph-service); admin-web typecheck+build green.
- e2e suite passes incl. the 5 compliance gates.
- Full-tree push protocol: batched push_files + blob-SHA verification sweep (W26 protocol).
