# Data Residency — Lagos Primary + af-south-1 DR (SPEC-W15, Agent D)

Deployment and residency guide for running the Agora stack for Nigerian
tenants under two regulatory drivers:

- **CBN data-residency directive (compliance deadline June 2026)** — payment
  and financial-ledger data of Nigerian data subjects must be stored and
  processed on infrastructure physically located in Nigeria. For Agora
  this covers the payments ledger (TigerBeetle), the payment/consent/PII
  rows in Postgres, and any object or log storage that carries payment or
  personal data.
- **NDPA 2023** — personal-data processing must be lawful, consent-based
  where required, and erasable on request. Agora's NDPA machinery
  (consent registry + tombstone erasure events, SPEC-W12 §4) is described in
  `docs/compliance-ndpa.md`; this document only covers the *residency*
  dimension — where bytes physically live.

**Target topology:** Lagos co-location facility = **primary** (all writes,
all personal/financial data at rest). **AWS `af-south-1` (Cape Town) = DR
only.** Cross-border replication into af-south-1 is a *transfer* under
NDPA §§41–43; it is permitted for DR under the adequacy/derogation rules,
but only if PII is minimised before it crosses the border (see
§Per-datastore mapping for what is replicated raw vs. redacted vs. not at
all). Financial-ledger replicas stay in-country (two Lagos racks/AZs) so the
DR region never holds a complete copy of the regulated ledger — it holds a
**recovery-capable** copy, consistent with CBN's residency intent while
still meeting RTO/RPO.

**Scope/honesty note.** This is *configuration and operations guidance* for
the existing stack. No new infrastructure code ships with this document; the
referenced files are the real, existing artifacts an operator edits. The dev
compose stack (`infra/docker-compose.core.yml` etc.) is single-node — the
production topology per datastore is pinned by
`docs/ADRs/0008-production-topology.md`; this document layers residency and
DR placement on top of ADR-0008 rather than replacing it.

---

## 1. Topology at a glance

| Site | Role | What runs there |
| --- | --- | --- |
| Lagos co-lo (primary) | All production traffic; system of record for every datastore | Full stack per ADR-0008: Patroni Postgres ×3, Kafka ×3, TigerBeetle ×3 (replicas 0–2), Redis, MinIO erasure-coded pool, OpenSearch ×3, Temporal, all services (`services/*`, gateway `infra/apisix/apisix.yaml`) |
| Lagos co-lo (second rack/AZ) | In-country ledger + DB replica | TigerBeetle replica (part of the 3-node cluster — spread replicas across racks), Postgres synchronous standby (`synchronous_mode: quorum` per ADR-0008) |
| AWS af-south-1 (Cape Town) | Warm DR, no steady-state traffic | Postgres async read replica + WAL-G archive, Kafka MirrorMaker 2 target cluster, MinIO DR bucket (mirrored), OpenSearch snapshot repository. **No TigerBeetle replica** — ledger recovery is from in-country replicas + TB data-file snapshots (§2.4) |

Failover direction is one-way (Lagos → af-south-1) with a planned failback;
see §5.

## 2. Per-datastore residency mapping (CBN June-2026 directive)

For each datastore: what it holds, its residency class, where primary and DR
copies live, and the concrete artifact to change.

### 2.1 Postgres 16 (identity, booking, conversation, payments metadata, consent)

- **Holds:** all service DBs — `identity` (incl. `identity.consents`, the
  NDPA consent registry, RLS-isolated per `tenant_isolation` policy),
  `booking`, `conversation`, `knowledge`, `crm_sync`, `twenty`,
  `analytics_meta`, `notifications`, `billing`, `kyc`, `platform`,
  `temporal`, `keycloak`, `permify`, `iceberg`, `opendesk`
  (see `PG_DBS` in `infra/backups/backup.sh` — W41-fixed default now
  includes `notifications`/`billing`/`kyc`/`platform`). Contains PII and
  payment metadata → **residency-restricted (Nigeria)**.
- **Primary:** Lagos, Patroni 3-node per ADR-0008 (`synchronous_mode:
  quorum` for the `booking` and ledger-adjacent metadata DBs). Dev single
  node: `infra/docker-compose.core.yml` service `postgres`
  (postgis/postgis:16-3.4).
- **DR:** async streaming **read replica** in af-south-1 **plus**
  **WAL-G** continuous archive to an S3 bucket *in af-south-1*
  (`wal-g wal-push` from the Lagos primary; restore with
  `wal-g backup-fetch` + `recovery_target` from the archive). ADR-0008
  already mandates WAL-G/pgBackRest replacing the dev `pg_dump` cron — the
  residency change is only *where the archive bucket lives*. Dev backup
  reference: `infra/backups/backup.sh` (pg_dump -Fc per DB, `BACKUP_KEEP`
  rotation) and `infra/backups/README.md` (which already points at
  WAL-G/pgBackRest for prod).
- **PII note:** the async replica and WAL archive contain raw PII
  (consent records, contact data). NDPA transfer basis must be documented in
  the tenant-facing privacy notice; the bucket must be SSE-KMS with a
  DR-region key and bucket-public-access blocked.
- **RPO/RTO:** RPO ≈ replication lag (alert on
  `pg_stat_replication` replay lag; target <5 min) + WAL-G archive cadence;
  RTO = promote replica or `backup-fetch` into a fresh Patroni cluster
  (target ≤4h, drill quarterly). The quarterly drill is executable:
  `tests/restore-drill/drill.py` performs a real dump → fresh-cluster
  `pg_restore` → assertion pass (row counts, RLS policies,
  `relforcerowsecurity`, tenant-deny) — see `tests/restore-drill/README.md`;
  executed runs are filed as `tests/restore-drill/RESULTS.md` (runbook:
  `docs/runbooks/operations.md` §6/§8).

### 2.2 Kafka (opendesk.* event backbone)

- **Holds:** event streams incl. `opendesk.payments.events`,
  `opendesk.identity.events`, `opendesk.consent.erasure.v1`,
  `opendesk.kyc.resolved.v1`, `cac.events` — full topic list in
  `infra/kafka/create-topics.sh`. Payment and identity events →
  **residency-restricted** while retained.
- **Primary:** Lagos, 3 brokers rf=3 `min.insync.replicas=2` per ADR-0008
  (dev topics are rf=1 — override at prod provisioning, do not edit
  `create-topics.sh`).
- **DR:** **MirrorMaker 2** active→passive into an af-south-1 cluster. The
  existing Strimzi CR `deploy/k3s/mirror-maker2.yaml` is the starting point:
  it currently replicates only `opendesk.transcripts-raw` edge→central with
  a `TODO: real endpoint`. For residency DR:
  - run a *second* MM2 cluster Lagos→af-south-1 with
    `topics: "opendesk\\..*|cac\\.events"` **excluding**
    `opendesk.transcripts-raw` (raw call audio transcripts are the largest
    PII payload; keep them in-country unless the legal review signs off —
    the enriched/redacted stream `opendesk.conversation.enriched` is the
    safer thing to replicate);
  - set `replication.factor: 2`+ on the DR cluster (the skeleton ships
    `replication.factor: 1` — dev MVP);
  - configure TLS + SCRAM-SHA-512 on the `central`/DR cluster entry per the
    secrets runbook (`docs/runbooks/secrets.md`) before applying — the
    skeleton ships neither.
  - Mirror consumer-group offsets (`sync.group.offsets.enabled: "true"`) for
    the analytics sink so failover does not reprocess the whole
    `cac.events` stream.
- **RPO/RTO:** RPO ≈ MM2 checkpoint lag (alert on
  `kafka.mirror.*record-count` lag; target <15 min), RTO = repoint producer
  bootstrap + consumer offsets (target ≤1h).

### 2.3 Redis 7 (cache, pacing, rate limits)

- **Holds:** notification pacing state, APISIX `limit-count` counters,
  short-lived session/cache keys. Transient, TTL-bound, non-ledger →
  **not residency-restricted** (no durable PII at rest beyond short TTLs).
- **Primary:** Lagos (dev: `redis:7-alpine`, `--appendonly yes`,
  `infra/docker-compose.core.yml`).
- **DR:** **none required.** Redis is rebuilt from Postgres/Kafka state on
  failover (pacing budgets re-derive; rate-limit counters may reset — an
  acceptable, documented degradation: limits fail *closed* at the gateway,
  so a cold Redis is conservative, not leaky). Do not replicate AOF
  cross-border; it can contain phone numbers in pacing keys.

### 2.4 TigerBeetle (financial ledger)

- **Holds:** the double-entry ledger for deposits, no-show fees, referral
  bounties, commission payouts (`services/payments-service/src/ledger/`).
  **Most strictly residency-restricted** — this is the core of the CBN
  directive.
- **Primary:** Lagos, 3-node cluster (`--replica-count=3`, replicas 0..2,
  VSR consensus tolerates 1 replica loss) per ADR-0008, spread across the
  two Lagos racks. Dev single replica: `infra/docker-compose.core.yml`
  service `tigerbeetle` (`--development`).
- **DR:** **no af-south-1 replica.** Rationale: a VSR replica outside
  Nigeria would put a full copy of the regulated ledger out of jurisdiction
  *and* add WAN latency to every quorum write. Instead:
  - the durability story is the 3 in-country replicas (ADR-0008:
    "Replication, not backups, is the durability story");
  - **async data-file snapshots** ship off-box: snapshot one *follower*
    replica's `0_0.tigerbeetle` data file (pause-free copy from a follower,
    as `infra/backups/backup.sh` already does with `TB_PAUSE`/`TB_DATA_FILE`
    for the dev container) and ship it to af-south-1 object storage,
    encrypted, on the same cadence as the WAL-G archive;
  - recovery in DR = bootstrap a fresh 3-node cluster from the latest
    snapshot, then replay `opendesk.payments.commands` from the MM2-replicated
    Kafka topics to close the gap. payments-service's `LedgerClient` takes a
    multi-address `TB_ADDRESSES` list (ADR-0008) — config-only change at
    failover.
- **RPO/RTO:** RPO = snapshot cadence + Kafka command replay (target ≤1h of
  ledger history re-derived, zero committed-loss inside Lagos); RTO = cluster
  bootstrap + replay (target ≤8h; this is the longest-RTO datastore and
  drives the DR drill schedule).

### 2.5 MinIO / lakehouse (Iceberg bronze/silver/gold, exports)

- **Holds:** `lake` bucket — Iceberg tables fed by the analytics sink
  (`services/analytics-pipeline/analytics_pipeline/`), incl. CAC gold tables
  (`cac_gold.daily_cac_by_channel`, `docs/cac-lakehouse.md`); `exports`
  bucket — tenant-facing exports. Contains pseudonymised PII and funnel
  data → **residency-restricted** (transcripts land here via the Fluvio
  sink — the rawest PII).
- **Primary:** Lagos, MinIO erasure-coded pool (dev:
  `infra/docker-compose.lakehouse.yml` service `minio`; Trino/Spark/dbt in
  `infra/lakehouse/`).
- **DR:** `mc mirror --watch` (continuous) or scheduled `mc mirror` of
  `lake` and `exports` to an af-south-1 bucket — exactly what
  `infra/backups/backup.sh` already does locally (`MINIO_BUCKETS="lake
  exports"`, `MC_IMAGE`); point the same pattern at the DR alias. The
  `iceberg` REST-catalog DB in Postgres (§2.1) must be recoverable
  *point-in-time consistent with* the object store (ADR-0008) — take the
  WAL-G base backup for `iceberg` immediately after each MinIO mirror
  checkpoint, or dbt/Spark recompute gold from bronze after restore.
- **Raw-audio exclusion (optional hardening):** if raw transcripts must not
  leave Nigeria, mirror only the Iceberg warehouse prefixes for
  bronze-minus-`transcripts_raw`/silver/gold and keep raw audio in a
  Lagos-only bucket.

### 2.6 OpenSearch (knowledge/transcript search indices)

- **Holds:** search indices over knowledge docs and enriched transcripts
  (`infra/opensearch/setup-indices.sh`; dev service in
  `infra/docker-compose.edge.yml`). Derived data → **restricted only via its
  source documents**.
- **Primary:** Lagos (3 nodes in prod per ADR-0008).
- **DR:** snapshot repository on the af-south-1 bucket
  (`PUT _snapshot/dr_repo` → S3), scheduled snapshots; or treat as fully
  rebuildable — re-index from the lakehouse bronze tables + knowledge
  service after failover. Rebuild is the cheaper, PII-safer option: nothing
  crosses the border until a DR event is declared.

### 2.7 Fluvio (edge transcript spine) — for completeness

Edge appliances buffer `opendesk.transcripts-raw` locally
(`infra/fluvio/README.md`, `infra/fluvio/deploy.sh`,
`infra/fluvio/kafka-sink-connector.yaml`) and store-and-forward to the Lagos
Kafka via MM2 (`deploy/k3s/mirror-maker2.yaml`). Edge devices are in-country
by definition (they sit in Nigerian stores/branches); no residency action
beyond ensuring the MM2 *target* is the Lagos cluster, not the DR cluster.

## 3. NDPA notes (tie-in to the W12 consent machinery)

Residency placement does not replace NDPA obligations; the existing
machinery keeps working across the DR boundary:

- **Consent registry** — `identity.consents` (RLS `tenant_isolation`,
  bootstrapped by the identity-service consent store,
  `services/identity-service/internal/consent/`). Because it lives in the
  `identity` DB, it is covered by the §2.1 replica + WAL-G plan; consent
  state is therefore consistent after a Postgres failover with no extra
  machinery.
- **Erasure events** — erasure is a tombstone plus a CloudEvent on
  `opendesk.consent.erasure.v1` (`docs/compliance-ndpa.md`;
  `opendesk.privacy.events` for the older GDPR flow). For DR correctness:
  `opendesk.consent.erasure.v1` **must** be in the MM2 replicated topic set
  (§2.2) so a DR-region consumer never serves erased subjects after
  failover. Tombstones are small and contain identifiers, not payloads —
  replicating them cross-border is NDPA-defensible; document it in the
  transfer register.
- **KYC** — `opendesk.kyc.resolved.v1` results and the consent gate before
  resolution (`docs/kyc.md`) follow the same rule: replicate the topic,
  never the raw BVN/NIN payloads into non-KMS-encrypted storage.
- **Right-to-erasure vs. backups:** erasure propagates to live systems via
  the tombstone events; WAL-G archives and MinIO mirrors age out on the
  backup rotation (`BACKUP_KEEP`, default 7 snapshots — set the prod
  retention so personal data in backups expires within the NDPA-compatible
  window, and never restore an erased subject's rows into a live system
  without re-applying the tombstone log).

## 4. What changes vs. the dev compose stack (operator checklist)

Everything below is configuration against existing files — no new infra
code:

1. `docs/ADRs/0008-production-topology.md` — adopt as the Lagos primary
   topology (Patroni ×3, Kafka ×3 rf=3, TB ×3, Temporal multi-node).
2. `deploy/k3s/mirror-maker2.yaml` — clone for Lagos→af-south-1; widen
   `topics` (keep `opendesk.transcripts-raw` excluded), raise
   `replication.factor`, add TLS/SCRAM per `docs/runbooks/secrets.md`.
3. Postgres: stand up the async replica + WAL-G archive to the DR bucket
   (replaces `infra/backups/backup.sh` pg_dump for prod, per
   `infra/backups/README.md`'s existing guidance).
4. TigerBeetle: `--replica-count=3` in Lagos across racks; schedule follower
   data-file snapshots to the DR bucket (extend the `TB_*` env contract in
   `infra/backups/backup.sh`); set `TB_ADDRESSES` multi-address in
   payments-service config.
5. MinIO: run the `mc mirror` pattern from `infra/backups/backup.sh`
   against a DR-region alias for `lake`/`exports`.
6. OpenSearch: register the DR snapshot repo, or document rebuild-from-lake
   as the DR path (`infra/opensearch/setup-indices.sh` re-run + reindex).
7. Redis: no action; document cold-start behaviour on failover.
8. Backups retention: set `BACKUP_KEEP` for the NDPA erasure window (§3).
9. Register the af-south-1 transfer in the NDPA transfer register with the
   per-datastore basis from §2.

## 5. Failover / failback summary

1. **Declare** — Lagos primary deemed unavailable beyond the Postgres
   promotion window.
2. **Postgres** — promote the af-south-1 read replica (or `wal-g
   backup-fetch` into a fresh Patroni cluster); replay final WAL from the
   archive.
3. **Kafka** — producers/consumers repoint bootstrap to the DR cluster;
   MM2 offset syncs resume consumers near their Lagos positions.
4. **TigerBeetle** — bootstrap from latest follower snapshot; replay
   `opendesk.payments.commands` from DR Kafka to close the gap; flip
   `TB_ADDRESSES`.
5. **Lakehouse/Search** — point Trino/Spark at the mirrored buckets;
   reindex OpenSearch or restore its snapshots.
6. **Traffic** — cut APISIX (`infra/apisix/apisix.yaml` upstreams) and
   Keycloak to the DR service endpoints; rate limits fail closed on cold
   Redis.
7. **Failback** — reverse MM2 (DR→Lagos), re-baseline Postgres with WAL-G,
   resync TigerBeetle by snapshot+replay, then re-point traffic. Failback is
   always planned, never automatic.

*Residual honesty notes:* the MM2 manifest is a skeleton (its own header
says so); WAL-G and the Patroni/TB multi-node setups are ADR-pinned but not
yet implemented in the repo; the dev compose stack remains the reference for
service wiring only. Quarterly DR drills should time §2.1 and §2.4 recovery
since they bound the platform RTO.
