"""cac_analytics — gold CAC (customer acquisition cost) tables (SPEC-W13 §4, Agent C).

Reads:

  * iceberg.bronze.cac_events — raw funnel events from the Kafka topic
    `cac.events` (CloudEvent type `com.opendesk.cac.FunnelEvent`, SPEC-W13 §2).
    INPUT CONTRACT (producer LANDED, SPEC-W33 §2 A2: the analytics-pipeline
    bronze sink now covers `cac.events` — analytics_pipeline.consumer
    topic_registry -> bronze.cac_events, auto-created like the other 4;
    mirror of geo_analytics.py's extract contracts):
    one row per funnel event with the CloudEvent `data` payload flattened:
      event_id string, tenant_id string, entity_type string ("lead|customer|agent"),
      entity_id string,
      event_name string ("lead_created|contacted|opted_in|qualified|converted|first_txn|lost"),
      event_ts timestamp, channel string, campaign_id string, lga_id int,
      amount_ngn double, idempotency_key string
    Env override: CAC_EVENTS_TABLE (default iceberg.bronze.cac_events). A
    missing/unreadable table logs a warning and yields empty gold tables
    instead of failing the pipeline — same graceful-degradation pattern as
    geo_analytics.py.
  * CAMPAIGN SPEND extract (parquet on MinIO) — daily spend entered via
    booking-service `POST /v1/campaigns/{id}/spend` (SPEC-W13 §4). INPUT
    CONTRACT (TODO producer: JDBC/analytics-pipeline export of the
    booking-service campaign-spend table). Path: env CAC_CAMPAIGN_SPEND_PATH
    (default s3://lake/extracts/campaign_spend/). Columns:
      tenant_id string, campaign_id string, channel string, day date,
      amount_ngn double
    Multiple rows per (tenant_id, channel, day) are summed.
  * LGA BOUNDARIES extract — Nigerian LGA polygons, same pattern as the
    service-areas extract in geo_analytics.py. Path: env
    CAC_LGA_BOUNDARIES_PATH (default s3://lake/extracts/lga_boundaries/);
    format env CAC_LGA_BOUNDARIES_FORMAT = parquet (default) | geojson.
      parquet columns: lga_id int, name string, geojson string
                       (GeoJSON Polygon/MultiPolygon, e.g. ST_AsGeoJSON(geom)
                       from a PostGIS export)
      geojson: a FeatureCollection whose features carry properties
               {lga_id, name}
    LGA boundaries are national reference data (not tenant-scoped); the gold
    table stays tenant-aware because the metric rows come from tenant events.

All three inputs are OPTIONAL at runtime: a missing source logs a warning and
the job writes empty/partial gold tables instead of failing the pipeline
(events missing → both tables empty; spend missing → spend_ngn = 0;
boundaries missing → geom/h3_cells NULL).

Outputs (Iceberg, Trino-visible, partitioned by `day`, dynamic partition
overwrite like the other gold jobs; geometry stored as WKT because Iceberg
has no geometry type):

  * cac_gold.daily_cac_by_channel — SPEC-W13 §4 contract:
    {day, tenant_id, channel, spend_ngn, leads, conversions, cac_ngn}
      leads       = deduped `lead_created` events
      conversions = deduped `converted` events (`first_txn` is a revenue
                    signal, NOT counted as a conversion — see
                    docs/cac-lakehouse.md)
      cac_ngn     = spend_ngn / conversions, NULL when conversions = 0
                    (never divide by zero; a day with spend but no conversions
                    has undefined CAC, not infinite CAC)
  * cac_gold.daily_cac_by_lga — SPEC-W13 §4 contract:
    {day, tenant_id, lga_id, leads, conversions, cac_ngn, geom}
    plus `h3_cells` (ARRAY<BIGINT>, H3 res-8 cells covering the LGA polygon —
    drill-down column, same ST_H3CellIDs/ST_H3ToGeom pattern as
    geo_analytics.py).
      Spend is recorded per (tenant, channel, day), not per LGA, so per-LGA
      CAC allocates the tenant-day total spend pro-rata by lead share:
          allocated_spend = tenant_day_spend * lga_leads / tenant_day_leads
          cac_ngn         = allocated_spend / conversions  (NULL if 0)
      Documented in docs/cac-lakehouse.md §4.

H3: the pinned Sedona (1.7.0, Spark 3.5 — see sedona_common.py) ships the H3
family (ST_H3CellIDs / ST_H3ToGeom, added in Sedona 1.6.0); resolution is
tunable via CAC_H3_RESOLUTION (default 8), mirroring GEO_H3_RESOLUTION.

Pure-function reference implementations of the aggregation math
(aggregate_daily_by_channel / aggregate_daily_by_lga and helpers) live at the
top of this module and are Spark-free so test_cac_analytics.py can exercise
them without a Spark session; the Spark transforms below mirror them
expression-for-expression. The pyspark import is guarded so this module stays
importable on driver-less CI boxes (same motivation as the lazy sedona import
in sedona_common.py).

Run (packages are injected by sedona_common; no --packages needed):

  docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
    --master spark://spark-master:7077 \
    /opt/spark-jobs/cac_analytics.py
"""

import os

try:  # driver-less CI boxes have no pyspark — pure functions stay importable.
    from pyspark.sql import DataFrame, SparkSession, Window
    from pyspark.sql import functions as F
    from pyspark.sql import types as T
except ImportError:  # pragma: no cover - exercised implicitly on CI lint boxes
    DataFrame = SparkSession = Window = None
    F = T = None

EVENTS_TABLE = os.getenv("CAC_EVENTS_TABLE", "iceberg.bronze.cac_events")
TARGET_NAMESPACE = "iceberg.cac_gold"
CHANNEL_TABLE = "iceberg.cac_gold.daily_cac_by_channel"
LGA_TABLE = "iceberg.cac_gold.daily_cac_by_lga"

H3_RESOLUTION = int(os.getenv("CAC_H3_RESOLUTION", "8"))
SPEND_PATH = os.getenv(
    "CAC_CAMPAIGN_SPEND_PATH", "s3://lake/extracts/campaign_spend/"
)
LGA_BOUNDARIES_PATH = os.getenv(
    "CAC_LGA_BOUNDARIES_PATH", "s3://lake/extracts/lga_boundaries/"
)
LGA_BOUNDARIES_FORMAT = os.getenv("CAC_LGA_BOUNDARIES_FORMAT", "parquet").lower()

# SPEC-W13 §2 event vocabulary.
LEAD_EVENT = "lead_created"
CONVERSION_EVENT = "converted"
KNOWN_CHANNELS = (
    "voice", "whatsapp", "telegram", "web", "sms",
    "webhook", "ussd", "qr", "promo", "field",
)
UNKNOWN_CHANNEL = "unknown"

# ---------------------------------------------------------------------------
# Pure aggregation math (Spark-free reference implementation — unit-tested)
# ---------------------------------------------------------------------------

def normalize_channel(channel) -> str:
    """Lower/strip a channel tag; empty/NULL buckets to 'unknown'."""
    if channel is None:
        return UNKNOWN_CHANNEL
    cleaned = str(channel).strip().lower()
    return cleaned if cleaned else UNKNOWN_CHANNEL


def classify_event(event_name) -> str | None:
    """Map a funnel event_name to 'lead' | 'conversion' | None (ignored).

    Case/whitespace tolerant. Only `lead_created` counts as a lead and only
    `converted` counts as a conversion; contacted/opted_in/qualified/
    first_txn/lost are funnel stages that do not enter the CAC numerator or
    denominator.
    """
    if event_name is None:
        return None
    name = str(event_name).strip().lower()
    if name == LEAD_EVENT:
        return "lead"
    if name == CONVERSION_EVENT:
        return "conversion"
    return None


def compute_cac_ngn(spend_ngn, conversions):
    """CAC = spend / conversions; None when conversions is 0/None (undefined,
    not infinite). spend None is treated as 0 (a day can have organic
    conversions with no recorded spend → CAC 0.0)."""
    if not conversions:
        return None
    spend = float(spend_ngn) if spend_ngn is not None else 0.0
    return spend / float(conversions)


def allocate_spend(total_spend_ngn, group_leads, total_leads) -> float:
    """Pro-rata share of a tenant-day spend pool for a sub-group (LGA),
    weighted by lead share. Zero/NULL-safe: no leads anywhere → 0.0."""
    if not total_leads or not group_leads or total_spend_ngn is None:
        return 0.0
    return float(total_spend_ngn) * float(group_leads) / float(total_leads)


def _dedupe_events(event_rows):
    """One row per idempotency_key (fallback event_id); latest event_ts wins.

    Mirrors the Spark transform: Window partitionBy idempotency key ordered by
    event_ts desc_nulls_last, row_number == 1. Rows without any key are kept
    (they cannot be deduped) but rows without tenant_id or event_ts are
    dropped as unkeyable/timeless — same hygiene filter as
    silver_clean_bookings.py.
    """
    keyed = {}
    out = []
    for row in event_rows:
        if row.get("tenant_id") is None or row.get("event_ts") is None:
            continue
        key = row.get("idempotency_key") or row.get("event_id")
        if key is None:
            out.append(row)
            continue
        current = keyed.get(key)
        if current is None or (row["event_ts"], ) >= (current["event_ts"], ):
            keyed[key] = row
    out.extend(keyed.values())
    return out


def aggregate_daily_by_channel(event_rows, spend_rows):
    """Reference aggregation for cac_gold.daily_cac_by_channel.

    event_rows: dicts with the bronze contract keys (event_name, event_ts,
    tenant_id, channel, idempotency_key/event_id; `day` as a date or
    derived from event_ts.date()).
    spend_rows: dicts {tenant_id, channel, day, amount_ngn} (pre-summed or
    not — rows are summed here too).

    Returns a sorted list of dicts {day, tenant_id, channel, spend_ngn,
    leads, conversions, cac_ngn} — one row per (day, tenant_id, channel) that
    has events and/or spend.
    """
    events = _dedupe_events(event_rows)

    spend_by_key = {}
    for row in spend_rows:
        if row.get("tenant_id") is None or row.get("day") is None:
            continue
        key = (row["day"], row["tenant_id"], normalize_channel(row.get("channel")))
        spend_by_key[key] = spend_by_key.get(key, 0.0) + float(row.get("amount_ngn") or 0.0)

    metrics = {}
    for row in events:
        kind = classify_event(row.get("event_name"))
        if kind is None:
            continue
        day = row.get("day")
        if day is None:
            day = row["event_ts"].date() if hasattr(row["event_ts"], "date") else row["event_ts"]
        key = (day, row["tenant_id"], normalize_channel(row.get("channel")))
        bucket = metrics.setdefault(key, {"leads": 0, "conversions": 0})
        bucket["leads" if kind == "lead" else "conversions"] += 1

    out = []
    for key in sorted(set(metrics) | set(spend_by_key), key=lambda k: (str(k[0]), str(k[1]), k[2])):
        day, tenant_id, channel = key
        counts = metrics.get(key, {"leads": 0, "conversions": 0})
        spend = spend_by_key.get(key, 0.0)
        out.append(
            {
                "day": day,
                "tenant_id": tenant_id,
                "channel": channel,
                "spend_ngn": spend,
                "leads": counts["leads"],
                "conversions": counts["conversions"],
                "cac_ngn": compute_cac_ngn(spend, counts["conversions"]),
            }
        )
    return out


def aggregate_daily_by_lga(event_rows, spend_rows):
    """Reference aggregation for cac_gold.daily_cac_by_lga (metrics only;
    geom/h3_cells are attached by the Spark transform via the LGA boundary
    extract — pure math cannot do geometry).

    Per-LGA CAC allocates the tenant-day spend pool (summed across channels)
    pro-rata by lead share: allocate_spend(tenant_day_spend, lga_leads,
    tenant_day_leads) / lga_conversions. Rows with NULL lga_id are excluded
    (they remain visible in the by-channel table).
    """
    events = [
        row for row in _dedupe_events(event_rows) if row.get("lga_id") is not None
    ]

    tenant_day_spend = {}
    for row in spend_rows:
        if row.get("tenant_id") is None or row.get("day") is None:
            continue
        key = (row["day"], row["tenant_id"])
        tenant_day_spend[key] = tenant_day_spend.get(key, 0.0) + float(row.get("amount_ngn") or 0.0)

    metrics = {}
    tenant_day_leads = {}
    for row in events:
        kind = classify_event(row.get("event_name"))
        if kind is None:
            continue
        day = row.get("day")
        if day is None:
            day = row["event_ts"].date() if hasattr(row["event_ts"], "date") else row["event_ts"]
        key = (day, row["tenant_id"], int(row["lga_id"]))
        bucket = metrics.setdefault(key, {"leads": 0, "conversions": 0})
        bucket["leads" if kind == "lead" else "conversions"] += 1
        if kind == "lead":
            td_key = (day, row["tenant_id"])
            tenant_day_leads[td_key] = tenant_day_leads.get(td_key, 0) + 1

    out = []
    for key in sorted(metrics, key=lambda k: (str(k[0]), str(k[1]), k[2])):
        day, tenant_id, lga_id = key
        counts = metrics[key]
        td_key = (day, tenant_id)
        allocated = allocate_spend(
            tenant_day_spend.get(td_key, 0.0),
            counts["leads"],
            tenant_day_leads.get(td_key, 0),
        )
        out.append(
            {
                "day": day,
                "tenant_id": tenant_id,
                "lga_id": lga_id,
                "leads": counts["leads"],
                "conversions": counts["conversions"],
                "cac_ngn": compute_cac_ngn(allocated, counts["conversions"]),
            }
        )
    return out


if T is not None:
    EVENTS_SCHEMA = T.StructType(
        [
            T.StructField("event_id", T.StringType()),
            T.StructField("tenant_id", T.StringType()),
            T.StructField("entity_type", T.StringType()),
            T.StructField("entity_id", T.StringType()),
            T.StructField("event_name", T.StringType()),
            T.StructField("event_ts", T.TimestampType()),
            T.StructField("channel", T.StringType()),
            T.StructField("campaign_id", T.StringType()),
            T.StructField("lga_id", T.IntegerType()),
            T.StructField("amount_ngn", T.DoubleType()),
            T.StructField("idempotency_key", T.StringType()),
        ]
    )
    SPEND_SCHEMA = T.StructType(
        [
            T.StructField("tenant_id", T.StringType()),
            T.StructField("campaign_id", T.StringType()),
            T.StructField("channel", T.StringType()),
            T.StructField("day", T.DateType()),
            T.StructField("amount_ngn", T.DoubleType()),
        ]
    )
    LGA_SCHEMA = T.StructType(
        [
            T.StructField("lga_id", T.IntegerType()),
            T.StructField("name", T.StringType()),
            T.StructField("geojson", T.StringType()),
        ]
    )


# ---------------------------------------------------------------------------
# Inputs (Spark)
# ---------------------------------------------------------------------------

def read_funnel_events(spark) -> "DataFrame":
    """Deduped, hygiene-filtered funnel events with a `day` column.

    Graceful degradation mirrors geo_analytics.py's extract readers: a
    missing bronze table logs a warning and yields an empty contract frame.
    """
    try:
        df = spark.table(EVENTS_TABLE)
    except Exception as exc:  # bronze.cac_events producer LANDED (SPEC-W33 §2 A2 analytics-pipeline sink); table may simply not exist yet on a fresh lakehouse
        print(f"[cac] WARNING: funnel events table unreadable at "
              f"{EVENTS_TABLE} ({exc.__class__.__name__}); using empty input")
        return spark.createDataFrame([], EVENTS_SCHEMA)
    dedupe_key = F.coalesce(F.col("idempotency_key"), F.col("event_id"))
    return (
        df.filter(F.col("tenant_id").isNotNull() & F.col("event_ts").isNotNull())
        # Idempotent consumer semantics (SPEC-W13 §2): one row per
        # idempotency_key (fallback event_id); latest event_ts wins.
        .withColumn("_dedupe_key", dedupe_key)
        .withColumn(
            "_rn",
            F.row_number().over(
                Window.partitionBy("_dedupe_key").orderBy(F.col("event_ts").desc_nulls_last())
            ),
        )
        .filter(F.col("_rn") == 1)
        .drop("_rn", "_dedupe_key")
        .withColumn("day", F.to_date("event_ts"))
        .withColumn("channel_norm", F.lower(F.trim(F.col("channel"))))
        .withColumn(
            "channel_norm",
            F.when(F.col("channel_norm").isNull() | (F.col("channel_norm") == ""), F.lit(UNKNOWN_CHANNEL))
            .otherwise(F.col("channel_norm")),
        )
        .withColumn("lga_id", F.col("lga_id").cast("int"))
    )


def read_campaign_spend(spark) -> "DataFrame":
    """Daily campaign spend summed per (tenant_id, channel, day); empty frame
    with a warning when the extract has not landed yet (TODO producer)."""
    try:
        df = spark.read.parquet(SPEND_PATH)
    except Exception as exc:  # path missing / no files yet — TODO producer above
        print(f"[cac] WARNING: campaign-spend extract unreadable at "
              f"{SPEND_PATH} ({exc.__class__.__name__}); using empty input")
        return spark.createDataFrame([], SPEND_SCHEMA)
    return (
        df.filter(F.col("tenant_id").isNotNull() & F.col("day").isNotNull())
        .withColumn("channel", F.lower(F.trim(F.col("channel"))))
        .withColumn(
            "channel",
            F.when(F.col("channel").isNull() | (F.col("channel") == ""), F.lit(UNKNOWN_CHANNEL))
            .otherwise(F.col("channel")),
        )
        .groupBy("tenant_id", "channel", "day")
        .agg(F.sum(F.coalesce(F.col("amount_ngn"), F.lit(0.0))).alias("spend_ngn"))
    )


def read_lga_boundaries(spark) -> "DataFrame":
    """LGA polygons (lga_id, name, geojson), or an empty contract frame.

    Same parquet|geojson dual-format reader as geo_analytics.read_service_areas.
    """
    try:
        if LGA_BOUNDARIES_FORMAT == "geojson":
            # FeatureCollection file(s); geometry kept as raw JSON text so
            # arbitrary Polygon/MultiPolygon objects survive re-serialisation
            # into ST_GeomFromGeoJSON (same trick as geo_analytics.py).
            raw = spark.read.text(LGA_BOUNDARIES_PATH, wholetext=True)
            df = (
                raw.select(
                    F.from_json(
                        "value",
                        T.StructType(
                            [T.StructField("features", T.ArrayType(T.StringType()))]
                        ),
                    ).alias("fc")
                )
                .select(F.explode("fc.features").alias("f"))
                .select(
                    F.get_json_object("f", "$.properties.lga_id").cast("int").alias("lga_id"),
                    F.get_json_object("f", "$.properties.name").alias("name"),
                    F.get_json_object("f", "$.geometry").alias("geojson"),
                )
            )
        else:
            df = spark.read.parquet(LGA_BOUNDARIES_PATH)
    except Exception as exc:
        print(f"[cac] WARNING: LGA boundaries extract unreadable at "
              f"{LGA_BOUNDARIES_PATH} ({exc.__class__.__name__}); using empty input")
        return spark.createDataFrame([], LGA_SCHEMA)
    return df.filter(F.col("lga_id").isNotNull() & F.col("geojson").isNotNull())


# ---------------------------------------------------------------------------
# Gold tables
# ---------------------------------------------------------------------------

def ensure_target_tables(spark) -> None:
    spark.sql(f"CREATE NAMESPACE IF NOT EXISTS {TARGET_NAMESPACE}")
    # SPEC-W13 §4 contract columns, verbatim. Partitioned by day: re-runs
    # dynamically overwrite only the day partitions present in the input.
    spark.sql(
        f"""
        CREATE TABLE IF NOT EXISTS {CHANNEL_TABLE} (
            day          DATE,
            tenant_id    STRING,
            channel      STRING,
            spend_ngn    DOUBLE,
            leads        BIGINT,
            conversions  BIGINT,
            cac_ngn      DOUBLE
        ) USING iceberg
        PARTITIONED BY (day)
        """
    )
    spark.sql(
        f"""
        CREATE TABLE IF NOT EXISTS {LGA_TABLE} (
            day          DATE,
            tenant_id    STRING,
            lga_id       INT,
            leads        BIGINT,
            conversions  BIGINT,
            cac_ngn      DOUBLE,
            geom         STRING,
            h3_cells     ARRAY<BIGINT>
        ) USING iceberg
        PARTITIONED BY (day)
        """
    )


def compute_daily_by_channel(events: "DataFrame", spend: "DataFrame") -> "DataFrame":
    """One row per (day, tenant_id, channel): leads, conversions, spend, CAC.

    Mirrors aggregate_daily_by_channel() expression-for-expression.
    """
    funnel = (
        events.filter(F.lower(F.trim(F.col("event_name"))).isin(LEAD_EVENT, CONVERSION_EVENT))
        .groupBy("day", "tenant_id", "channel_norm")
        .agg(
            F.sum(F.when(F.lower(F.trim(F.col("event_name"))) == LEAD_EVENT, 1).otherwise(0)).alias("leads"),
            F.sum(F.when(F.lower(F.trim(F.col("event_name"))) == CONVERSION_EVENT, 1).otherwise(0)).alias("conversions"),
        )
    )
    # FULL OUTER: days with spend but no events still get a row (spend_ngn > 0,
    # leads/conversions 0, cac_ngn NULL) — the reference aggregator does the
    # same via `set(metrics) | set(spend_by_key)`.
    joined = funnel.join(
        spend,
        (funnel.day == spend.day)
        & (funnel.tenant_id == spend.tenant_id)
        & (funnel.channel_norm == spend.channel),
        "full",
    ).select(
        F.coalesce(funnel.day, spend.day).alias("day"),
        F.coalesce(funnel.tenant_id, spend.tenant_id).alias("tenant_id"),
        F.coalesce(funnel.channel_norm, spend.channel).alias("channel"),
        F.coalesce(spend.spend_ngn, F.lit(0.0)).alias("spend_ngn"),
        F.coalesce(funnel.leads, F.lit(0)).alias("leads"),
        F.coalesce(funnel.conversions, F.lit(0)).alias("conversions"),
    )
    return joined.withColumn(
        "cac_ngn", F.col("spend_ngn") / F.nullif(F.col("conversions"), F.lit(0))
    ).select("day", "tenant_id", "channel", "spend_ngn", "leads", "conversions", "cac_ngn")


def compute_daily_by_lga(spark, events: "DataFrame", spend: "DataFrame") -> "DataFrame":
    """One row per (day, tenant_id, lga_id) with LGA geometry (WKT) and the
    H3 res-N drill-down cell array.

    Per-LGA spend is the tenant-day pool (all channels) allocated pro-rata by
    lead share — mirrors aggregate_daily_by_lga().
    """
    funnel = (
        events.filter(F.col("lga_id").isNotNull())
        .filter(F.lower(F.trim(F.col("event_name"))).isin(LEAD_EVENT, CONVERSION_EVENT))
        .groupBy("day", "tenant_id", "lga_id")
        .agg(
            F.sum(F.when(F.lower(F.trim(F.col("event_name"))) == LEAD_EVENT, 1).otherwise(0)).alias("leads"),
            F.sum(F.when(F.lower(F.trim(F.col("event_name"))) == CONVERSION_EVENT, 1).otherwise(0)).alias("conversions"),
        )
    )
    tenant_day_spend = spend.groupBy("day", "tenant_id").agg(
        F.sum("spend_ngn").alias("tenant_day_spend_ngn")
    )
    tenant_day_leads = (
        events.filter(F.lower(F.trim(F.col("event_name"))) == LEAD_EVENT)
        .filter(F.col("lga_id").isNotNull())
        .groupBy("day", "tenant_id")
        .count()
        .withColumnRenamed("count", "tenant_day_leads")
    )
    allocated = (
        funnel.join(tenant_day_spend, ["day", "tenant_id"], "left")
        .join(tenant_day_leads, ["day", "tenant_id"], "left")
        .withColumn(
            "allocated_spend_ngn",
            F.coalesce(F.col("tenant_day_spend_ngn"), F.lit(0.0))
            * F.col("leads")
            / F.nullif(F.col("tenant_day_leads"), F.lit(0)),
        )
        .withColumn(
            "cac_ngn",
            F.col("allocated_spend_ngn") / F.nullif(F.col("conversions"), F.lit(0)),
        )
    )
    lgas = read_lga_boundaries(spark).withColumn(
        "lga_geom", F.expr("ST_GeomFromGeoJSON(geojson)")
    )
    return (
        allocated.join(lgas, "lga_id", "left")
        # geom as WKT (Iceberg has no geometry type — same as geo_analytics).
        .withColumn("geom", F.expr("ST_AsText(lga_geom)"))
        # H3 res-N drill-down: cells covering the LGA polygon (Trino can
        # UNNEST; join cell geometry via ST_H3ToGeom as in geo_analytics).
        .withColumn("h3_cells", F.expr(f"ST_H3CellIDs(lga_geom, {H3_RESOLUTION})"))
        .select(
            "day", "tenant_id", "lga_id", "leads", "conversions", "cac_ngn",
            "geom", "h3_cells",
        )
    )


def main() -> None:
    # Imported here (not at module top) so the module stays importable without
    # the sedona Python package on driver-less CI boxes (sedona_common.py
    # already lazily imports sedona for the same reason).
    from sedona_common import build_sedona_context

    spark = build_sedona_context(app_name="opendesk-cac-analytics")
    try:
        ensure_target_tables(spark)
        events = read_funnel_events(spark).cache()
        spend = read_campaign_spend(spark).cache()
        print(f"[cac] {events.count()} deduped funnel events, "
              f"{spend.count()} tenant/channel/day spend rows")

        by_channel = compute_daily_by_channel(events, spend)
        by_channel.writeTo(CHANNEL_TABLE).overwritePartitions()
        print(f"[gold] wrote {by_channel.count()} rows to {CHANNEL_TABLE}")

        by_lga = compute_daily_by_lga(spark, events, spend)
        by_lga.writeTo(LGA_TABLE).overwritePartitions()
        print(f"[gold] wrote {by_lga.count()} rows to {LGA_TABLE}")
    finally:
        spark.stop()


if __name__ == "__main__":
    main()
