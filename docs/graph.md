# Tenant knowledge graph — FalkorDB + Ollama GraphRAG (SPEC-W28, WS-D)

Per-tenant knowledge graph for **consent-gated outreach**. Each tenant's graph
contains only that tenant's contacts/leads/customers — there is no
platform-wide user graph and no cross-tenant edges (SPEC-W28 §0 scope ruling:
RLS architecture, NDPA, trust model). Outreach is consent-gated *by
construction*: no Person leaves the graph in an audience without a
purpose-matching `CONSENTED` edge, and erasure propagates to the graph.

| Component | Decision | Compose service |
|---|---|---|
| Graph store | FalkorDB (Cypher, Redis protocol) | `graph-db` |
| Local LLM + embeddings | Ollama (`qwen2.5:7b-instruct`, `nomic-embed-text`) | `ollama-graph` (`ollama-graph-gpu` profile) |
| Event sync | graph-sync (Go consumer, WS-A) | `graph-sync` |
| Cypher/segment API | graph-service (FastAPI :7014, WS-B) | `graph-service` |
| Lakehouse bi-direction | Spark `graph_enrichment.py` + `graph_export.py` | spark-master jobs |

## 1. Architecture

```
Postgres outbox -> Kafka (opendesk.booking.events, opendesk.cac.events,
  opendesk.identity.events, opendesk.conversation.transcripts,
  opendesk.consent.erasure.v1, opendesk.graph.enrichment.v1, opendesk.dlq)
        |
        v
+-----------------+        Cypher (Redis protocol)        +-------------------+
|   graph-sync    | ------------------------------------> |  graph-db         |
|   (Go consumer) |    idempotent upserts, event_id       |  (FalkorDB)       |
|   :7015 health/ |    dedupe, tenant_id on every node    |  graph:           |
|    metrics      | <------------------------------------ |  agora_tenants    |
+--------+--------+     enrichment properties (nightly)   +---------+---------+
         ^                                                          ^
         | consumes opendesk.graph.enrichment.v1                    | Cypher
         |                                                          |
+------------------+        +-------------------+                   |
|  spark nightly   |        |  ollama-graph     |  OpenAI-compat    |
|  graph_enrichment|        |  qwen2.5:7b-inst. |  chat + embed     |
|  (gold -> graph) |        |  nomic-embed-text |                   |
+------------------+        +---------+---------+                   |
                                     ^ embeddings                   |
                                     | (entity resolution)          |
                            +--------+---------+                    |
                            |  graph-service   | -------------------+
                            |  FastAPI :7014   |
                            |  /v1/graph/segments (+/{id}/audience)
                            |  /v1/graph/ask  (NL->Cypher GraphRAG)
                            |  /v1/graph/persons/{id}
                            |  /v1/graph/cypher (template-allowlisted)
                            +--------+---------+
                                     | audience hand-off
                                     v
                            notification-worker (consent/DND/
                            quiet-hours gates unchanged)

Lakehouse write-side: spark graph_export.py
  graph node/edge exports -> iceberg.gold.graph_node_features /
  iceberg.gold.graph_edge_features (GNN seam, Phase 3)
  usage send x outcome     -> iceberg.gold.outreach_trajectories
                              (ART seam, Phase 4)
```

## 2. Graph schema (v1)

All nodes carry `tenant_id: string` (mandatory — compliance gate 1) and
`updated_at`. Phone numbers are stored **hashed** (SHA-256 + `PHONE_HASH_SALT`,
the same scheme as leads dedupe). Raw PII stays in Postgres; the graph is a
relationship + metadata layer. `name` (if any) is the only plaintext PII in
the graph and is deleted on erasure.

Nodes:

- `Person {person_id, tenant_id, phone_hash, name?, channels[], consent_summary, quarantine: bool, propensity_*?: float}`
- `Contact {lead_id, tenant_id, channel_of_first_touch, captured_at, geo?, source}`
- `Consent {consent_id, tenant_id, purpose, granted_at, revoked_at?, proof_ref?}`
- `Booking {booking_id, tenant_id, status, offering_id, created_at, showed?}`
- `Offering {offering_id, tenant_id, name, price_cents?}`
- `Location {tenant_id, lga?, ward?, lat?, lon?}`
- `Campaign {campaign_id, tenant_id, kind}`
- `Tenant {tenant_id, slug}` — single node per tenant; anchor for isolation checks

Edges:

- `(Person)-[:HAS_CONTACT]->(Contact)`
- `(Person)-[:CONSENTED {purpose, at}]->(Consent)`
- `(Person)-[:REFERRED {at, program}]->(Person)` — referral tree (W14)
- `(Person)-[:BOOKED {at}]->(Booking)`; `(Booking)-[:FOR]->(Offering)`
- `(Contact)-[:CAPTURED_AT]->(Location)`
- `(Person)-[:MESSAGED {campaign_id, at, status}]->(Campaign)`
- `(Contact)-[:PART_OF]->(Tenant)` + every node carries tenant_id (belt and braces)
- `(Person)-[:MERGE_CANDIDATE]->(Person)` — entity-resolution proposal
  (embedding cosine >= 0.92, same tenant; auto-merge only on exact phone_hash)

## 3. Query cookbook

All examples run through graph-service (tenant filter injected from the auth
context) or directly against FalkorDB for ops:

```
docker exec opendesk-graph-db redis-cli GRAPH.QUERY agora_tenants "<cypher>"
```

### 3.1 Person 360 — contacts, bookings, consents for one person

```cypher
MATCH (p:Person {tenant_id: $tenant_id, person_id: $person_id})
OPTIONAL MATCH (p)-[:HAS_CONTACT]->(c:Contact)
OPTIONAL MATCH (p)-[:BOOKED]->(b:Booking)-[:FOR]->(o:Offering)
OPTIONAL MATCH (p)-[r:CONSENTED]->(co:Consent)
RETURN p, collect(DISTINCT c) AS contacts,
       collect(DISTINCT {booking: b, offering: o}) AS bookings,
       collect(DISTINCT {purpose: r.purpose, at: r.at, consent: co}) AS consents
```

### 3.2 Consent-gated audience (THE audience shape — gate 2)

Every audience materialization is this pattern; the `CONSENTED` edge match is
mandatory, never optional, and quarantined Persons are excluded (gate 4):

```cypher
MATCH (p:Person {tenant_id: $tenant_id})-[:CONSENTED {purpose: 'marketing'}]->(c:Consent)
WHERE p.quarantine = false
  AND (c.revoked_at IS NULL)
  AND NOT (p)-[:MESSAGED {campaign_id: $campaign_id}]->(:Campaign)
RETURN p.person_id, p.phone_hash, p.channels
LIMIT $cap
```

### 3.3 Segment: lapsed bookers in one LGA, not messaged in 30 days

```cypher
MATCH (p:Person {tenant_id: $tenant_id})-[:CONSENTED {purpose: 'marketing'}]->(:Consent)
MATCH (p)-[:HAS_CONTACT]->(ct:Contact)-[:CAPTURED_AT]->(l:Location {lga: $lga})
MATCH (p)-[b:BOOKED]->(bk:Booking)
WHERE p.quarantine = false
  AND b.at < date() - duration('P90D')
  AND NOT (p)-[m:MESSAGED]->(:Campaign)
    WHERE m.at > datetime() - duration('P30D')
RETURN count(p) AS audience_size
```

### 3.4 Referral tree depth for a program (W14)

```cypher
MATCH path = (root:Person {tenant_id: $tenant_id, person_id: $root_id})
      -[:REFERRED {program: $program}*1..5]->(down:Person)
RETURN down.person_id, length(path) AS depth
ORDER BY depth, down.person_id
```

### 3.5 Channel mix of consent-passing Persons (aggregate, ops-safe)

```cypher
MATCH (p:Person {tenant_id: $tenant_id})-[:CONSENTED {purpose: $purpose}]->(:Consent)
WHERE p.quarantine = false
UNWIND p.channels AS channel
RETURN channel, count(*) AS persons
ORDER BY persons DESC
```

NL variants of these are what `POST /v1/graph/ask` produces via Ollama —
schema-prompted, read-only templates, result capped, tenant filter injected,
generated Cypher shown alongside the row table. Raw Cypher from clients is
**never** executed; `POST /v1/graph/cypher` accepts template-allowlisted
statements only (gate 5).

## 4. Consent & erasure model

**Consent capture.** `POST /v1/consents` (identity-service, W12) emits an
identity event; graph-sync upserts `(Person)-[:CONSENTED {purpose, at}]->(Consent)`.
Revocation sets `Consent.revoked_at`; the edge stays as audit but no longer
passes the audience predicate (`c.revoked_at IS NULL`).

**Audience gating.** Segment DSL compiles to Cypher with a mandatory
purpose-matching `CONSENTED` edge + `p.quarantine = false` (SPEC-W28 §5 gates
2/4). Downstream gates in notification-worker (DND 2442, quiet hours) are
unchanged — the graph gate is *additive*, earlier in the funnel.

**Erasure.** `POST /v1/consents/erasure` emits
`com.opendesk.consent.ErasureRequested` on `opendesk.consent.erasure.v1`
(`GRAPH_SYNC_ERASURE_TOPIC`). graph-sync consumes it and:

1. `MATCH (p:Person {tenant_id, person_id}) DETACH DELETE p` — the whole
   Person subgraph (contacts, consent edges, referral/message edges) is gone.
2. Emits `opendesk.graph.erasure.done.v1` (`GRAPH_SYNC_ERASURE_DONE_TOPIC`)
   as the audit trail (W12 §4 companion).
3. Re-consent after erasure is an explicit new `CONSENTED` edge — the Person
   node is re-created from fresh events only.

Enrichment jobs never resurrect erased Persons: `graph_enrichment.py`
properties are applied via `MERGE` keyed on `(tenant_id, person_id)` by
graph-sync, which drops enrichment for unknown/erased persons (no event-sourced
node -> nothing to enrich).

**Quarantine.** Imported, consent-unverified Persons carry `quarantine: true`:
query-visible (Graph Explorer, Ask) but audience-ineligible by construction
(§3.2 predicate). Verification flips the flag via the consent path.

## 5. Ops

### 5.1 Bring-up

```
docker compose -f docker-compose.yml -f infra/docker-compose.graph.yml up -d
# models (one-shot, idempotent — part of the stack):
docker compose -f docker-compose.yml -f infra/docker-compose.graph.yml up ollama-graph-models
```

Ports: FalkorDB on host **6380** (6379 is the platform redis), graph-service
on **7014**, graph-sync health/metrics on **7015**, graph Ollama on **11435**
(GPU profile: **11436**). Host 11434 belongs to the root stack's `ollama`
service (voice profile, `ollama/ollama:0.5.7` for voice-agent-runtime) — the
graph stack runs its own `ollama-graph` service so the two never collide;
in-cluster they are distinct DNS names (`ollama` vs `ollama-graph`).

### 5.2 Ollama bootstrap + GPU profile

`scripts/graph/ollama_bootstrap.sh` pulls `qwen2.5:7b-instruct` +
`nomic-embed-text` idempotently (skips models already present; env
`OLLAMA_CHAT_MODEL` / `OLLAMA_EMBED_MODEL` override). The `ollama-graph-models`
compose service runs the same script in-cluster.

CPU is the default profile. For NVIDIA hosts:

```
docker compose -f docker-compose.yml -f infra/docker-compose.graph.yml \
  --profile gpu up -d ollama-graph-gpu ollama-graph-models
```

`ollama-graph-gpu` shares the model volume (`ollama-graph-data`) with the CPU
service and listens on host 11436; point `OLLAMA_BASE_URL=http://ollama-graph-gpu:11434`
(graph-service) and `GRAPH_SYNC_OLLAMA_BASE_URL` (graph-sync) at it. Requires
the NVIDIA container toolkit on the host. The one-shot bootstrap service is
`ollama-graph-models` (CPU default; set `OLLAMA_HOST` for the GPU twin).

### 5.3 Backup & restore — FalkorDB RDB snapshot

The `graph-db` container runs with `--appendonly yes --save 60 1`, so
`/data/dump.rdb` on the `falkordb-data` volume is a crash-consistent snapshot
refreshed every 60s when writes occurred. Backup:

```
docker exec opendesk-graph-db redis-cli SAVE           # synchronous snapshot
VOL=$(docker volume ls -q --filter name=falkordb-data | head -1)  # project-prefixed
docker run --rm -v "$VOL":/data -v "$PWD:/out" alpine \
  cp /data/dump.rdb /out/graph-$(date +%F).rdb
```

Restore: stop `graph-db`, replace `dump.rdb` on the volume, start. The graph
is a derived store — worst case it is rebuilt from Kafka replay + the nightly
enrichment job, so RPO seconds / RTO minutes without elaborate tooling.

### 5.4 Lakehouse jobs

```
# nightly gold -> graph enrichment (topic opendesk.graph.enrichment.v1):
docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
  --master spark://spark-master:7077 /opt/spark-jobs/graph_enrichment.py
# graph -> Iceberg feature export (GNN/ART seams):
docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
  --master spark://spark-master:7077 /opt/spark-jobs/graph_export.py
```

### 5.5 Observability

Both services expose `/metrics` (workforce conventions). Prometheus
scrape-config fragment for `infra/observability/prometheus/prometheus.yml`
(kept here rather than editing the shared config in this wave):

```yaml
  # ---- W28 graph stack ----
  - job_name: graph-service
    static_configs:
      - targets: ["graph-service:7014"]
  - job_name: graph-sync
    static_configs:
      - targets: ["graph-sync:7015"]
```

Suggested alerts: graph-sync consumer lag per topic, erasure-topic lag (SLA —
gate 3), graph-service 5xx rate, Ollama chat/embed latency p95, FalkorDB
memory (`redis_memory_used_bytes`) against the container limit.

## 6. Compliance gates (SPEC-W28 §5 — binary, tested)

1. **Tenant isolation** — every node/edge carries `tenant_id`; graph-service
   injects the tenant filter on ALL paths. e2e: cross-tenant reads return
   empty/404 (`tests/e2e/test_graph_wave.py`).
2. **Consent-gated audiences** — materialization excludes Persons without a
   purpose-matching `CONSENTED` edge. e2e: no-consent person never appears.
3. **Erasure SLA** — `opendesk.consent.erasure.v1` -> Person subgraph gone
   within consumer SLA + `opendesk.graph.erasure.done.v1` audit event. e2e.
4. **Quarantine exclusion** — imported/consent-unverified Persons are
   query-visible but audience-ineligible. e2e.
5. **No raw Cypher** — template allowlist only; injection attempts rejected
   (400/403). e2e.

## 7. Seams delivered (not built this wave — SPEC-W28 §6)

- **GNN (Phase 3):** `iceberg.gold.graph_node_features` /
  `graph_edge_features` written by `graph_export.py`; prediction write-back
  path defined as `Person.propensity_*` via `graph_enrichment.py`.
- **ART (Phase 4):** `iceberg.gold.outreach_trajectories` (send x outcome x
  reward) populated from `opendesk.usage.events` trajectory logging.
- **Neo4j drop-in:** all access is Cypher via graph-service; an adapter
  remains a drop-in if an enterprise tenant mandates it.
