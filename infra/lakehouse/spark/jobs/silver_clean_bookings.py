"""silver_clean_bookings — bronze → silver for booking events (SPEC §13).

Reads iceberg.bronze.booking_events (raw Kafka sink from analytics-pipeline),
deduplicates on event_id keeping the latest occurrence, and writes
iceberg.silver.booking_events.

Partition evolution: the silver table is created unpartitioned (spec v1) and then
evolved with `days(occurred_at)` (spec v2). Iceberg keeps both specs readable —
existing files stay valid, new writes are day-partitioned. The job is idempotent:
re-runs dynamically overwrite only the day-partitions present in the input.

Run (see infra/lakehouse/spark/README.md):

  docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
    --master spark://spark-master:7077 \
    --packages org.apache.iceberg:iceberg-spark-runtime-3.5_2.12:1.6.1,org.apache.iceberg:iceberg-aws-bundle:1.6.1 \
    /opt/spark-jobs/silver_clean_bookings.py
"""

import os

from pyspark.sql import SparkSession, Window
from pyspark.sql import functions as F

SOURCE_TABLE = "iceberg.bronze.booking_events"
TARGET_TABLE = "iceberg.silver.booking_events"
TARGET_NAMESPACE = "iceberg.silver"
PARTITION_SOURCE_COL = "occurred_at"
PARTITION_FIELD = "occurred_at_day"


def build_spark() -> SparkSession:
    """Spark session wired to the Iceberg REST catalog + MinIO (S3FileIO)."""
    return (
        SparkSession.builder.appName("opendesk-silver-clean-bookings")
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
    """Create the silver table (schema cloned from bronze) + evolve its partition spec."""
    spark.sql(f"CREATE NAMESPACE IF NOT EXISTS {TARGET_NAMESPACE}")

    existing = {r.tableName for r in spark.sql(f"SHOW TABLES IN {TARGET_NAMESPACE}").collect()}
    if "booking_events" not in existing:
        # Empty CTAS clones the bronze schema exactly (columns/types), spec v1: unpartitioned.
        spark.sql(
            f"CREATE TABLE {TARGET_TABLE} USING iceberg AS "
            f"SELECT * FROM {SOURCE_TABLE} WHERE 1 = 0"
        )
        print(f"[silver] created {TARGET_TABLE} (unpartitioned, spec v1)")

    # Partition evolution → spec v2: days(occurred_at). DESCRIBE on a v2/Iceberg table
    # lists partition transform columns; skip if already evolved.
    described = [r.col_name for r in spark.sql(f"DESCRIBE {TARGET_TABLE}").collect()]
    # 2026-11-21: DESCRIBE does not always list the partition field name, so a re-run tried to add
    # the same partition again ("Cannot add duplicate partition field"); the table DDL is authoritative.
    ddl = spark.sql(f"SHOW CREATE TABLE {TARGET_TABLE}").collect()[0][0].lower()
    already = PARTITION_FIELD in described or f"days({PARTITION_SOURCE_COL})" in ddl
    if not already:
        spark.sql(f"ALTER TABLE {TARGET_TABLE} ADD PARTITION FIELD days({PARTITION_SOURCE_COL})")
        print(f"[silver] evolved partition spec: + days({PARTITION_SOURCE_COL})")
    else:
        print(f"[silver] partition field {PARTITION_FIELD} already present, no evolution needed")


def clean(spark: SparkSession):
    bronze = spark.table(SOURCE_TABLE)

    # Dedupe: one row per event_id; on delivery duplicates keep the latest occurrence.
    latest = Window.partitionBy("event_id").orderBy(F.col("occurred_at").desc_nulls_last())
    deduped = (
        bronze.withColumn("_rn", F.row_number().over(latest))
        .filter(F.col("_rn") == 1)
        .drop("_rn")
        # Basic hygiene: drop malformed rows that cannot be keyed or placed in time.
        .filter(F.col("tenant_id").isNotNull() & F.col("booking_id").isNotNull())
        .filter(F.col("occurred_at").isNotNull())
    )
    return deduped


def main() -> None:
    spark = build_spark()
    try:
        ensure_target_table(spark)
        deduped = clean(spark)

        # Column order must match the target for DataFrameWriterV2; silver was CTAS-cloned
        # from bronze so names/order align — select explicitly to be safe.
        target_cols = [r.col_name for r in spark.sql(f"DESCRIBE {TARGET_TABLE}").collect()
                       if r.col_name and not r.col_name.startswith("#")][: len(deduped.columns)]
        out = deduped.select([c for c in deduped.columns if c in target_cols])

        # Dynamic partition overwrite: idempotent re-runs replace only touched day partitions.
        out.writeTo(TARGET_TABLE).overwritePartitions()
        print(f"[silver] wrote {out.count()} deduplicated rows to {TARGET_TABLE}")
    finally:
        spark.stop()


if __name__ == "__main__":
    main()
