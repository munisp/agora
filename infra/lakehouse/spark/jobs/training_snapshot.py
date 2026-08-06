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

manifest.json (schema_version training-manifest-v1): family, snapshot_date,
created_at, seed (env TRAINING_SEED, default 42 — recorded per I3 even
though this job itself is deterministic and does no sampling), source table
paths, row counts, and reference distribution stats per numeric feature
{count, mean, std, min, max} — the W33-C drift monitor (PSI/KS) compares
incoming feature distributions against these. std is the SAMPLE stddev
(Spark stddev, ddof=1); NULL when fewer than 2 non-null values.

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
) -> dict:
    """manifest.json builder (schema training-manifest-v1). created_at is
    injected so tests can pin it; defaults to now (UTC). Keys are sorted on
    serialization for byte-stable diffs."""
    if family not in FAMILIES:
        raise ValueError(f"unknown training family {family!r}")
    date.fromisoformat(str(snapshot_date))  # contract: YYYY-MM-DD
    return {
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
    }


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


def main() -> None:
    spark = build_spark()
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
        )
        write_manifest_file(spark, f"{fraud_path}manifest.json", fraud_manifest)
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
        )
        write_manifest_file(spark, f"{credit_path}manifest.json", credit_manifest)
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
        gnn_manifest = build_manifest(
            "gnn_export", SNAPSHOT_DATE,
            [nodes_src, edges_src],
            {"nodes": gnn_n.count(), "edges": gnn_e.count()},
            gnn_stats,
        )
        write_manifest_file(spark, f"{gnn_path}manifest.json", gnn_manifest)
        print(f"[training-snapshot] wrote nodes={gnn_manifest['row_counts']['nodes']} "
              f"edges={gnn_manifest['row_counts']['edges']} to {gnn_path}")
    finally:
        spark.stop()


if __name__ == "__main__":
    main()
