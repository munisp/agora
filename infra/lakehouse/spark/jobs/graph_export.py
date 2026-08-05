"""graph_export — graph -> Iceberg feature export, GNN/ART seams (SPEC-W28 §1/§4, WS-D).

Write-side of the lakehouse <-> graph bi-direction. This wave delivers the
SEAMS only (SPEC-W28 §6: no GNN training, no ART training):

  * GNN seam (Phase 3): periodic graph snapshots land as Iceberg feature
    tables a future trainer can consume directly. Prediction write-back is
    defined as `Person.propensity_*` properties (graph-service applies them
    through the same enrichment path as graph_enrichment.py).
  * ART seam (Phase 4): outreach trajectories (send x outcome) land as a
    training table. Trajectory rows originate from notification sends x
    outcomes streamed through `opendesk.usage.events` (WS-C trajectory
    logging) into the bronze sink.

Inputs (ALL optional — graceful degradation, cac_analytics.py pattern):

  * NODE EXPORT (parquet on MinIO) — bulk node dump from FalkorDB.
    INPUT CONTRACT (TODO producer: graph-service `GET /internal/export/nodes`
    or a graph-sync periodic dump; until then the job logs a warning and
    writes empty tables). Path: env GRAPH_EXPORT_NODES_PATH
    (default s3://lake/extracts/graph_nodes/). Columns:
      snapshot_date date, tenant_id string, label string, node_id string,
      in_degree int, out_degree int,
      consent_marketing boolean, quarantine boolean,
      bookings_total bigint, ltv_cents bigint, no_show_rate double,
      propensity_show double, propensity_convert double,
      channel_of_first_touch string, last_active_at timestamp
    Only Person/Contact/Offering/Campaign node labels are expected; rows
    without tenant_id or node_id are dropped (compliance gate 1: tenant_id
    is mandatory on every node — feature rows inherit the invariant).
  * EDGE EXPORT (parquet on MinIO) — bulk edge dump, same producer seam.
    Path: env GRAPH_EXPORT_EDGES_PATH (default s3://lake/extracts/graph_edges/).
    Columns:
      snapshot_date date, tenant_id string, edge_type string,
      src_label string, src_id string, dst_label string, dst_id string,
      weight double, edge_at timestamp
  * iceberg.bronze.usage_events (env GRAPH_EXPORT_TRAJ_TABLE) — send x
    outcome rows emitted by WS-C trajectory logging. INPUT CONTRACT
    (columns used; extras ignored):
      tenant_id string, person_id string, campaign_id string,
      channel string, template_id string, sent_at timestamp,
      outcome string, outcome_at timestamp
    outcome vocabulary: delivered | replied | booked | no_show | opted_out
    (unknown outcomes are kept with reward NULL).

Outputs (Iceberg, Trino-visible, dynamic partition overwrite like the other
gold jobs; `iceberg.gold` namespace so dbt gold models can pass them
through — see infra/lakehouse/dbt/models/gold/graph_features.sql):

  * iceberg.gold.graph_node_features — one row per (snapshot_date, tenant_id,
    label, node_id); columns = node export contract verbatim. Partitioned by
    snapshot_date.
  * iceberg.gold.graph_edge_features — one row per (snapshot_date, tenant_id,
    edge_type, src_id, dst_id); columns = edge export contract verbatim.
    Partitioned by snapshot_date.
  * iceberg.gold.outreach_trajectories — one row per (tenant_id, person_id,
    campaign_id, sent_at) with the ART-shaped reward:
      day date, tenant_id string, person_id string, campaign_id string,
      channel string, template_id string, sent_at timestamp,
      outcome string, outcome_at timestamp,
      latency_minutes double, reward double
    reward = +1.0 booked | +0.5 replied | +0.1 delivered | -0.5 no_show |
             -1.0 opted_out | NULL unknown (documented shaping; the ART
             trainer may re-shape). Partitioned by day (= sent day).

Pure-function reference implementations (compute_reward and row hygiene)
are Spark-free at the top of this module; the Spark transforms mirror them.
The pyspark import is guarded so the module stays importable on driver-less
CI boxes (cac_analytics.py pattern).

Run (packages are injected via spark.jars.packages; no --packages needed):

  docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \\
    --master spark://spark-master:7077 \\
    /opt/spark-jobs/graph_export.py
"""

import os

try:  # driver-less CI boxes have no pyspark — pure functions stay importable.
    from pyspark.sql import DataFrame, SparkSession
    from pyspark.sql import functions as F
    from pyspark.sql import types as T
except ImportError:  # pragma: no cover - exercised implicitly on CI lint boxes
    DataFrame = SparkSession = None
    F = T = None

NODES_PATH = os.getenv("GRAPH_EXPORT_NODES_PATH", "s3://lake/extracts/graph_nodes/")
EDGES_PATH = os.getenv("GRAPH_EXPORT_EDGES_PATH", "s3://lake/extracts/graph_edges/")
TRAJ_TABLE = os.getenv("GRAPH_EXPORT_TRAJ_TABLE", "iceberg.bronze.usage_events")

TARGET_NAMESPACE = "iceberg.gold"
NODE_TABLE = "iceberg.gold.graph_node_features"
EDGE_TABLE = "iceberg.gold.graph_edge_features"
TRAJ_TARGET_TABLE = "iceberg.gold.outreach_trajectories"

# ART reward shaping (documented in the module header; NULL = unknown outcome
# — kept so the trainer sees the full exposure set, not just scored rows).
OUTCOME_REWARDS = {
    "booked": 1.0,
    "replied": 0.5,
    "delivered": 0.1,
    "no_show": -0.5,
    "opted_out": -1.0,
}

ICEBERG_PACKAGES = [
    "org.apache.iceberg:iceberg-spark-runtime-3.5_2.12:1.6.1",
    "org.apache.iceberg:iceberg-aws-bundle:1.6.1",
]

# ---------------------------------------------------------------------------
# Pure functions (Spark-free reference implementation — unit-tested)
# ---------------------------------------------------------------------------


def compute_reward(outcome):
    """ART reward for an outcome; None for unknown/missing outcomes."""
    if outcome is None:
        return None
    return OUTCOME_REWARDS.get(str(outcome).strip().lower())


def clean_feature_rows(rows, required=("tenant_id",)):
    """Hygiene filter shared by node/edge/trajectory rows: drop rows missing
    any required key (tenant_id is always required — compliance gate 1)."""
    out = []
    for row in rows:
        if any(row.get(key) is None for key in required):
            continue
        out.append(row)
    return out


def shape_trajectory_rows(event_rows):
    """Reference shaping for outreach_trajectories.

    event_rows: dicts with the trajectory input contract keys. Returns rows
    with day, latency_minutes and reward attached; rows without tenant_id /
    person_id / sent_at are dropped (unkeyable/timeless hygiene, same as
    silver_clean_bookings.py).
    """
    out = []
    for row in clean_feature_rows(event_rows, ("tenant_id", "person_id", "sent_at")):
        sent = row["sent_at"]
        outcome_at = row.get("outcome_at")
        latency = None
        if outcome_at is not None:
            latency = (outcome_at - sent).total_seconds() / 60.0
        out.append(
            {
                "day": sent.date() if hasattr(sent, "date") else sent,
                "tenant_id": row["tenant_id"],
                "person_id": row["person_id"],
                "campaign_id": row.get("campaign_id"),
                "channel": row.get("channel"),
                "template_id": row.get("template_id"),
                "sent_at": sent,
                "outcome": row.get("outcome"),
                "outcome_at": outcome_at,
                "latency_minutes": latency,
                "reward": compute_reward(row.get("outcome")),
            }
        )
    return out


if T is not None:
    NODE_SCHEMA = T.StructType(
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
    EDGE_SCHEMA = T.StructType(
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
    TRAJ_SCHEMA = T.StructType(
        [
            T.StructField("tenant_id", T.StringType()),
            T.StructField("person_id", T.StringType()),
            T.StructField("campaign_id", T.StringType()),
            T.StructField("channel", T.StringType()),
            T.StructField("template_id", T.StringType()),
            T.StructField("sent_at", T.TimestampType()),
            T.StructField("outcome", T.StringType()),
            T.StructField("outcome_at", T.TimestampType()),
        ]
    )
    TRAJ_COLS = [f.name for f in TRAJ_SCHEMA.fields]


# ---------------------------------------------------------------------------
# Inputs (Spark) — graceful degradation on every source
# ---------------------------------------------------------------------------


def _project(df, cols):
    """Keep only contract columns that actually exist (missing -> NULL)."""
    out = df
    for col in cols:
        if col not in out.columns:
            out = out.withColumn(col, F.lit(None))
    return out.select(*cols)


def read_extract_or_empty(spark, path, schema, tag):
    """Parquet extract reader; a missing path logs a warning and yields an
    empty contract frame (cac_analytics.py campaign-spend pattern)."""
    try:
        df = spark.read.schema(schema).parquet(path)
    except Exception as exc:  # export not landed yet — TODO producer
        print(f"[graph-export] WARNING: {tag} unreadable at {path} "
              f"({exc.__class__.__name__}); using empty input")
        return spark.createDataFrame([], schema)
    return df


def read_trajectories(spark) -> "DataFrame":
    try:
        df = spark.table(TRAJ_TABLE)
    except Exception as exc:  # bronze sink for usage events not landed yet
        print(f"[graph-export] WARNING: trajectory source unreadable at "
              f"{TRAJ_TABLE} ({exc.__class__.__name__}); using empty input")
        return spark.createDataFrame([], TRAJ_SCHEMA)
    return _project(df, TRAJ_COLS)


# ---------------------------------------------------------------------------
# Gold tables
# ---------------------------------------------------------------------------


def ensure_target_tables(spark) -> None:
    spark.sql(f"CREATE NAMESPACE IF NOT EXISTS {TARGET_NAMESPACE}")
    spark.sql(
        f"""
        CREATE TABLE IF NOT EXISTS {NODE_TABLE} (
            snapshot_date          DATE,
            tenant_id              STRING,
            label                  STRING,
            node_id                STRING,
            in_degree              INT,
            out_degree             INT,
            consent_marketing      BOOLEAN,
            quarantine             BOOLEAN,
            bookings_total         BIGINT,
            ltv_cents              BIGINT,
            no_show_rate           DOUBLE,
            propensity_show        DOUBLE,
            propensity_convert     DOUBLE,
            channel_of_first_touch STRING,
            last_active_at         TIMESTAMP
        ) USING iceberg
        PARTITIONED BY (snapshot_date)
        """
    )
    spark.sql(
        f"""
        CREATE TABLE IF NOT EXISTS {EDGE_TABLE} (
            snapshot_date DATE,
            tenant_id     STRING,
            edge_type     STRING,
            src_label     STRING,
            src_id        STRING,
            dst_label     STRING,
            dst_id        STRING,
            weight        DOUBLE,
            edge_at       TIMESTAMP
        ) USING iceberg
        PARTITIONED BY (snapshot_date)
        """
    )
    spark.sql(
        f"""
        CREATE TABLE IF NOT EXISTS {TRAJ_TARGET_TABLE} (
            day             DATE,
            tenant_id       STRING,
            person_id       STRING,
            campaign_id     STRING,
            channel         STRING,
            template_id     STRING,
            sent_at         TIMESTAMP,
            outcome         STRING,
            outcome_at      TIMESTAMP,
            latency_minutes DOUBLE,
            reward          DOUBLE
        ) USING iceberg
        PARTITIONED BY (day)
        """
    )


def compute_node_features(nodes: "DataFrame") -> "DataFrame":
    """Node feature rows: contract columns verbatim, hygiene-filtered."""
    return (
        nodes.filter(F.col("tenant_id").isNotNull() & F.col("node_id").isNotNull())
        .filter(F.col("snapshot_date").isNotNull())
        .select(*[f.name for f in NODE_SCHEMA.fields])
    )


def compute_edge_features(edges: "DataFrame") -> "DataFrame":
    """Edge feature rows: contract columns verbatim, hygiene-filtered."""
    return (
        edges.filter(
            F.col("tenant_id").isNotNull()
            & F.col("src_id").isNotNull()
            & F.col("dst_id").isNotNull()
        )
        .filter(F.col("snapshot_date").isNotNull())
        .select(*[f.name for f in EDGE_SCHEMA.fields])
    )


def compute_trajectories(traj: "DataFrame") -> "DataFrame":
    """Send x outcome rows shaped for ART: day partition, latency, reward."""
    reward_expr = F.create_map(
        *[item for kv in OUTCOME_REWARDS.items() for item in (F.lit(kv[0]), F.lit(kv[1]))]
    )
    return (
        traj.filter(
            F.col("tenant_id").isNotNull()
            & F.col("person_id").isNotNull()
            & F.col("sent_at").isNotNull()
        )
        .withColumn("day", F.to_date("sent_at"))
        .withColumn(
            "latency_minutes",
            (F.col("outcome_at").cast("long") - F.col("sent_at").cast("long")) / 60.0,
        )
        .withColumn(
            "reward",
            reward_expr[F.lower(F.trim(F.col("outcome")))],
        )
        .select(
            "day", "tenant_id", "person_id", "campaign_id", "channel",
            "template_id", "sent_at", "outcome", "outcome_at",
            "latency_minutes", "reward",
        )
    )


# ---------------------------------------------------------------------------
# Spark session + main
# ---------------------------------------------------------------------------


def _merged_packages() -> str:
    """Iceberg packages merged with any pre-existing packages config."""
    merged: list[str] = []
    for pkg in os.getenv("SPARK_JARS_PACKAGES", "").split(",") + ICEBERG_PACKAGES:
        pkg = pkg.strip()
        if pkg and pkg not in merged:
            merged.append(pkg)
    return ",".join(merged)


def build_spark() -> "SparkSession":
    """Spark session wired to the Iceberg REST catalog + MinIO (S3FileIO)
    (silver_clean_bookings.py pattern)."""
    return (
        SparkSession.builder.appName("opendesk-graph-export")
        .config("spark.jars.packages", _merged_packages())
        .config("spark.sql.catalog.iceberg", "org.apache.iceberg.spark.SparkCatalog")
        .config("spark.sql.catalog.iceberg.type", "rest")
        .config(
            "spark.sql.catalog.iceberg.uri",
            os.getenv("ICEBERG_REST_URI", "http://iceberg-rest:8181"),
        )
        .config("spark.sql.catalog.iceberg.warehouse", "s3://lake/warehouse")
        .config("spark.sql.catalog.iceberg.io-impl", "org.apache.iceberg.aws.s3.S3FileIO")
        .config(
            "spark.sql.catalog.iceberg.s3.endpoint",
            os.getenv("S3_ENDPOINT", "http://minio:9000"),
        )
        .config("spark.sql.catalog.iceberg.s3.path-style-access", "true")
        .config(
            "spark.sql.catalog.iceberg.s3.access-key-id",
            os.getenv("AWS_ACCESS_KEY_ID", "minioadmin"),
        )
        .config(
            "spark.sql.catalog.iceberg.s3.secret-access-key",
            os.getenv("AWS_SECRET_ACCESS_KEY", "minioadmin"),
        )
        .config("spark.sql.iceberg.handle-timestamp-without-timezone", "true")
        .getOrCreate()
    )


def main() -> None:
    spark = build_spark()
    try:
        ensure_target_tables(spark)

        nodes = compute_node_features(read_extract_or_empty(spark, NODES_PATH, NODE_SCHEMA, "node export"))
        nodes.writeTo(NODE_TABLE).overwritePartitions()
        print(f"[gold] wrote {nodes.count()} rows to {NODE_TABLE}")

        edges = compute_edge_features(read_extract_or_empty(spark, EDGES_PATH, EDGE_SCHEMA, "edge export"))
        edges.writeTo(EDGE_TABLE).overwritePartitions()
        print(f"[gold] wrote {edges.count()} rows to {EDGE_TABLE}")

        traj = compute_trajectories(read_trajectories(spark))
        traj.writeTo(TRAJ_TARGET_TABLE).overwritePartitions()
        print(f"[gold] wrote {traj.count()} rows to {TRAJ_TARGET_TABLE}")
    finally:
        spark.stop()


if __name__ == "__main__":
    main()
