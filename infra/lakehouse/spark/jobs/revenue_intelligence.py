"""revenue_intelligence — gold.reco_pricing (SPEC-W3 §3, innovation 9).

Reads the lakehouse and writes per-tenant/offering pricing recommendations
consumed by analytics-pipeline's GET /v1/recommendations:

  * iceberg.silver.booking_events  (deduped by silver_clean_bookings;
    offering_id flows from bronze schema evolution — NULLs group as 'unknown')
  * iceberg.bronze.payment_events  (no silver payment job exists; the job
    dedupes on event_id inline, exactly like dbt's gold.revenue_daily)

Outputs (one row per tenant_id × offering_id per run):

  tenant_id, offering_id, computed_at,
  bookings_30d, net_revenue_cents_30d,
  peak_hour (0-23, argmax of the booking-count histogram over hour(starts_at)),
  peak_share (largest hour bucket / total bookings),
  no_show_rate (no_shows / (confirmed + no_shows)),
  suggested_peak_multiplier  (peak_share band: >=0.40 → 1.5, >=0.25 → 1.25, else 1.0),
  suggested_deposit_pct      (no_show_rate band: >=0.30 → 30, >=0.15 → 20,
                              >=0.05 → 10, else 0)

Idempotent: the gold table is partitioned by tenant_id and written with
dynamic partition overwrite, so re-runs only replace recomputed tenants.

Run (see infra/lakehouse/spark/README.md):

  docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
    --master spark://spark-master:7077 \
    --packages org.apache.iceberg:iceberg-spark-runtime-3.5_2.12:1.6.1,org.apache.iceberg:iceberg-aws-bundle:1.6.1 \
    /opt/spark-jobs/revenue_intelligence.py
"""

import os
from datetime import UTC, datetime, timedelta

from pyspark.sql import SparkSession, Window
from pyspark.sql import functions as F

BOOKINGS_TABLE = "iceberg.silver.booking_events"
PAYMENTS_TABLE = "iceberg.bronze.payment_events"
TARGET_TABLE = "iceberg.gold.reco_pricing"
TARGET_NAMESPACE = "iceberg.gold"

WINDOW_DAYS = int(os.getenv("RECO_WINDOW_DAYS", "30"))


def build_spark() -> SparkSession:
    """Spark session wired to the Iceberg REST catalog + MinIO (S3FileIO)."""
    return (
        SparkSession.builder.appName("opendesk-revenue-intelligence")
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


def ensure_target_table(spark: SparkSession) -> None:
    spark.sql(f"CREATE NAMESPACE IF NOT EXISTS {TARGET_NAMESPACE}")
    spark.sql(
        f"""
        CREATE TABLE IF NOT EXISTS {TARGET_TABLE} (
            tenant_id                 STRING,
            offering_id               STRING,
            computed_at               TIMESTAMP,
            bookings_30d              BIGINT,
            net_revenue_cents_30d     BIGINT,
            peak_hour                 INT,
            peak_share                DOUBLE,
            no_show_rate              DOUBLE,
            suggested_peak_multiplier DOUBLE,
            suggested_deposit_pct     INT
        ) USING iceberg
        PARTITIONED BY (tenant_id)
        """
    )


def compute(spark: SparkSession):
    """Per tenant/offering recommendation rows for the last WINDOW_DAYS days."""
    cutoff = datetime.now(UTC).replace(tzinfo=None) - timedelta(days=WINDOW_DAYS)

    bookings = spark.table(BOOKINGS_TABLE).filter(F.col("occurred_at") >= F.lit(cutoff))
    if "offering_id" in bookings.columns:
        bookings = bookings.withColumn(
            "offering_id", F.coalesce(F.col("offering_id"), F.lit("unknown"))
        )
    else:
        # Silver table predates the offering_id schema evolution — degrade
        # to per-tenant recommendations instead of failing the whole job.
        bookings = bookings.withColumn("offering_id", F.lit("unknown"))

    created = bookings.filter(F.lower(F.col("event_type")).isin("bookingcreated"))

    # Peak-hour distribution over the appointment start times.
    hourly = (
        created.filter(F.col("starts_at").isNotNull())
        .withColumn("hr", F.hour(F.col("starts_at")))
        .groupBy("tenant_id", "offering_id", "hr")
        .count()
    )
    totals = hourly.groupBy("tenant_id", "offering_id").agg(F.sum("count").alias("total"))
    win = Window.partitionBy("tenant_id", "offering_id").orderBy(F.col("count").desc(), F.col("hr"))
    peaks = (
        hourly.withColumn("_rn", F.row_number().over(win))
        .filter(F.col("_rn") == 1)
        .join(totals, ["tenant_id", "offering_id"])
        .select(
            "tenant_id",
            "offering_id",
            F.col("hr").alias("peak_hour"),
            (F.col("count") / F.col("total")).alias("peak_share"),
        )
    )

    funnel = (
        bookings.groupBy("tenant_id", "offering_id")
        .agg(
            F.sum(F.when(F.lower(F.col("event_type")) == "bookingcreated", 1).otherwise(0)).alias("bookings_30d"),
            F.sum(F.when(F.lower(F.col("event_type")) == "bookingconfirmed", 1).otherwise(0)).alias("confirmed"),
            F.sum(F.when(F.lower(F.col("event_type")) == "bookingnoshow", 1).otherwise(0)).alias("no_shows"),
        )
        .withColumn(
            "no_show_rate",
            # 2026-11-21: F.nullif on aggregate columns fails in Spark 3.5 ("Invalid call to
            # dataType on unresolved object"); same semantics: NULL when the denominator is 0.
            F.when(
                (F.col("confirmed") + F.col("no_shows")) == 0, F.lit(None)
            ).otherwise(F.col("no_shows") / (F.col("confirmed") + F.col("no_shows"))),
        )
    )

    # Net captured revenue per offering (deduped payments joined by booking_id).
    payments = spark.table(PAYMENTS_TABLE).filter(F.col("occurred_at") >= F.lit(cutoff))
    dedup = Window.partitionBy("event_id").orderBy(F.col("occurred_at").desc_nulls_last())
    payments = (
        payments.withColumn("_rn", F.row_number().over(dedup))
        .filter(F.col("_rn") == 1)
        .filter(F.col("transfer_code").isin(101, 102, 103))
        .withColumn(
            "signed_cents",
            F.when(F.col("transfer_code") == 102, -F.col("amount_cents")).otherwise(F.col("amount_cents")),
        )
    )
    booking_offering = bookings.select("tenant_id", "booking_id", "offering_id").dropDuplicates(
        ["tenant_id", "booking_id"]
    )
    revenue = (
        payments.join(booking_offering, ["tenant_id", "booking_id"], "inner")
        .groupBy("tenant_id", "offering_id")
        .agg(F.sum("signed_cents").alias("net_revenue_cents_30d"))
    )

    out = (
        funnel.join(peaks, ["tenant_id", "offering_id"], "left")
        .join(revenue, ["tenant_id", "offering_id"], "left")
        .withColumn("computed_at", F.current_timestamp())
        .withColumn("net_revenue_cents_30d", F.coalesce(F.col("net_revenue_cents_30d"), F.lit(0)))
        .withColumn("no_show_rate", F.coalesce(F.col("no_show_rate"), F.lit(0.0)))
        .withColumn(
            "suggested_peak_multiplier",
            F.when(F.col("peak_share") >= 0.40, F.lit(1.5))
            .when(F.col("peak_share") >= 0.25, F.lit(1.25))
            .otherwise(F.lit(1.0)),
        )
        .withColumn(
            "suggested_deposit_pct",
            F.when(F.col("no_show_rate") >= 0.30, F.lit(30))
            .when(F.col("no_show_rate") >= 0.15, F.lit(20))
            .when(F.col("no_show_rate") >= 0.05, F.lit(10))
            .otherwise(F.lit(0)),
        )
        .select(
            "tenant_id",
            "offering_id",
            "computed_at",
            "bookings_30d",
            "net_revenue_cents_30d",
            F.col("peak_hour").cast("int").alias("peak_hour"),
            "peak_share",
            "no_show_rate",
            "suggested_peak_multiplier",
            F.col("suggested_deposit_pct").cast("int").alias("suggested_deposit_pct"),
        )
    )
    return out


def main() -> None:
    spark = build_spark()
    try:
        ensure_target_table(spark)
        out = compute(spark)
        out.writeTo(TARGET_TABLE).overwritePartitions()
        print(f"[gold] wrote {out.count()} recommendation rows to {TARGET_TABLE}")
    finally:
        spark.stop()


if __name__ == "__main__":
    main()
