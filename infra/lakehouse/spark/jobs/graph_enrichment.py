"""graph_enrichment — nightly gold/silver -> graph enrichment (SPEC-W28 §2, WS-D).

Read-side of the lakehouse <-> graph bi-direction: recomputes per-Person
properties from the batch-verified lakehouse tables and emits them for
graph-sync to apply as FalkorDB `Person` node properties. The graph itself
stays event-sourced (graph-sync owns writes); this job is the idempotent,
replayable reconciliation/enrichment path — same batch-verified philosophy
as cac_analytics.py vs the realtime CAC rollups.

Reads (ALL optional at runtime — a missing/unreadable source logs a warning
and yields an empty partial input instead of failing the pipeline, same
graceful-degradation pattern as cac_analytics.py / geo_analytics.py):

  * iceberg.silver.booking_events (env GRAPH_ENRICH_BOOKINGS_TABLE) —
    deduped booking events. INPUT CONTRACT (columns used; extras ignored):
      tenant_id string, booking_id string, event_type string,
      status string, occurred_at timestamp,
      customer_phone_hash string (nullable), customer_id string (nullable),
      offering_id string (nullable), price_cents bigint (nullable),
      showed boolean (nullable)
    Person key: customer_phone_hash, falling back to customer_id (the graph
    stores phones hashed — SPEC-W28 §3 — so the lakehouse never needs raw
    PII here). Rows without any person key are dropped.
  * iceberg.cac_gold.daily_cac_by_channel (env GRAPH_ENRICH_CAC_TABLE) —
    the SPEC-W13 §4 gold table written by cac_analytics.py. Trailing-30-day
    mean CAC per (tenant_id, channel) is attached to Persons whose
    channel_of_first_touch matches (property `cac_channel_ngn_30d`).
  * iceberg.cac_gold.commission_recon (env GRAPH_ENRICH_COMMISSION_TABLE,
    OPTIONAL — W14 commission reconciliation gold table, may not exist yet).
    INPUT CONTRACT: tenant_id string, agent_id string, day date,
      earned_ngn double, paid_ngn double, mismatches bigint
    Aggregated per agent over the trailing 30 days; applied to Person nodes
    that represent agents (person_id == agent_id in the referral graph).
  * iceberg.bronze.cac_events (env GRAPH_ENRICH_LEADS_TABLE, OPTIONAL) —
    funnel events (SPEC-W13 §2 contract, see cac_analytics.py). Used for
    `channel_of_first_touch` (channel of the earliest `lead_created`) and
    `funnel_stage_max` per lead/customer entity.

Output — per-Person enrichment events (one JSON object per Kafka record):

  topic: env GRAPH_ENRICHMENT_TOPIC (default opendesk.graph.enrichment.v1)
  key:   "<tenant_id>:<person_id>"      (compaction/dedupe key)
  value: CloudEvents-1.0-style envelope consumed by graph-sync:

    {
      "specversion": "1.0",
      "type": "com.opendesk.graph.PersonEnrichment",
      "source": "spark/graph_enrichment",
      "id": "<tenant_id>:<person_id>:<snapshot_day>",   // idempotency key
      "tenantid": "<tenant_id>",
      "time": "<job run timestamp, RFC3339>",
      "data": {
        "tenant_id": "...", "person_id": "...",
        "snapshot_day": "YYYY-MM-DD",
        "properties": {
          "bookings_total":      int,    // deduped bookings, all time
          "bookings_showed":     int,
          "bookings_no_show":    int,
          "ltv_cents":           long,   // sum of price_cents (NULL-safe -> 0)
          "no_show_rate":        double, // NULL when bookings_total == 0
          "last_booking_at":     string, // RFC3339, NULL when never booked
          "channel_of_first_touch": string,   // from cac_events, NULL-safe
          "funnel_stage_max":    string,      // deepest funnel stage reached
          "cac_channel_ngn_30d": double,      // trailing-30d channel CAC
          "commission_earned_ngn_30d": double, // agents only, else absent
          "commission_paid_ngn_30d":   double, // agents only, else absent
          "recon_mismatches_30d":    int       // agents only, else absent
        }
      }
    }

  graph-sync applies `properties` via idempotent SET on the matching Person
  (MERGE on (tenant_id, person_id)); event `id` dedupe mirrors the W24
  consumer idempotency pattern. The same rows can also land as parquet
  (GRAPH_ENRICH_OUTPUT=both|file) at GRAPH_ENRICH_OUTPUT_PATH for replay.

Pure-function reference implementations of the aggregation math
(aggregate_person_bookings / aggregate_channel_cac / build_enrichment_event
and helpers) live at the top of this module and are Spark-free so they can
be unit-tested without a Spark session; the Spark transforms below mirror
them expression-for-expression. The pyspark import is guarded so this
module stays importable on driver-less CI boxes (cac_analytics.py pattern).

Run (packages are injected via spark.jars.packages; no --packages needed):

  docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \\
    --master spark://spark-master:7077 \\
    /opt/spark-jobs/graph_enrichment.py
"""

import json
import os
from datetime import UTC, date, datetime, timedelta

try:  # driver-less CI boxes have no pyspark — pure functions stay importable.
    from pyspark.sql import DataFrame, SparkSession
    from pyspark.sql import functions as F
    from pyspark.sql import types as T
except ImportError:  # pragma: no cover - exercised implicitly on CI lint boxes
    DataFrame = SparkSession = None
    F = T = None

BOOKINGS_TABLE = os.getenv(
    "GRAPH_ENRICH_BOOKINGS_TABLE", "iceberg.silver.booking_events"
)
CAC_TABLE = os.getenv(
    "GRAPH_ENRICH_CAC_TABLE", "iceberg.cac_gold.daily_cac_by_channel"
)
COMMISSION_TABLE = os.getenv(
    "GRAPH_ENRICH_COMMISSION_TABLE", "iceberg.cac_gold.commission_recon"
)
LEADS_TABLE = os.getenv("GRAPH_ENRICH_LEADS_TABLE", "iceberg.bronze.cac_events")

ENRICHMENT_TOPIC = os.getenv(
    "GRAPH_ENRICHMENT_TOPIC", "opendesk.graph.enrichment.v1"
)
KAFKA_BROKERS = os.getenv("KAFKA_BROKERS", "kafka:9092")
# kafka | file | both — `file` is the replay/debug path (no Kafka package
# needed); `both` emits to Kafka AND archives the parquet extract.
OUTPUT_MODE = os.getenv("GRAPH_ENRICH_OUTPUT", "kafka").lower()
OUTPUT_PATH = os.getenv(
    "GRAPH_ENRICH_OUTPUT_PATH", "s3://lake/extracts/graph_enrichment/"
)

CAC_WINDOW_DAYS = int(os.getenv("GRAPH_ENRICH_CAC_WINDOW_DAYS", "30"))
COMMISSION_WINDOW_DAYS = int(os.getenv("GRAPH_ENRICH_COMMISSION_WINDOW_DAYS", "30"))

EVENT_TYPE = "com.opendesk.graph.PersonEnrichment"
EVENT_SOURCE = "spark/graph_enrichment"

# SPEC-W13 §2 funnel ordering (deepest stage reached wins).
FUNNEL_STAGE_ORDER = (
    "lead_created", "contacted", "opted_in", "qualified", "converted",
    "first_txn", "lost",
)

ICEBERG_PACKAGES = [
    "org.apache.iceberg:iceberg-spark-runtime-3.5_2.12:1.6.1",
    "org.apache.iceberg:iceberg-aws-bundle:1.6.1",
]
KAFKA_PACKAGE = os.getenv(
    "GRAPH_ENRICH_KAFKA_PACKAGE", "org.apache.spark:spark-sql-kafka-0-10_2.12:3.5.1"
)

# ---------------------------------------------------------------------------
# Pure aggregation math (Spark-free reference implementation — unit-tested)
# ---------------------------------------------------------------------------


def person_key(row) -> str | None:
    """Person node key: hashed phone first (graph stores no raw PII), then
    customer_id. None when neither is present (row is dropped)."""
    for col in ("customer_phone_hash", "customer_id"):
        value = row.get(col)
        if value is not None and str(value).strip():
            return str(value).strip()
    return None


def aggregate_person_bookings(event_rows):
    """Per-Person booking aggregates from deduped booking events.

    event_rows: dicts with the silver contract keys (see module header).
    Returns {person_id: {bookings_total, bookings_showed, bookings_no_show,
    ltv_cents, no_show_rate, last_booking_at}} — one entry per person key.
    Rows without tenant_id/person key are dropped (hygiene, same as
    silver_clean_bookings.py).
    """
    people: dict[str, dict] = {}
    for row in event_rows:
        if row.get("tenant_id") is None:
            continue
        pid = person_key(row)
        if pid is None:
            continue
        bucket = people.setdefault(
            pid,
            {
                "bookings_total": 0,
                "bookings_showed": 0,
                "bookings_no_show": 0,
                "ltv_cents": 0,
                "last_booking_at": None,
            },
        )
        bucket["bookings_total"] += 1
        status = str(row.get("status") or "").strip().lower()
        showed = row.get("showed")
        if showed is True or status in ("completed", "showed"):
            bucket["bookings_showed"] += 1
        elif showed is False or status == "no_show":
            bucket["bookings_no_show"] += 1
        price = row.get("price_cents")
        if price is not None:
            bucket["ltv_cents"] += int(price)
        ts = row.get("occurred_at")
        if ts is not None and (
            bucket["last_booking_at"] is None or ts > bucket["last_booking_at"]
        ):
            bucket["last_booking_at"] = ts
    for bucket in people.values():
        total = bucket["bookings_total"]
        bucket["no_show_rate"] = (
            bucket["bookings_no_show"] / float(total) if total else None
        )
    return people


def aggregate_channel_cac(cac_rows, snapshot_day, window_days=CAC_WINDOW_DAYS):
    """Trailing-window mean CAC per (tenant_id, channel).

    cac_rows: dicts with the cac_gold.daily_cac_by_channel contract
    {day, tenant_id, channel, cac_ngn, ...}. NULL cac_ngn rows (spend but no
    conversions — undefined, not infinite; see cac_analytics.py) are excluded
    from the mean. Returns {(tenant_id, channel): mean_cac_ngn}.
    """
    window_start = snapshot_day - timedelta(days=window_days)
    sums: dict[tuple, list[float]] = {}
    for row in cac_rows:
        if row.get("tenant_id") is None or row.get("day") is None:
            continue
        cac = row.get("cac_ngn")
        if cac is None:
            continue
        day = row["day"]
        if day < window_start or day > snapshot_day:
            continue
        key = (row["tenant_id"], str(row.get("channel") or "unknown"))
        sums.setdefault(key, []).append(float(cac))
    return {key: sum(vals) / len(vals) for key, vals in sums.items() if vals}


def aggregate_commissions(recon_rows, snapshot_day, window_days=COMMISSION_WINDOW_DAYS):
    """Per-agent trailing-window commission totals (agents are Persons in the
    referral graph). Returns {agent_id: {commission_earned_ngn_30d,
    commission_paid_ngn_30d, recon_mismatches_30d}}."""
    window_start = snapshot_day - timedelta(days=window_days)
    agents: dict[str, dict] = {}
    for row in recon_rows:
        if row.get("tenant_id") is None or row.get("agent_id") is None:
            continue
        day = row.get("day")
        if day is not None and (day < window_start or day > snapshot_day):
            continue
        bucket = agents.setdefault(
            str(row["agent_id"]),
            {
                "commission_earned_ngn_30d": 0.0,
                "commission_paid_ngn_30d": 0.0,
                "recon_mismatches_30d": 0,
            },
        )
        bucket["commission_earned_ngn_30d"] += float(row.get("earned_ngn") or 0.0)
        bucket["commission_paid_ngn_30d"] += float(row.get("paid_ngn") or 0.0)
        bucket["recon_mismatches_30d"] += int(row.get("mismatches") or 0)
    return agents


def funnel_stage_max(stage_rows):
    """Deepest funnel stage + channel of first touch per lead/customer entity.

    stage_rows: dicts with the SPEC-W13 §2 cac_events contract
    {tenant_id, entity_id, event_name, event_ts, channel}. Returns
    {entity_id: (channel_of_first_touch, funnel_stage_max)}.
    """
    order = {name: i for i, name in enumerate(FUNNEL_STAGE_ORDER)}
    out: dict[str, dict] = {}
    for row in stage_rows:
        if row.get("tenant_id") is None or row.get("entity_id") is None:
            continue
        name = str(row.get("event_name") or "").strip().lower()
        if name not in order:
            continue
        bucket = out.setdefault(
            str(row["entity_id"]),
            {"stage": None, "stage_rank": -1, "first_ts": None, "first_channel": None},
        )
        if order[name] > bucket["stage_rank"]:
            bucket["stage"], bucket["stage_rank"] = name, order[name]
        ts = row.get("event_ts")
        if name == "lead_created" and ts is not None and (
            bucket["first_ts"] is None or ts < bucket["first_ts"]
        ):
            bucket["first_ts"] = ts
            bucket["first_channel"] = row.get("channel")
    return {
        pid: (b["first_channel"], b["stage"]) for pid, b in out.items()
    }


def build_enrichment_event(tenant_id, pid, properties, snapshot_day, run_ts):
    """One CloudEvents-style enrichment record (Kafka value, JSON string).

    `id` is the idempotency key: re-runs of the same snapshot day produce
    the same id, and graph-sync's event_id dedupe makes replay a no-op.
    """
    return json.dumps(
        {
            "specversion": "1.0",
            "type": EVENT_TYPE,
            "source": EVENT_SOURCE,
            "id": f"{tenant_id}:{pid}:{snapshot_day.isoformat()}",
            "tenantid": tenant_id,
            "time": run_ts.isoformat(),
            "data": {
                "tenant_id": tenant_id,
                "person_id": pid,
                "snapshot_day": snapshot_day.isoformat(),
                "properties": properties,
            },
        },
        sort_keys=True,
    )


if T is not None:
    BOOKINGS_SCHEMA = T.StructType(
        [
            T.StructField("tenant_id", T.StringType()),
            T.StructField("booking_id", T.StringType()),
            T.StructField("event_type", T.StringType()),
            T.StructField("status", T.StringType()),
            T.StructField("occurred_at", T.TimestampType()),
            T.StructField("customer_phone_hash", T.StringType()),
            T.StructField("customer_id", T.StringType()),
            T.StructField("offering_id", T.StringType()),
            T.StructField("price_cents", T.LongType()),
            T.StructField("showed", T.BooleanType()),
        ]
    )
    CAC_SCHEMA = T.StructType(
        [
            T.StructField("day", T.DateType()),
            T.StructField("tenant_id", T.StringType()),
            T.StructField("channel", T.StringType()),
            T.StructField("cac_ngn", T.DoubleType()),
        ]
    )
    COMMISSION_SCHEMA = T.StructType(
        [
            T.StructField("tenant_id", T.StringType()),
            T.StructField("agent_id", T.StringType()),
            T.StructField("day", T.DateType()),
            T.StructField("earned_ngn", T.DoubleType()),
            T.StructField("paid_ngn", T.DoubleType()),
            T.StructField("mismatches", T.LongType()),
        ]
    )
    LEADS_SCHEMA = T.StructType(
        [
            T.StructField("tenant_id", T.StringType()),
            T.StructField("entity_id", T.StringType()),
            T.StructField("event_name", T.StringType()),
            T.StructField("event_ts", T.TimestampType()),
            T.StructField("channel", T.StringType()),
        ]
    )
    # Contract columns projected out of the bronze/silver inputs before the
    # aggregation joins (extras in the physical tables are ignored).
    BOOKINGS_COLS = [f.name for f in BOOKINGS_SCHEMA.fields]
    CAC_COLS = [f.name for f in CAC_SCHEMA.fields]
    COMMISSION_COLS = [f.name for f in COMMISSION_SCHEMA.fields]
    LEADS_COLS = [f.name for f in LEADS_SCHEMA.fields]


# ---------------------------------------------------------------------------
# Inputs (Spark) — every reader degrades to an empty contract frame
# ---------------------------------------------------------------------------


def _project(df, cols):
    """Keep only contract columns that actually exist (missing -> NULL)."""
    out = df
    for col in cols:
        if col not in out.columns:
            out = out.withColumn(col, F.lit(None))
    return out.select(*cols)


def read_table_or_empty(spark, table, schema, cols, tag):
    """Graceful-degradation table reader (cac_analytics.py extract pattern)."""
    try:
        df = spark.table(table)
    except Exception as exc:  # table not landed yet — TODO producer optional
        print(f"[graph-enrich] WARNING: {tag} unreadable at {table} "
              f"({exc.__class__.__name__}); using empty input")
        return spark.createDataFrame([], schema)
    return _project(df, cols)


def read_bookings(spark) -> "DataFrame":
    df = read_table_or_empty(spark, BOOKINGS_TABLE, BOOKINGS_SCHEMA, BOOKINGS_COLS, "bookings")
    return (
        df.filter(F.col("tenant_id").isNotNull())
        .filter(
            F.col("customer_phone_hash").isNotNull()
            | F.col("customer_id").isNotNull()
        )
        .withColumn(
            "person_id",
            F.coalesce(F.col("customer_phone_hash"), F.col("customer_id")),
        )
        .filter(F.length(F.trim("person_id")) > 0)
    )


def read_cac(spark) -> "DataFrame":
    return read_table_or_empty(spark, CAC_TABLE, CAC_SCHEMA, CAC_COLS, "channel CAC")


def read_commissions(spark) -> "DataFrame":
    return read_table_or_empty(
        spark, COMMISSION_TABLE, COMMISSION_SCHEMA, COMMISSION_COLS, "commission recon"
    )


def read_leads(spark) -> "DataFrame":
    return read_table_or_empty(spark, LEADS_TABLE, LEADS_SCHEMA, LEADS_COLS, "funnel events")


# ---------------------------------------------------------------------------
# Aggregation (Spark) — mirrors the pure functions expression-for-expression
# ---------------------------------------------------------------------------


def compute_enrichment_rows(spark, snapshot_day) -> "DataFrame":
    """One row per (tenant_id, person_id) with the full property set, ready
    for Kafka/file serialization."""
    bookings = read_bookings(spark)
    booking_agg = (
        bookings.groupBy("tenant_id", "person_id")
        .agg(
            F.count("*").alias("bookings_total"),
            F.sum(
                F.when(
                    (F.col("showed") == F.lit(True))
                    | F.lower(F.trim(F.col("status"))).isin("completed", "showed"),
                    1,
                ).otherwise(0)
            ).alias("bookings_showed"),
            F.sum(
                F.when(
                    (F.col("showed") == F.lit(False))
                    | (F.lower(F.trim(F.col("status"))) == "no_show"),
                    1,
                ).otherwise(0)
            ).alias("bookings_no_show"),
            F.sum(F.coalesce(F.col("price_cents"), F.lit(0))).alias("ltv_cents"),
            F.max("occurred_at").alias("last_booking_at"),
        )
        .withColumn(
            "no_show_rate",
            F.col("bookings_no_show") / F.nullif(F.col("bookings_total"), F.lit(0)),
        )
    )

    # Funnel stage / first-touch channel per lead entity (cac_events contract).
    leads = read_leads(spark)
    stage_rank_expr = F.expr(
        "CASE lower(trim(event_name)) "
        + " ".join(
            f"WHEN '{name}' THEN {rank}"
            for rank, name in enumerate(FUNNEL_STAGE_ORDER)
        )
        + " ELSE -1 END"
    )
    lead_agg = (
        leads.filter(F.col("tenant_id").isNotNull() & F.col("entity_id").isNotNull())
        .withColumn("_rank", stage_rank_expr)
        .filter(F.col("_rank") >= 0)
        .groupBy("tenant_id", F.col("entity_id").alias("person_id"))
        .agg(
            F.max("_rank").alias("_stage_rank"),
            F.min(
                F.when(F.lower(F.trim("event_name")) == "lead_created", F.col("event_ts"))
            ).alias("_first_ts"),
        )
    )
    first_touch = (
        leads.filter(F.lower(F.trim("event_name")) == "lead_created")
        .join(
            lead_agg.select("tenant_id", "person_id", "_first_ts"),
            ["tenant_id", "person_id"],
        )
        .filter(F.col("event_ts") == F.col("_first_ts"))
        .groupBy("tenant_id", "person_id")
        .agg(F.first("channel", ignorenulls=True).alias("channel_of_first_touch"))
    )
    lead_props = lead_agg.select("tenant_id", "person_id", "_stage_rank").join(
        first_touch, ["tenant_id", "person_id"], "left"
    )
    # Map stage rank back to the funnel vocabulary.
    stage_name_expr = F.expr(
        "CASE _stage_rank "
        + " ".join(
            f"WHEN {rank} THEN '{name}'"
            for rank, name in enumerate(FUNNEL_STAGE_ORDER)
        )
        + " END"
    )
    lead_props = lead_props.withColumn("funnel_stage_max", stage_name_expr).drop("_stage_rank")

    # Trailing-window channel CAC per (tenant, channel).
    window_start = F.date_sub(F.lit(snapshot_day), CAC_WINDOW_DAYS)
    cac = (
        read_cac(spark)
        .filter(F.col("cac_ngn").isNotNull())
        .filter(F.col("day").between(window_start, F.lit(snapshot_day)))
        .groupBy("tenant_id", "channel")
        .agg(F.avg("cac_ngn").alias("cac_channel_ngn_30d"))
    )

    # Trailing-window commission totals per agent (agents are Persons too).
    comm_window_start = F.date_sub(F.lit(snapshot_day), COMMISSION_WINDOW_DAYS)
    comms = (
        read_commissions(spark)
        .filter(F.col("tenant_id").isNotNull() & F.col("agent_id").isNotNull())
        .filter(
            F.col("day").isNull()
            | F.col("day").between(comm_window_start, F.lit(snapshot_day))
        )
        .groupBy("tenant_id", F.col("agent_id").alias("person_id"))
        .agg(
            F.sum(F.coalesce("earned_ngn", F.lit(0.0))).alias("commission_earned_ngn_30d"),
            F.sum(F.coalesce("paid_ngn", F.lit(0.0))).alias("commission_paid_ngn_30d"),
            F.sum(F.coalesce("mismatches", F.lit(0))).alias("recon_mismatches_30d"),
        )
    )

    # Union of all person keys seen in any source, then LEFT JOIN the props.
    persons = (
        booking_agg.select("tenant_id", "person_id")
        .union(lead_props.select("tenant_id", "person_id"))
        .union(comms.select("tenant_id", "person_id"))
        .distinct()
    )
    enriched = (
        persons.join(booking_agg, ["tenant_id", "person_id"], "left")
        .join(lead_props, ["tenant_id", "person_id"], "left")
        .join(
            cac,
            (persons.tenant_id == cac.tenant_id)
            & (lead_props.channel_of_first_touch == cac.channel),
            "left",
        )
        .join(comms, ["tenant_id", "person_id"], "left")
        .select(
            persons.tenant_id,
            persons.person_id,
            "bookings_total",
            "bookings_showed",
            "bookings_no_show",
            "ltv_cents",
            "no_show_rate",
            "last_booking_at",
            "channel_of_first_touch",
            "funnel_stage_max",
            "cac_channel_ngn_30d",
            "commission_earned_ngn_30d",
            "commission_paid_ngn_30d",
            "recon_mismatches_30d",
        )
    )
    return enriched


def to_kafka_frame(enriched: "DataFrame", snapshot_day, run_ts) -> "DataFrame":
    """Serialize enrichment rows into (key, value) Kafka records."""
    # Nested structs serialize directly — no JSON round-trip needed.
    props_struct = F.struct(
        F.col("bookings_total"),
        F.col("bookings_showed"),
        F.col("bookings_no_show"),
        F.col("ltv_cents"),
        F.col("no_show_rate"),
        F.date_format("last_booking_at", "yyyy-MM-dd'T'HH:mm:ss'Z'").alias(
            "last_booking_at"
        ),
        F.col("channel_of_first_touch"),
        F.col("funnel_stage_max"),
        F.col("cac_channel_ngn_30d"),
        F.col("commission_earned_ngn_30d"),
        F.col("commission_paid_ngn_30d"),
        F.col("recon_mismatches_30d"),
    )
    data_struct = F.struct(
        F.col("tenant_id"),
        F.col("person_id"),
        F.lit(snapshot_day.isoformat()).alias("snapshot_day"),
        props_struct.alias("properties"),
    )
    envelope = F.to_json(
        F.struct(
            F.lit("1.0").alias("specversion"),
            F.lit(EVENT_TYPE).alias("type"),
            F.lit(EVENT_SOURCE).alias("source"),
            F.concat_ws(
                ":",
                F.col("tenant_id"),
                F.col("person_id"),
                F.lit(snapshot_day.isoformat()),
            ).alias("id"),
            F.col("tenant_id").alias("tenantid"),
            F.lit(run_ts.isoformat()).alias("time"),
            data_struct.alias("data"),
        )
    )
    return enriched.select(
        F.concat_ws(":", F.col("tenant_id"), F.col("person_id")).alias("key"),
        envelope.alias("value"),
    )


# ---------------------------------------------------------------------------
# Spark session + sinks
# ---------------------------------------------------------------------------


def _merged_packages() -> str:
    """Iceberg + Kafka packages merged with any pre-existing packages config
    (sedona_common.py pattern, minus the Sedona artifacts — no geometry)."""
    merged: list[str] = []
    for pkg in (
        os.getenv("SPARK_JARS_PACKAGES", "").split(",")
        + ICEBERG_PACKAGES
        + ([KAFKA_PACKAGE] if OUTPUT_MODE in ("kafka", "both") else [])
    ):
        pkg = pkg.strip()
        if pkg and pkg not in merged:
            merged.append(pkg)
    return ",".join(merged)


def build_spark() -> "SparkSession":
    """Spark session wired to the Iceberg REST catalog + MinIO (S3FileIO)
    (silver_clean_bookings.py pattern) plus the Kafka sink package."""
    return (
        SparkSession.builder.appName("opendesk-graph-enrichment")
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
        snapshot_day = date.today()
        run_ts = datetime.now(UTC).replace(microsecond=0)
        enriched = compute_enrichment_rows(spark, snapshot_day).cache()
        count = enriched.count()
        print(f"[graph-enrich] {count} person enrichment rows for {snapshot_day}")

        records = to_kafka_frame(enriched, snapshot_day, run_ts)
        if OUTPUT_MODE in ("kafka", "both"):
            (
                records.write.format("kafka")
                .option("kafka.bootstrap.servers", KAFKA_BROKERS)
                .option("topic", ENRICHMENT_TOPIC)
                .save()
            )
            print(f"[graph-enrich] wrote {count} records to kafka topic {ENRICHMENT_TOPIC}")
        if OUTPUT_MODE in ("file", "both"):
            path = f"{OUTPUT_PATH.rstrip('/')}/snapshot_day={snapshot_day.isoformat()}"
            records.write.mode("overwrite").parquet(path)
            print(f"[graph-enrich] wrote {count} records to {path}")
        if OUTPUT_MODE not in ("kafka", "file", "both"):
            raise ValueError(f"unknown GRAPH_ENRICH_OUTPUT mode: {OUTPUT_MODE}")
    finally:
        spark.stop()


if __name__ == "__main__":
    main()
