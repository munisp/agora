# rdf-export (SPEC-W54 Scope A)

Batch linked-data exporter for the tenant knowledge graph. One call exports
ONE tenant's graph from graph-service's internal NDJSON export and lands
standards-based RDF — **Turtle** and **JSON-LD 1.1** — to the MinIO `lake`
bucket next to the parquet extracts:

```
s3://lake/extracts/graph_rdf/{tenant_id}/{snapshot_date}/graph.ttl
s3://lake/extracts/graph_rdf/{tenant_id}/{snapshot_date}/graph.jsonld
```

Sibling of the parquet contract consumed by
`infra/lakehouse/spark/jobs/graph_export.py`
(`extracts/graph_nodes|graph_edges/`); that job does **not** read the RDF
extracts (they serve compliance/audit/interop, not the Spark feature
pipeline).

## Data flow

```
POST /v1/rdf-export/run {tenant_id, snapshot_date?}
  -> GET graph-service /v1/graph/internal/export/nodes|edges
       (X-Internal-Token, mandatory tenant_id, NDJSON stream,
        X-Row-Count / X-Snapshot-Date headers — contract:
        services/graph-service/app/routers/internal_export.py:44-116)
  -> streaming line-by-line parse (bounded memory; per-row tenant re-verify)
  -> mapping (src/mapping.rs): labels->classes, edge types->predicates,
     tenant->named graph, ids pass-through (already W28-hashed at source)
  -> serialize (src/serialize.rs): oxirs Turtle + JSON-LD 1.1 (inline
     @context), all quads in the tenant named graph
  -> storage (src/storage.rs): two SigV4-signed PUTs to MinIO
  -> response {tenant_id, snapshot_date, nodes, edges, triples,
     skipped_unknown, outputs[]}
```

**Batch semantics:** the run is SYNCHRONOUS within `RDF_EXPORT_TIMEOUT_S`
(default 300s; 504 on expiry). This is an ops/compliance batch tool, not an
interactive API — no job queue, no partial results.

## API

| Route | Auth | Notes |
|---|---|---|
| `GET /healthz` | none | liveness |
| `GET /metrics` | none (mesh-only) | Prometheus text counters |
| `POST /v1/rdf-export/run` | `X-Internal-Token` | one synchronous export |

`X-Internal-Token` is a constant-time compare against
`RDF_EXPORT_INTERNAL_TOKEN`, **fail-closed**: unset server-side → 503
(disabled-config), missing/wrong → 401. JWTs are never accepted.
**Mesh-internal only: there is deliberately NO APISIX route** (kyc-service
precedent for internal-only services; the gateway hard-blocks
`/api/graph/internal*` per GW-07 regardless).

## Configuration (env)

| Var | Default | Notes |
|---|---|---|
| `RDF_EXPORT_BIND` | `0.0.0.0:7021` | listen address |
| `GRAPH_SERVICE_URL` | *(required)* | graph-service base, e.g. `http://graph-service:7014` |
| `GRAPH_INTERNAL_TOKEN` | *(required)* | outbound token; MUST equal graph-service's `INTERNAL_TOKEN` (graph overlay) |
| `RDF_EXPORT_INTERNAL_TOKEN` | *(unset → API 503)* | inbound token guard |
| `MINIO_ENDPOINT` | `http://minio:9000` | bare `host:port` gets `http://` prepended |
| `RDF_MINIO_BUCKET` | `lake` | extract bucket |
| `MINIO_REGION` | `us-east-1` | SigV4 region (MinIO accepts any) |
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | *(required)* | SigV4 credentials (shared MinIO indirection, OOS-18) |
| `RDF_EXPORT_TIMEOUT_S` | `300` | whole-run budget |

Compose wires `${VAR:-default}` fallbacks; `.env.example` documents the dev
values. Required vars unset = no boot (fail-closed).

## Ontology & mapping

`ontology/agora.ttl` (repo root) is the platform ontology: classes for the
docs/graph.md §2 node labels (Person, Contact, Consent, Booking, Case,
Location, Tenant, Offering, Campaign — plus SPEC-W30 `Alert`) with
schema.org alignment where sensible (`agora:Person rdfs:subClassOf
schema:Person`, `Booking → schema:Reservation`, `Location → schema:Place`,
`Tenant → schema:Organization`, `Offering → schema:Offer`), 12 object
properties for the graph-sync edge types, datatype properties for the export
node-row fields, and an `owl:Ontology` header with version IRI
`https://agora.example/ontology/1.1.0`.

The `agora:` namespace (`https://agora.example/ontology#`) is a documented
**placeholder** (reserved documentation domain) — stable IRIs for exports,
not a published vocabulary.

**Ontology versioning discipline:** any term addition/change bumps the
`owl:versionIRI` (`…/ontology/X.Y.Z`) and the JSON-LD `@context` in
`src/serialize.rs` is versioned as a unit with it. Never silently repurpose
an existing term. mapping.rs ↔ agora.ttl coherence is test-enforced in both
directions (`tests/w54_pipeline.rs::ontology_mapping_coherence`).

### Mapping notes

- Node id → subject IRI `https://agora.example/tenant/{uuid}/node/{label}/{id}`.
  Person ids arrive **already W28-hashed** at source (export.py `w28_hash`);
  they pass through verbatim — this service **never re-hashes** and the
  export contract carries **no name/phone fields**, so no plaintext PII can
  reach the RDF output (grep-tested in CI).
- Numbers → `xsd:integer`/`xsd:double`, booleans → `xsd:boolean`,
  timestamps → `xsd:dateTime`, partition date → `xsd:date`. Empty/absent
  properties are skipped. `consent_marketing` and `quarantine` are
  compliance-relevant and always carried when present.
- **Unknown labels / edge types are skipped + counted + reported**
  (`skipped_unknown` in the response, `rdf_export_skipped_unknown_total` in
  metrics) — never guessed.
- **Not-yet-mapped edge types** present in the live graph: `MESSAGED`
  (campaign sends), `RECOMMENDED_FOR` (graph-ml), `FLAGGED`
  (fraud-engine alert→person; the ontology term is `ALERTED` per the SPEC
  enumeration). Rows with these types are skip+counted by design until
  docs/graph.md §2 adopts them into the canonical schema and the ontology
  version bumps.
- Edge scalar properties (`weight`, `edge_at`) are **not reified** in v1 —
  the platform edge schema's compliance-relevant data lives on the nodes.
- Edge endpoints are always `rdf:type`'d (Booking/Consent/Location/Tenant/
  Case never appear as node rows in the export contract — only
  Person/Contact/Offering/Campaign do), so the RDF graph stays fully typed.

### Named-graph posture

Every quad sits in the per-tenant named graph
`https://agora.example/tenant/{uuid}` — never a shared default graph mixing
tenants. Turtle 1.1 has no graph dimension, so `graph.ttl` carries the
triples bare; the file is inherently single-tenant (one tenant per run, the
tenant is in the S3 key, and the graph-metadata triples
`agora:tenantId`/`agora:snapshotDate` survive into the Turtle as plain
triples on the graph IRI). `graph.jsonld` carries the full named-graph
structure (`@id` + `@graph`).

## Erasure / lifecycle (NDPA posture)

RDF extracts are **ephemeral derived snapshots**: they are batch products of
the graph (which is itself derived/rebuildable from Kafka replay), never
authoritative. On person/tenant erasure the corresponding
`extracts/graph_rdf/{tenant_id}/…` objects **MUST be deleted with the
snapshot lifecycle** — a persisted Turtle/JSON-LD dump of an erased subject
is a compliance liability (RECON §5.2). Rebuild on demand by re-running the
export; treat old snapshot partitions as expired evidence. Person IRIs are
hashed and no name literals exist, but hashed IRIs are still personal data
under NDPA (pseudonymous, not anonymous).

## MinIO client choice (justification)

Hand-rolled SigV4 PUT (`src/storage.rs`, ~90 lines) — a direct Rust
translation of the proven `notification-worker/internal/activities/s3.go`
logic (path-style, `host;x-amz-content-sha256;x-amz-date` signed headers),
on top of `sha2`/`hmac`/`hex` + the reqwest client the service already has.
The official AWS SDK / `minio` Rust crates pull a large async + XML +
credential-provider tree for exactly one PUT; `rust-s3` drags its own hyper
stack. Key derivation is known-answer-tested against the AWS SigV4 test
vector construction.

## oxirs maturity caveat (exact-pin rationale)

oxirs is a **very young** project (first public crate 2025-09; RECON §5.1).
The 0.4.1 release closed a full oxirs-fuseki authentication bypass and a
SPARQL parser bug that silently dropped FILTER clauses — security-critical
defects were shipping weeks before this wave. Therefore:

- `oxirs-core` / `oxirs-ttl` are **exact-pinned** (`=0.4.1`,
  `default-features = false`) in Cargo.toml **and** Cargo.lock. Any bump is
  a deliberate, reviewed change.
- Only the stable surface we actually use is touched: `oxirs-core::model`
  (Quad/NamedNode/Literal) and `oxirs-ttl`'s `TurtleWriter`,
  `jsonld::JsonLdWriter`, `TurtleParser`, `jsonld::JsonLdProcessor`.
- **No SPARQL endpoint, no live RDF store, no oxirs-fuseki** this wave
  (explicit SPEC NO-GOs — a raw SPARQL surface contradicts compliance
  gate 5, template-allowlist-only query access).
- oxirs-ttl's JSON-LD support **was sufficient** for the fixed term set
  (inline `@context`, typed literals, named-graph `@id`+`@graph` grouping),
  so no hand-rolled JSON-LD writer was needed; the round-trip tests parse
  both outputs back with oxirs and assert counts + named-graph separation,
  which is the regression net if a future oxirs bump changes behavior.
- **Capability gaps found this wave (verified against the 0.4.1 sources,
  all worked around — none affect the shipped output):**
  1. `oxirs_ttl::writer::TurtleWriter` abbreviates IRIs by naive prefix
     match: a prefix whose remainder contains `/` yields invalid Turtle
     (`/` is not PN_LOCAL). Our node/graph IRIs live under
     `https://agora.example/tenant/…`, so the `tenant` prefix is registered
     in the JSON-LD `@context` only (compact IRIs allow `/`), never in the
     Turtle writer — those IRIs serialize as full `<IRI>`s.
  2. The oxirs-ttl Turtle tokenizer mis-tracks positions around multi-byte
     UTF-8 (e.g. an em-dash in a comment corrupts subsequent lexing —
     reproduced: `PrefixName("a")` error after a comment containing `—`).
     `ontology/agora.ttl` is therefore kept pure ASCII. This is a *parser*
     bug; our writer output is unaffected, but re-parsing non-ASCII Turtle
     literals with oxirs-ttl 0.4.1 is not trusted (round-trip tests are
     ASCII-clean by construction).
  3. `oxirs_ttl::TurtleParser` rejects a bare prefix with empty local part
     (`agora: a owl:Ontology`); the ontology header uses the full IRI as
     subject instead.
- Fallback if oxirs ever becomes untenable: `oxigraph`'s `oxrdf`/`oxttl`/
  `oxjsonld` (the lineage oxirs ported) behind the same mapping.rs seam.

## Tests (offline)

```
cargo test
```

- golden fixture (`tests/fixtures/*.ndjson`, test-only, every class +
  predicate + unknown rows + decoy PII fields) → Turtle + JSON-LD;
- round-trip parse via oxirs: triple counts, spot triples, named-graph
  separation (no default-graph leakage);
- zero-plaintext-PII grep on both outputs;
- unknown label/predicate skip counting;
- X-Internal-Token fail-closed matrix (503 unset / 401 missing-or-wrong),
  422 input validation, 502 upstream failure;
- NDJSON robustness: blank lines, CRLF, missing final newline;
- SigV4 key-derivation known-answer test.

`tests/w54_http.rs` uses LOOPBACK stubs only (127.0.0.1:0) for the
graph-service and MinIO ends of the golden run — nothing leaves the host.

## Run (dev)

```
docker compose -f docker-compose.yml -f infra/docker-compose.graph.yml up -d rdf-export
curl -X POST http://rdf-export:7021/v1/rdf-export/run \
  -H "X-Internal-Token: $RDF_EXPORT_INTERNAL_TOKEN" \
  -H "content-type: application/json" \
  -d '{"tenant_id": "<uuid>"}'   # run from inside the opendesk network
```
