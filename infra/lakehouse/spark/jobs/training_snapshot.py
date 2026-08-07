"""training_snapshot — lakehouse silver/gold -> versioned training datasets
(SPEC-W33 §2 A2, Slice A pipeline closure; reference stats feed W33-C drift
PSI per SPEC-W33 §4 C2).

Reads (ALL optional — graceful degradation, cac_analytics.py/graph_export.py
pattern: a missing source logs a warning and yields an empty/partial
snapshot instead of failing the pipeline):

  * iceberg.gold.graph_node_features (env TRAINING_NODES_TABLE) — per-node
    feature export produced by graph_export.py from the graph-service
    internal export endpoints. Person node_ids are already W28-hashed
    upstream (I6: no plaintext PII enters this job).
  * iceberg.gold.graph_edge_features (env TRAINING_EDGES_TABLE) — edge
    export, same producer.
  * iceberg.bronze.cac_events (env TRAINING_CAC_TABLE) — funnel events
    landed by the analytics-pipeline bronze sink (W33-A2). Used for the
    credit_features first-transaction signal.
  * LABELS extract (parquet on MinIO, env TRAINING_LABELS_PATH, default
    s3://lake/extracts/labels/) — W33-A1 labeled synthetic fraud ground
    truth (labels.json -> parquet upload). Columns:
      entity_id string, scenario string, fraud boolean
    Joined to fraud_features on person_id = entity_id (labels are optional;
    unlabeled rows keep label_fraud NULL — honest provenance, I3).

Writes (parquet, partitioned by tenant_id, mode=overwrite for idempotent
re-runs of the same snapshot_date):

  * {TRAINING_BASE_PATH}/fraud_features/{snapshot_date}/   (default base
    s3://lake/training/)
  * {TRAINING_BASE_PATH}/credit_features/{snapshot_date}/
  * {TRAINING_BASE_PATH}/gnn_export/{snapshot_date}/nodes/ and .../edges/
  * {TRAINING_BASE_PATH}/{family}/{snapshot_date}/manifest.json per family

manifest.json (dual schema, SPEC-W34 GF2): family, snapshot_date,
created_at, seed (env TRAINING_SEED, default 42 — recorded per I3 even
though this job itself is deterministic and does no sampling), source table
paths, row counts, and reference distribution stats per numeric feature
{count, mean, std, min, max} — the W33-C drift monitor (PSI/KS) compares
incoming feature distributions against these. std is the SAMPLE stddev
(Spark stddev, ddof=1); NULL when fewer than 2 non-null values.

Every manifest ALSO carries the drift contract that
services/model-registry/model_registry/drift.py consumes verbatim
(schema opendesk/training-manifest/v1, kept alongside the legacy
schema_version key for backcompat):

  * schema: "opendesk/training-manifest/v1"
  * features: {<name>: {histogram: {edges, counts}}} — fixed-bin
    histograms (HISTOGRAM_BINS equal-width bins over observed min/max,
    degenerate single-value ranges expanded to [v-0.5, v+0.5]) computed
    from the actual snapshot feature columns, binned with EXACTLY the
    drift.py histogram_counts semantics (bisect_right, edge-clamped).
  * score_baseline: intentionally EMPTY histogram + note — snapshots carry
    training labels, not serving scores, so no honest score baseline exists
    at snapshot time (the drift sweep's score leg uses the trailing 7-day
    serving baseline, see drift.py).
  * manifest_hash: "sha256:<hex>" over the canonical JSON (sorted keys,
    compact separators) minus the hash field itself.

REGISTRY SYNC (SPEC-W34 GF2): the drift sweep reads
$DRIFT_MANIFEST_DIR/<registry-family>.json where registry families are
fraud-ml / credit-ml / graphsage — NOT the snapshot family names. The
--registry-sync DIR mode (standalone, Spark-free, local/file:// paths;
--sync-only skips the snapshot job) copies each snapshot manifest through
FAMILY_REGISTRY_MAPPING into DIR. During a Spark run, setting
REGISTRY_SYNC_DIR additionally writes the registry manifests next to the
snapshot run (works on s3a:// too, via the Hadoop FS writer).

PII discipline (I6/GA4): person identifiers are hashed at the graph-service
export endpoint (W28 sha256(salt|tenant|id)); this job never sees raw PII
and emits no name/phone columns.

Pure-function reference implementations (row shaping, feature stats,
manifest builder) are Spark-free at the top of this module; the Spark
transforms mirror them. The pyspark import is guarded so the module stays
importable on driver-less CI boxes (cac_analytics.py/graph_export.py
pattern). Unit tests: test_training_snapshot.py.

Run (packages are injected by the job; no --packages needed):

  docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \\
    --master spark://spark-master:7077 \\
    /opt/spark-jobs/training_snapshot.py
"""

import argparse
import bisect
import hashlib
import json
import os
from datetime import date, datetime, timezone

try:  # driver-less CI boxes have no pyspark — pure functions stay importable.
    from pyspark.sql import DataFrame, SparkSession
    from pyspark.sql import functions as F
    from pyspark.sql import types as T
except ImportError:  # pragma: no cover - exercised implicitly on CI lint boxes
    DataFrame = SparkSession = None
    F = T = None

BASE_PATH = os.getenv("TRAINING_BASE_PATH", "s3://lake/training/").rstrip("/")
SNAPSHOT_DATE = os.getenv("TRAINING_SNAPSHOT_DATE") or date.today().isoformat()
SEED = os.getenv("TRAINING_SEED", "42")

NODES_TABLE = os.getenv("TRAINING_NODES_TABLE", "iceberg.gold.graph_node_features")
EDGES_TABLE = os.getenv("TRAINING_EDGES_TABLE", "iceberg.gold.graph_edge_features")
CAC_TABLE = os.getenv("TRAINING_CAC_TABLE", "iceberg.bronze.cac_events")
LABELS_PATH = os.getenv("TRAINING_LABELS_PATH", "s3://lake/extracts/labels/")

FAMILIES = ("fraud_features", "credit_features", "gnn_export")
MANIFEST_SCHEMA_VERSION = "training-manifest-v1"

# Drift contract (SPEC-W34 GF2): the schema identifier drift.py's
# DirectoryManifestProvider expects at top level.
REGISTRY_MANIFEST_SCHEMA = "opendesk/training-manifest/v1"

# Snapshot family -> model-registry family. The drift sweep enumerates
# REGISTRY families (fraud-ml/credit-ml/graphsage) and looks up
# $DRIFT_MANIFEST_DIR/<registry-family>.json; this mapping is the single
# explicit translation point between the two vocabularies.
FAMILY_REGISTRY_MAPPING = {
    "fraud_features": "fraud-ml",
    "credit_features": "credit-ml",
    "gnn_export": "graphsage",
}

# Fixed-bin histogram width for the drift reference distributions. 10 bins
# matches the drift.py score-leg convention (edges = 11 points over range).
HISTOGRAM_BINS = 10

# I3 honest-metrics note stamped into every manifest: Slice A validation is
# synthetic-only (W33-A1 labeled synthetic generator); no platform ground
# truth exists yet.
PROVENANCE_NOTE = (
    "Slice A training snapshot: features derive from lakehouse graph/bronze "
    "exports; labels, when present, come from the W33-A1 SYNTHETIC labeled "
    "generator (validation is synthetic-only until adjudication labels land)."
)

ICEBERG_PACKAGES = [
    "org.apache.iceberg:iceberg-spark-runtime-3.5_2.12:1.6.1",
    "org.apache.iceberg:iceberg-aws-bundle:1.6.1",
    # hadoop-aws for raw parquet/manifest writes to s3a:// (MinIO) — the
    # iceberg-aws-bundle alone only covers the Iceberg S3FileIO path.
    "org.apache.hadoop:hadoop-aws:3.3.4",
]

# Numeric features per dataset — the reference-stat set the W33-C drift
# monitor consumes (mean/std/min/max per feature).
PERSON_NUMERIC_FEATURES = (
    "in_degree",
    "out_degree",
    "bookings_total",
    "ltv_cents",
    "no_show_rate",
    "propensity_show",
    "propensity_convert",
)
CREDIT_NUMERIC_FEATURES = (
    "bookings_total",
    "ltv_cents",
    "avg_booking_cents",
    "no_show_rate",
    "propensity_show",
    "propensity_convert",
    "first_txn_ngn",
)
EDGE_NUMERIC_FEATURES = ("weight",)

# cac.events vocabulary used for the credit first-transaction signal
# (SPEC-W13 §2).
FIRST_TXN_EVENT = "first_txn"

# ---------------------------------------------------------------------------
# Pure functions (Spark-free reference implementation — unit-tested)
# ---------------------------------------------------------------------------


def numeric_stats(values) -> dict | None:
    """Reference stats for one numeric feature: {count, mean, std, min, max}.

    None values are skipped; std is the SAMPLE stddev (ddof=1, matching
    Spark's stddev) and is None when fewer than 2 non-null values — the
    Spark transform returns NULL in the same case. Returns None when there
    are no non-null values at all.
    """
    nums = [float(v) for v in values if v is not None and not isinstance(v, bool)]
    if not nums:
        return None
    count = len(nums)
    mean = sum(nums) / count
    std = None
    if count >= 2:
        var = sum((v - mean) ** 2 for v in nums) / (count - 1)
        std = var ** 0.5
    return {"count": count, "mean": mean, "std": std, "min": min(nums), "max": max(nums)}


def compute_feature_stats(rows, numeric_fields) -> dict:
    """{feature: {count, mean, std, min, max}} over the requested numeric
    fields; features with no non-null values are omitted (Spark: NULL stats
    are dropped from the manifest too)."""
    stats = {}
    for field in numeric_fields:
        s = numeric_stats([row.get(field) for row in rows])
        if s is not None:
            stats[field] = s
    return stats


def _numeric_values(values) -> list[float]:
    """Non-null, non-bool values as floats (same filter as numeric_stats)."""
    return [float(v) for v in values if v is not None and not isinstance(v, bool)]


def histogram_edges(lo: float, hi: float, bins: int = HISTOGRAM_BINS) -> list[float]:
    """N+1 ascending equal-width bin edges over [lo, hi]. A degenerate
    (single-value) range is expanded to [v-0.5, v+0.5] so edges strictly
    ascend and the drift.py binning never divides by a zero width."""
    lo, hi = float(lo), float(hi)
    if hi <= lo:
        lo, hi = lo - 0.5, hi + 0.5
    return [lo + (hi - lo) * i / bins for i in range(bins + 1)]


def histogram_counts_on_edges(samples, edges) -> list[int]:
    """Bin ``samples`` on ``edges`` — IDENTICAL semantics to
    model_registry/drift.py histogram_counts (bisect_right; values below
    edges[0] fall in the first bin, above edges[-1] in the last). Kept as a
    local copy so this module stays dependency-free (I5)."""
    counts = [0] * (len(edges) - 1)
    for v in samples:
        i = bisect.bisect_right(edges, v) - 1
        i = min(max(i, 0), len(counts) - 1)
        counts[i] += 1
    return counts


def feature_histogram(values, bins: int = HISTOGRAM_BINS) -> dict | None:
    """{edges: [e0..eN], counts: [c0..cN-1]} for one numeric feature over
    the snapshot data, or None when there are no non-null values. Edges are
    HISTOGRAM_BINS equal-width bins over the observed min/max (degenerate
    range expanded, see histogram_edges), so the drift sweep can bin
    serving values on the same edges."""
    nums = _numeric_values(values)
    if not nums:
        return None
    edges = histogram_edges(min(nums), max(nums), bins)
    return {"edges": edges, "counts": histogram_counts_on_edges(nums, edges)}


def compute_feature_histograms(rows, numeric_fields, bins: int = HISTOGRAM_BINS) -> dict:
    """{feature: {histogram: {edges, counts}}} over the requested numeric
    fields; features with no non-null values are omitted (mirrors
    compute_feature_stats)."""
    out = {}
    for field in numeric_fields:
        h = feature_histogram([row.get(field) for row in rows], bins)
        if h is not None:
            out[field] = {"histogram": h}
    return out


def empty_score_baseline() -> dict:
    """Documented-empty score baseline: snapshots carry training LABELS,
    not serving SCORES, so no honest score histogram exists at snapshot
    time. drift.py's score leg compares serving windows against the
    trailing 7-day serving baseline, not against this field."""
    return {
        "histogram": {"edges": [], "counts": []},
        "note": (
            "No model scores exist at snapshot time (snapshots carry "
            "training labels, not serving scores); the drift sweep's score "
            "leg uses the trailing 7-day serving baseline "
            "(model_registry/drift.py). Intentionally empty."
        ),
    }


def manifest_hash(manifest: dict) -> str:
    """"sha256:<hex>" over the canonical JSON (sorted keys, compact
    separators) of the manifest MINUS the manifest_hash field itself —
    the format the opendesk/training-manifest/v1 schema documents in
    drift.py. drift.py currently RECORDS the hash (provenance, I2) and does
    not verify it at load time; verify_manifest_hash is provided for
    consumers/tests that want the integrity check."""
    body = {k: v for k, v in manifest.items() if k != "manifest_hash"}
    canonical = json.dumps(body, sort_keys=True, separators=(",", ":"))
    return "sha256:" + hashlib.sha256(canonical.encode("utf-8")).hexdigest()


def verify_manifest_hash(manifest: dict) -> bool:
    """Recompute and compare the manifest_hash (see manifest_hash)."""
    declared = manifest.get("manifest_hash")
    return isinstance(declared, str) and declared == manifest_hash(manifest)


def _require(row, *keys) -> bool:
    """Hygiene: rows missing any required key are dropped (tenant_id is
    always required — compliance gate 1, graph_export.py pattern)."""
    return all(row.get(key) is not None for key in keys)


def fraud_feature_rows(node_rows, label_rows=()):
    """fraud_features: one row per Person node feature row + optional A1
    label join (person_id = entity_id). Structural per-person aggregates
    (degree, booking/LTV/no-show, propensity) match the graph export
    contract; labels stay NULL when the labels extract is absent or has no
    row for the person (unlabeled != benign — honest provenance, I3).

    Deterministically ordered by (tenant_id, person_id).
    """
    labels = {}
    for lab in label_rows or ():
        entity = lab.get("entity_id")
        if entity is None:
            continue
        labels[str(entity)] = lab
    out = []
    for row in node_rows:
        if not _require(row, "tenant_id", "node_id"):
            continue
        if row.get("label") != "Person":
            continue
        lab = labels.get(str(row["node_id"]))
        out.append(
            {
                "snapshot_date": row.get("snapshot_date"),
                "tenant_id": row["tenant_id"],
                "person_id": str(row["node_id"]),
                "in_degree": row.get("in_degree"),
                "out_degree": row.get("out_degree"),
                "bookings_total": row.get("bookings_total"),
                "ltv_cents": row.get("ltv_cents"),
                "no_show_rate": row.get("no_show_rate"),
                "propensity_show": row.get("propensity_show"),
                "propensity_convert": row.get("propensity_convert"),
                "consent_marketing": row.get("consent_marketing"),
                "quarantine": row.get("quarantine"),
                "channel_of_first_touch": row.get("channel_of_first_touch"),
                "label_fraud": lab.get("fraud") if lab else None,
                "label_scenario": lab.get("scenario") if lab else None,
            }
        )
    out.sort(key=lambda r: (r["tenant_id"], r["person_id"]))
    return out


def credit_feature_rows(node_rows, cac_rows=()):
    """credit_features: one row per Person node with financial aggregates
    (bookings/LTV/avg ticket/no-show) + the first-transaction signal from
    bronze cac_events (event_name = first_txn), joined best-effort on
    entity_id = person_id (cac entity ids are lead ids today, so the join
    is NULL until the id spaces align — documented, never fabricated).

    Deterministically ordered by (tenant_id, person_id).
    """
    first_txn = {}
    for ev in cac_rows or ():
        if str(ev.get("event_name") or "").strip().lower() != FIRST_TXN_EVENT:
            continue
        entity = ev.get("entity_id")
        if entity is None:
            continue
        entity = str(entity)
        current = first_txn.get(entity)
        ts = ev.get("event_ts")
        if current is None or (ts is not None and str(ts) < str(current[1])):
            first_txn[entity] = (ev.get("amount_ngn"), ts)
    out = []
    for row in node_rows:
        if not _require(row, "tenant_id", "node_id"):
            continue
        if row.get("label") != "Person":
            continue
        bookings = row.get("bookings_total")
        ltv = row.get("ltv_cents")
        avg = (float(ltv) / float(bookings)) if bookings and ltv is not None else None
        txn = first_txn.get(str(row["node_id"]))
        out.append(
            {
                "snapshot_date": row.get("snapshot_date"),
                "tenant_id": row["tenant_id"],
                "person_id": str(row["node_id"]),
                "bookings_total": bookings,
                "ltv_cents": ltv,
                "avg_booking_cents": avg,
                "no_show_rate": row.get("no_show_rate"),
                "consent_marketing": row.get("consent_marketing"),
                "propensity_show": row.get("propensity_show"),
                "propensity_convert": row.get("propensity_convert"),
                "channel_of_first_touch": row.get("channel_of_first_touch"),
                "first_txn_ngn": txn[0] if txn else None,
                "first_txn_at": txn[1] if txn else None,
            }
        )
    out.sort(key=lambda r: (r["tenant_id"], r["person_id"]))
    return out


def gnn_node_rows(node_rows):
    """gnn_export nodes: the graph node feature contract verbatim,
    hygiene-filtered (tenant_id + node_id mandatory). Deterministic order."""
    out = []
    for row in node_rows:
        if not _require(row, "tenant_id", "node_id"):
            continue
        out.append(dict(row))
    out.sort(key=lambda r: (r["tenant_id"], str(r.get("label")), str(r["node_id"])))
    return out


def gnn_edge_rows(edge_rows):
    """gnn_export edges: the graph edge feature contract verbatim,
    hygiene-filtered. Deterministic order."""
    out = []
    for row in edge_rows:
        if not _require(row, "tenant_id", "src_id", "dst_id"):
            continue
        out.append(dict(row))
    out.sort(key=lambda r: (r["tenant_id"], str(r.get("edge_type")),
                            str(r["src_id"]), str(r["dst_id"])))
    return out


def build_manifest(
    family,
    snapshot_date,
    sources,
    row_counts,
    feature_stats,
    seed=SEED,
    created_at=None,
    feature_histograms=None,
    score_baseline=None,
) -> dict:
    """manifest.json builder (dual schema, SPEC-W34 GF2): the legacy
    training-manifest-v1 keys (schema_version, feature_stats, ...) are kept
    for backcompat AND the drift contract consumed by
    model_registry/drift.py is emitted alongside:
    schema="opendesk/training-manifest/v1", features.<name>.histogram,
    score_baseline (empty/documented by default — snapshots have labels,
    not scores), and manifest_hash (sha256 over the canonical JSON minus
    the hash field). created_at is injected so tests can pin it; defaults
    to now (UTC). Keys are sorted on serialization for byte-stable diffs."""
    if family not in FAMILIES:
        raise ValueError(f"unknown training family {family!r}")
    date.fromisoformat(str(snapshot_date))  # contract: YYYY-MM-DD
    manifest = {
        # --- legacy keys (training-manifest-v1), unchanged consumers
        "schema_version": MANIFEST_SCHEMA_VERSION,
        "family": family,
        "snapshot_date": str(snapshot_date),
        "created_at": created_at or datetime.now(timezone.utc).isoformat(),
        "seed": str(seed),
        "path": f"{BASE_PATH}/{family}/{snapshot_date}/",
        "sources": list(sources),
        "row_counts": dict(row_counts),
        "feature_stats": dict(feature_stats),
        "notes": PROVENANCE_NOTE,
        # --- drift contract (opendesk/training-manifest/v1, drift.py)
        "schema": REGISTRY_MANIFEST_SCHEMA,
        "features": dict(feature_histograms or {}),
        "score_baseline": score_baseline
        if score_baseline is not None else empty_score_baseline(),
    }
    manifest["manifest_hash"] = manifest_hash(manifest)
    return manifest


def registry_manifest(manifest: dict) -> dict:
    """Snapshot manifest -> registry manifest: same content, family name
    translated via FAMILY_REGISTRY_MAPPING (drift.py looks up
    <registry-family>.json), manifest_hash recomputed over the translated
    body so the integrity hash stays honest."""
    family = manifest.get("family")
    if family not in FAMILY_REGISTRY_MAPPING:
        raise ValueError(f"no registry-family mapping for {family!r}")
    out = dict(manifest)
    out["family"] = FAMILY_REGISTRY_MAPPING[family]
    out.pop("manifest_hash", None)
    out["manifest_hash"] = manifest_hash(out)
    return out


def _local_path(path: str) -> str:
    """Plain local path (file:// URIs unwrapped). s3:// etc. are rejected —
    the Spark run syncs object-store manifests via REGISTRY_SYNC_DIR."""
    if path.startswith("file://"):
        return path[len("file://"):]
    if "://" in path:
        raise ValueError(
            f"--registry-sync reads local/file:// snapshot manifests only; "
            f"got {path!r} (on object stores set REGISTRY_SYNC_DIR during "
            f"the Spark run instead)"
        )
    return path


def sync_registry_manifests(
    registry_dir,
    base_path=BASE_PATH,
    snapshot_date=SNAPSHOT_DATE,
    families=FAMILIES,
) -> dict:
    """Copy {base_path}/{family}/{snapshot_date}/manifest.json for each
    family to {registry_dir}/{registry-family}.json through
    FAMILY_REGISTRY_MAPPING — the files drift.py's
    DirectoryManifestProvider loads from $DRIFT_MANIFEST_DIR.

    Spark-free, local/file:// paths only. Missing family manifests are
    skipped with a warning (graceful-degradation pattern: a family that
    never landed must not block the others). Returns
    {snapshot_family: written_path}."""
    registry_dir = _local_path(registry_dir)
    base_path = _local_path(base_path.rstrip("/"))
    os.makedirs(registry_dir, exist_ok=True)
    written = {}
    for family in families:
        registry_family = FAMILY_REGISTRY_MAPPING.get(family)
        if registry_family is None:
            print(f"[training-snapshot] WARNING: no registry mapping for "
                  f"{family!r}; skipped")
            continue
        src = os.path.join(base_path, family, str(snapshot_date), "manifest.json")
        try:
            with open(src, "r", encoding="utf-8") as fh:
                snapshot_manifest = json.load(fh)
        except FileNotFoundError:
            print(f"[training-snapshot] WARNING: no manifest at {src}; "
                  f"skipped (family not snapshotted?)")
            continue
        reg = registry_manifest(snapshot_manifest)
        dst = os.path.join(registry_dir, f"{registry_family}.json")
        with open(dst, "w", encoding="utf-8") as fh:
            fh.write(manifest_json(reg))
        written[family] = dst
        print(f"[training-snapshot] synced {src} -> {dst}")
    return written


def manifest_json(manifest: dict) -> str:
    """Canonical serialization: sorted keys, stable formatting."""
    return json.dumps(manifest, indent=2, sort_keys=True)


# ---------------------------------------------------------------------------
# Schemas (Spark) — mirror the pure row builders field-for-field
# ---------------------------------------------------------------------------

if T is not None:
    NODE_SOURCE_SCHEMA = T.StructType(
        [
            T.StructField("snapshot_date", T.DateType()),
            T.StructField("tenant_id", T.StringType()),
            T.StructField("label", T.StringType()),
            T.StructField("node_id", T.StringType()),
            T.StructField("in_degree", T.IntegerType()),
            T.StructField("out_degree", T.IntegerType()),
            T.StructField("consent_marketing", T.BooleanType()),
            T.StructField("quarantine", T.BooleanType()),
            T.StructField("bookings_total", T.LongType()),
            T.StructField("ltv_cents", T.LongType()),
            T.StructField("no_show_rate", T.DoubleType()),
            T.StructField("propensity_show", T.DoubleType()),
            T.StructField("propensity_convert", T.DoubleType()),
            T.StructField("channel_of_first_touch", T.StringType()),
            T.StructField("last_active_at", T.TimestampType()),
        ]
    )
    EDGE_SOURCE_SCHEMA = T.StructType(
        [
            T.StructField("snapshot_date", T.DateType()),
            T.StructField("tenant_id", T.StringType()),
            T.StructField("edge_type", T.StringType()),
            T.StructField("src_label", T.StringType()),
            T.StructField("src_id", T.StringType()),
            T.StructField("dst_label", T.StringType()),
            T.StructField("dst_id", T.StringType()),
            T.StructField("weight", T.DoubleType()),
            T.StructField("edge_at", T.TimestampType()),
        ]
    )
    CAC_SOURCE_SCHEMA = T.StructType(
        [
            T.StructField("event_id", T.StringType()),
            T.StructField("tenant_id", T.StringType()),
            T.StructField("entity_type", T.StringType()),
            T.StructField("entity_id", T.StringType()),
            T.StructField("event_name", T.StringType()),
            T.StructField("event_ts", T.TimestampType()),
            T.StructField("channel", T.StringType()),
            T.StructField("campaign_id", T.StringType()),
            T.StructField("lga_id", T.LongType()),
            T.StructField("amount_ngn", T.DoubleType()),
            T.StructField("idempotency_key", T.StringType()),
        ]
    )
    LABELS_SCHEMA = T.StructType(
        [
            T.StructField("entity_id", T.StringType()),
            T.StructField("scenario", T.StringType()),
            T.StructField("fraud", T.BooleanType()),
        ]
    )
    FRAUD_SCHEMA = T.StructType(
        [
            T.StructField("snapshot_date", T.DateType()),
            T.StructField("tenant_id", T.StringType()),
            T.StructField("person_id", T.StringType()),
            T.StructField("in_degree", T.IntegerType()),
            T.StructField("out_degree", T.IntegerType()),
            T.StructField("bookings_total", T.LongType()),
            T.StructField("ltv_cents", T.LongType()),
            T.StructField("no_show_rate", T.DoubleType()),
            T.StructField("propensity_show", T.DoubleType()),
            T.StructField("propensity_convert", T.DoubleType()),
            T.StructField("consent_marketing", T.BooleanType()),
            T.StructField("quarantine", T.BooleanType()),
            T.StructField("channel_of_first_touch", T.StringType()),
            T.StructField("label_fraud", T.BooleanType()),
            T.StructField("label_scenario", T.StringType()),
        ]
    )
    CREDIT_SCHEMA = T.StructType(
        [
            T.StructField("snapshot_date", T.DateType()),
            T.StructField("tenant_id", T.StringType()),
            T.StructField("person_id", T.StringType()),
            T.StructField("bookings_total", T.LongType()),
            T.StructField("ltv_cents", T.LongType()),
            T.StructField("avg_booking_cents", T.DoubleType()),
            T.StructField("no_show_rate", T.DoubleType()),
            T.StructField("consent_marketing", T.BooleanType()),
            T.StructField("propensity_show", T.DoubleType()),
            T.StructField("propensity_convert", T.DoubleType()),
            T.StructField("channel_of_first_touch", T.StringType()),
            T.StructField("first_txn_ngn", T.DoubleType()),
            T.StructField("first_txn_at", T.TimestampType()),
        ]
    )


# ---------------------------------------------------------------------------
# Inputs (Spark) — graceful degradation on every source
# ---------------------------------------------------------------------------


def read_table_or_empty(spark, table, schema, tag):
    """Iceberg table reader; a missing/unreadable table logs a warning and
    yields an empty contract frame (cac_analytics.py pattern)."""
    try:
        df = spark.table(table)
    except Exception as exc:  # source not landed yet
        print(f"[training-snapshot] WARNING: {tag} unreadable at {table} "
              f"({exc.__class__.__name__}); using empty input")
        return spark.createDataFrame([], schema)
    out = df
    for field in schema.fields:
        if field.name not in out.columns:
            out = out.withColumn(field.name, F.lit(None).cast(field.dataType))
    return out.select(*[f.name for f in schema.fields])


def read_parquet_or_empty(spark, path, schema, tag):
    """Parquet extract reader; missing path -> warning + empty frame."""
    try:
        return spark.read.schema(schema).parquet(path)
    except Exception as exc:  # extract not landed yet — labels optional
        print(f"[training-snapshot] WARNING: {tag} unreadable at {path} "
              f"({exc.__class__.__name__}); using empty input")
        return spark.createDataFrame([], schema)


# ---------------------------------------------------------------------------
# Family transforms (Spark) — mirror the pure row builders
# ---------------------------------------------------------------------------


def fraud_features_df(nodes, labels) -> "DataFrame":
    persons = nodes.filter(
        F.col("tenant_id").isNotNull()
        & F.col("node_id").isNotNull()
        & (F.col("label") == "Person")
    ).select(
        "snapshot_date", "tenant_id",
        F.col("node_id").alias("person_id"),
        "in_degree", "out_degree", "bookings_total", "ltv_cents",
        "no_show_rate", "propensity_show", "propensity_convert",
        "consent_marketing", "quarantine", "channel_of_first_touch",
    )
    labs = labels.select(
        F.col("entity_id").alias("label_entity_id"),
        F.col("fraud").alias("label_fraud"),
        F.col("scenario").alias("label_scenario"),
    )
    return persons.join(
        labs, persons["person_id"] == labs["label_entity_id"], "left"
    ).select(*[f.name for f in FRAUD_SCHEMA.fields])


def credit_features_df(nodes, cac) -> "DataFrame":
    persons = nodes.filter(
        F.col("tenant_id").isNotNull()
        & F.col("node_id").isNotNull()
        & (F.col("label") == "Person")
    ).select(
        "snapshot_date", "tenant_id",
        F.col("node_id").alias("person_id"),
        "bookings_total", "ltv_cents", "no_show_rate", "consent_marketing",
        "propensity_show", "propensity_convert", "channel_of_first_touch",
    ).withColumn(
        "avg_booking_cents",
        F.when(
            F.col("bookings_total").isNotNull()
            & (F.col("bookings_total") > 0)
            & F.col("ltv_cents").isNotNull(),
            F.col("ltv_cents") / F.col("bookings_total"),
        ),
    )
    first_txn = (
        cac.filter(F.lower(F.trim(F.col("event_name"))) == FIRST_TXN_EVENT)
        .filter(F.col("entity_id").isNotNull())
        .groupBy("entity_id")
        .agg(
            F.min_by("amount_ngn", "event_ts").alias("first_txn_ngn"),
            F.min("event_ts").alias("first_txn_at"),
        )
    )
    return persons.join(
        first_txn, persons["person_id"] == first_txn["entity_id"], "left"
    ).select(*[f.name for f in CREDIT_SCHEMA.fields])


def gnn_nodes_df(nodes) -> "DataFrame":
    return nodes.filter(
        F.col("tenant_id").isNotNull() & F.col("node_id").isNotNull()
    ).select(*[f.name for f in NODE_SOURCE_SCHEMA.fields])


def gnn_edges_df(edges) -> "DataFrame":
    return edges.filter(
        F.col("tenant_id").isNotNull()
        & F.col("src_id").isNotNull()
        & F.col("dst_id").isNotNull()
    ).select(*[f.name for f in EDGE_SOURCE_SCHEMA.fields])


# ---------------------------------------------------------------------------
# Stats + manifest (Spark)
# ---------------------------------------------------------------------------


def spark_feature_stats(df, numeric_fields) -> dict:
    """One-pass {feature: {count, mean, std, min, max}} over numeric columns;
    mirrors compute_feature_stats (stddev = sample, NULL stats dropped)."""
    aggs = []
    for field in numeric_fields:
        if field not in df.columns:
            continue
        aggs.extend(
            [
                F.count(F.col(field)).alias(f"{field}__count"),
                F.mean(F.col(field)).alias(f"{field}__mean"),
                F.stddev(F.col(field)).alias(f"{field}__std"),
                F.min(F.col(field)).alias(f"{field}__min"),
                F.max(F.col(field)).alias(f"{field}__max"),
            ]
        )
    if not aggs:
        return {}
    row = df.agg(*aggs).collect()[0].asDict()
    stats = {}
    for field in numeric_fields:
        count = row.get(f"{field}__count")
        if not count:
            continue
        stats[field] = {
            "count": int(count),
            "mean": row.get(f"{field}__mean"),
            "std": row.get(f"{field}__std"),
            "min": row.get(f"{field}__min"),
            "max": row.get(f"{field}__max"),
        }
    return stats


def spark_feature_histograms(df, numeric_fields, bins: int = HISTOGRAM_BINS) -> dict:
    """{feature: {histogram: {edges, counts}}} over numeric columns;
    mirrors compute_feature_histograms. Two deterministic passes: min/max
    for the edges (same histogram_edges helper as the pure path), then one
    bucketed-count pass with EXACTLY the drift.py binning semantics
    (bin 0 = (-inf, e1), interior bins [e_i, e_{i+1}), last bin
    [e_{N-1}, +inf); NULLs never count). Empty features are omitted."""
    stats = spark_feature_stats(df, numeric_fields)
    edges_by_field = {
        field: histogram_edges(s["min"], s["max"], bins)
        for field, s in stats.items()
    }
    aggs = []
    for field, edges in edges_by_field.items():
        col = F.col(field)
        for i in range(len(edges) - 1):
            if i == 0:
                cond = col < F.lit(edges[1])
            elif i == len(edges) - 2:
                cond = col >= F.lit(edges[-2])
            else:
                cond = (col >= F.lit(edges[i])) & (col < F.lit(edges[i + 1]))
            aggs.append(
                F.sum(F.when(cond, 1).otherwise(0)).alias(f"{field}__bin{i}")
            )
    if not aggs:
        return {}
    row = df.agg(*aggs).collect()[0].asDict()
    out = {}
    for field, edges in edges_by_field.items():
        counts = [int(row.get(f"{field}__bin{i}") or 0)
                  for i in range(len(edges) - 1)]
        out[field] = {"histogram": {"edges": edges, "counts": counts}}
    return out


def write_parquet(df, path) -> None:
    """Parquet dataset partitioned by tenant_id, overwrite for idempotent
    re-runs of the same snapshot_date."""
    df.coalesce(1).write.mode("overwrite").partitionBy("tenant_id").parquet(path)


def write_manifest_file(spark, path: str, manifest: dict) -> None:
    """manifest.json via the Hadoop FileSystem API (works on s3a:// MinIO
    and local fs alike; single small object, no Spark writer overhead)."""
    sc = spark.sparkContext
    jvm = sc._gateway.jvm
    conf = sc._jsc.hadoopConfiguration()
    jpath = jvm.org.apache.hadoop.fs.Path(path)
    fs = jpath.getFileSystem(conf)
    parent = jpath.getParent()
    if not fs.exists(parent):
        fs.mkdirs(parent)
    out = fs.create(jpath, True)
    try:
        out.write(bytearray(manifest_json(manifest).encode("utf-8")))
    finally:
        out.close()


# ---------------------------------------------------------------------------
# Spark session + main
# ---------------------------------------------------------------------------


def _merged_packages() -> str:
    merged: list[str] = []
    for pkg in os.getenv("SPARK_JARS_PACKAGES", "").split(",") + ICEBERG_PACKAGES:
        pkg = pkg.strip()
        if pkg and pkg not in merged:
            merged.append(pkg)
    return ",".join(merged)


def build_spark() -> "SparkSession":
    """Spark session wired to the Iceberg REST catalog + MinIO (S3FileIO)
    for table reads and s3a for raw parquet/manifest writes
    (graph_export.py pattern + fs.s3a for the training paths)."""
    s3_endpoint = os.getenv("S3_ENDPOINT", "http://minio:9000")
    s3_key = os.getenv("AWS_ACCESS_KEY_ID", "minioadmin")
    s3_secret = os.getenv("AWS_SECRET_ACCESS_KEY", "minioadmin")
    return (
        SparkSession.builder.appName("opendesk-training-snapshot")
        .config("spark.jars.packages", _merged_packages())
        .config("spark.sql.catalog.iceberg", "org.apache.iceberg.spark.SparkCatalog")
        .config("spark.sql.catalog.iceberg.type", "rest")
        .config(
            "spark.sql.catalog.iceberg.uri",
            os.getenv("ICEBERG_REST_URI", "http://iceberg-rest:8181"),
        )
        .config("spark.sql.catalog.iceberg.warehouse", "s3://lake/warehouse")
        .config("spark.sql.catalog.iceberg.io-impl", "org.apache.iceberg.aws.s3.S3FileIO")
        .config("spark.sql.catalog.iceberg.s3.endpoint", s3_endpoint)
        .config("spark.sql.catalog.iceberg.s3.path-style-access", "true")
        .config("spark.sql.catalog.iceberg.s3.access-key-id", s3_key)
        .config("spark.sql.catalog.iceberg.s3.secret-access-key", s3_secret)
        .config("spark.sql.iceberg.handle-timestamp-without-timezone", "true")
        .config("spark.hadoop.fs.s3a.impl", "org.apache.hadoop.fs.s3a.S3AFileSystem")
        .config("spark.hadoop.fs.s3a.endpoint", s3_endpoint)
        .config("spark.hadoop.fs.s3a.access.key", s3_key)
        .config("spark.hadoop.fs.s3a.secret.key", s3_secret)
        .config("spark.hadoop.fs.s3a.path.style.access", "true")
        .getOrCreate()
    )


def _family_path(family: str, snapshot_date: str, sub: str = "") -> str:
    base = f"{BASE_PATH}/{family}/{snapshot_date}/"
    return f"{base}{sub}" if sub else base


def main(registry_sync_dir: str | None = None) -> None:
    registry_sync_dir = registry_sync_dir or os.getenv("REGISTRY_SYNC_DIR")
    spark = build_spark()

    def emit(family, snapshot_manifest):
        """Write the snapshot manifest and, when a registry sync dir is
        configured, the translated $DIR/<registry-family>.json (drift.py
        contract) next to it via the Hadoop FS writer (s3a/local alike)."""
        write_manifest_file(
            spark, f"{_family_path(family, SNAPSHOT_DATE)}manifest.json",
            snapshot_manifest)
        if registry_sync_dir:
            dst = f"{registry_sync_dir.rstrip('/')}/{FAMILY_REGISTRY_MAPPING[family]}.json"
            write_manifest_file(spark, dst, registry_manifest(snapshot_manifest))
            print(f"[training-snapshot] synced registry manifest {dst}")

    try:
        nodes = read_table_or_empty(spark, NODES_TABLE, NODE_SOURCE_SCHEMA, "node features")
        edges = read_table_or_empty(spark, EDGES_TABLE, EDGE_SOURCE_SCHEMA, "edge features")
        cac = read_table_or_empty(spark, CAC_TABLE, CAC_SOURCE_SCHEMA, "cac events")
        labels = read_parquet_or_empty(spark, LABELS_PATH, LABELS_SCHEMA, "A1 labels")

        nodes_src = {"kind": "table", "path": NODES_TABLE}
        edges_src = {"kind": "table", "path": EDGES_TABLE}
        cac_src = {"kind": "table", "path": CAC_TABLE}
        labels_src = {"kind": "extract", "path": LABELS_PATH}

        # ---- fraud_features (person structural features + A1 labels join)
        fraud = fraud_features_df(nodes, labels).cache()
        fraud_path = _family_path("fraud_features", SNAPSHOT_DATE)
        write_parquet(fraud, fraud_path)
        fraud_manifest = build_manifest(
            "fraud_features", SNAPSHOT_DATE,
            [nodes_src, labels_src],
            {"rows": fraud.count()},
            spark_feature_stats(fraud, PERSON_NUMERIC_FEATURES),
            feature_histograms=spark_feature_histograms(
                fraud, PERSON_NUMERIC_FEATURES),
        )
        emit("fraud_features", fraud_manifest)
        print(f"[training-snapshot] wrote {fraud_manifest['row_counts']['rows']} "
              f"rows to {fraud_path}")

        # ---- credit_features (person financial aggregates + first_txn)
        credit = credit_features_df(nodes, cac).cache()
        credit_path = _family_path("credit_features", SNAPSHOT_DATE)
        write_parquet(credit, credit_path)
        credit_manifest = build_manifest(
            "credit_features", SNAPSHOT_DATE,
            [nodes_src, cac_src],
            {"rows": credit.count()},
            spark_feature_stats(credit, CREDIT_NUMERIC_FEATURES),
            feature_histograms=spark_feature_histograms(
                credit, CREDIT_NUMERIC_FEATURES),
        )
        emit("credit_features", credit_manifest)
        print(f"[training-snapshot] wrote {credit_manifest['row_counts']['rows']} "
              f"rows to {credit_path}")

        # ---- gnn_export (graph export pass-through: nodes + edges)
        gnn_n = gnn_nodes_df(nodes).cache()
        gnn_e = gnn_edges_df(edges).cache()
        gnn_path = _family_path("gnn_export", SNAPSHOT_DATE)
        write_parquet(gnn_n, _family_path("gnn_export", SNAPSHOT_DATE, "nodes/"))
        write_parquet(gnn_e, _family_path("gnn_export", SNAPSHOT_DATE, "edges/"))
        gnn_stats = {
            f"nodes.{k}": v
            for k, v in spark_feature_stats(gnn_n, PERSON_NUMERIC_FEATURES).items()
        }
        gnn_stats.update(
            {f"edges.{k}": v
             for k, v in spark_feature_stats(gnn_e, EDGE_NUMERIC_FEATURES).items()}
        )
        gnn_histograms = {
            f"nodes.{k}": v
            for k, v in spark_feature_histograms(
                gnn_n, PERSON_NUMERIC_FEATURES).items()
        }
        gnn_histograms.update(
            {f"edges.{k}": v
             for k, v in spark_feature_histograms(
                 gnn_e, EDGE_NUMERIC_FEATURES).items()}
        )
        gnn_manifest = build_manifest(
            "gnn_export", SNAPSHOT_DATE,
            [nodes_src, edges_src],
            {"nodes": gnn_n.count(), "edges": gnn_e.count()},
            gnn_stats,
            feature_histograms=gnn_histograms,
        )
        emit("gnn_export", gnn_manifest)
        print(f"[training-snapshot] wrote nodes={gnn_manifest['row_counts']['nodes']} "
              f"edges={gnn_manifest['row_counts']['edges']} to {gnn_path}")
    finally:
        spark.stop()


def _parse_args(argv=None):
    parser = argparse.ArgumentParser(
        description="Lakehouse -> versioned training snapshots + drift "
                    "reference manifests (SPEC-W33 A2, SPEC-W34 GF2).")
    parser.add_argument(
        "--registry-sync", metavar="DIR",
        default=os.getenv("REGISTRY_SYNC_DIR"),
        help="write drift-contract registry manifests DIR/<registry-family>"
             ".json (fraud-ml/credit-ml/graphsage) from the snapshot "
             "manifests; with --sync-only this is all that runs")
    parser.add_argument(
        "--sync-only", action="store_true",
        help="skip the Spark snapshot job; only sync registry manifests "
             "from EXISTING snapshot manifests (Spark-free, local/file:// "
             "paths only)")
    parser.add_argument(
        "--snapshot-base", default=BASE_PATH,
        help="base path of the snapshot tree for --sync-only "
             "(default: TRAINING_BASE_PATH or s3://lake/training/)")
    parser.add_argument(
        "--snapshot-date", default=SNAPSHOT_DATE,
        help="snapshot date dir for --sync-only "
             "(default: TRAINING_SNAPSHOT_DATE or today)")
    return parser.parse_args(argv)


if __name__ == "__main__":
    args = _parse_args()
    if args.sync_only:
        if not args.registry_sync:
            raise SystemExit("--sync-only requires --registry-sync DIR")
        sync_registry_manifests(
            args.registry_sync, base_path=args.snapshot_base,
            snapshot_date=args.snapshot_date)
    else:
        main(registry_sync_dir=args.registry_sync)
