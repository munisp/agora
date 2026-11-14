"""receipt_recon_gold — gold receipt recon + ingest daily tables (SPEC-W62 §3
+ SPEC-W64 §6, Coder C lakehouse).

Reads (ALL OPTIONAL at runtime — degrade-graceful per SPEC-W62 "Scheduling":
a missing/unreadable bronze table logs a warning and the job SKIPS that
output (or degrades that signal, see below), still exiting 0; the tables
land with the analytics-pipeline mappers and may simply not exist yet on an
older deployment):

  * iceberg.bronze.receipt_recon_findings — ReceiptReconFinding CloudEvents
    (`com.opendesk.booking.ReceiptReconFinding` on `opendesk.booking.events`)
    routed to their own bronze table by the analytics-pipeline mapper
    (SPEC-W62 §2/G2; previously these landed sparsely in bronze.booking_events).
    INPUT CONTRACT (pinned columns):
      event_id string, tenant_id string, run_id string, finding_kind string
      (receipt_without_payment|payment_without_receipt|amount_mismatch|
      currency_unknown), confidence string (exact|heuristic|none),
      capture_client_id string (nullable), booking_id string (nullable),
      attempt_tx_ref string (nullable), expected_amount_minor long (nullable),
      captured_amount_minor long (nullable), receipt_amount_major double
      (nullable), receipt_currency string (nullable), occurred_at timestamp,
      finding_id string (nullable — SPEC-W64 appended column; pre-W64 bronze
      tables may lack it entirely, handled below)
    Env override: RECEIPT_RECON_FINDINGS_TABLE.
  * iceberg.bronze.receipt_documents — OCR DocumentExtracted events
    (`com.opendesk.ocr.DocumentExtracted` on `opendesk.ocr.events`), kind='receipt'
    documents only, kyc_* kinds DROPPED by the mapper (SPEC-W62 §1/G1 PII
    exclusion; hash-only sha256/phash + whitelisted scalars, no free text).
    INPUT CONTRACT (SPEC-W62 pinned columns):
      event_id string, tenant_id string, document_id string, kind string,
      status string, page_count int, sha256 string, phash string,
      model_version string, amount_minor long (nullable), currency string
      (nullable), receipt_date string (nullable), occurred_at timestamp
    Env override: RECEIPT_DOCUMENTS_TABLE.
  * iceberg.bronze.recon_resolutions — ReceiptReconResolved events
    (`com.opendesk.booking.ReceiptReconResolved` on `opendesk.booking.events`,
    SPEC-W64 §2: booking-service Store.ResolveFinding outbox, same tx as the
    resolved_at update).
    INPUT CONTRACT (SPEC-W64 pinned columns):
      event_id string, tenant_id string, finding_id string, run_id string,
      kind string, resolved_at timestamp (nullable), resolved_by string
      (nullable), occurred_at timestamp
    Env override: RECEIPT_RECON_RESOLUTIONS_TABLE.
  * iceberg.bronze.receipt_ingest — ReceiptIngested events
    (`com.opendesk.booking.ReceiptIngested` on `opendesk.booking.events`,
    SPEC-W64 §3/G-B: fieldcapture AttachReceipt channel='field_pwa',
    media_inbox IngestMediaInbox fresh-insert branch channel='whatsapp').
    Only document_id + channel are read here (channel attribution join).
    INPUT CONTRACT (SPEC-W64 pinned columns):
      event_id string, tenant_id string, channel string, document_id string,
      capture_client_id string (nullable), media_inbox_id string (nullable),
      booking_id string (nullable), mime string (nullable), bytes long
      (nullable), sha256_prefix string (nullable), state string (nullable),
      occurred_at timestamp
    Env override: RECEIPT_INGEST_TABLE.

Outputs (Iceberg, Trino-visible, partitioned by `day`, dynamic partition
overwrite like the other gold jobs — re-runs replace only the day partitions
present in the input):

  * iceberg.gold.receipt_recon_daily — contract:
    {tenant_id, day, finding_kind, confidence, findings_count, resolved_count,
     expected_sum_minor, captured_sum_minor, mismatch_abs_sum_minor}
      findings_count        = deduped findings (one row per event_id)
      resolved_count        = SPEC-W64: REAL count of deduped
                              bronze.recon_resolutions events joined back to
                              their finding on finding_id (attributed to the
                              finding's tenant/day/kind/confidence bucket —
                              the finding's day, not the resolution day).
                              Resolutions whose finding_id has no finding row
                              (e.g. finding predates the W64 finding_id
                              bronze column) are unattributable and NOT
                              counted. Degrade-graceful: when
                              bronze.recon_resolutions (or the findings
                              finding_id column) is missing, resolved_count
                              falls back to 0 — the W62 posture.
      expected_sum_minor    = sum(coalesce(expected_amount_minor, 0))
      captured_sum_minor    = sum(coalesce(captured_amount_minor, 0))
      mismatch_abs_sum_minor = sum(abs(expected - captured)) over rows where
                              BOTH sides are present (0 otherwise)
  * iceberg.gold.receipt_ingest_daily — contract (W64 grain: channel added):
    {tenant_id, day, channel, documents, pages, receipts, receipts_with_amount}
      documents            = deduped OCR document events
      pages                = sum(coalesce(page_count, 0))
      receipts             = rows with kind='receipt' (the mapper only lands
                             receipts; the filter is kept for robustness)
      receipts_with_amount = receipts with amount_minor NOT NULL
      channel              = SPEC-W64: attributed from bronze.receipt_ingest
                             via document_id (field_pwa|whatsapp) — closes
                             the W62 channel-attribution gap. NULL when no
                             ingest event exists for the document (direct
                             /v1/ocr/extract uploads, pre-W64 rows) or when
                             bronze.receipt_ingest is missing entirely
                             (degrade-graceful on old deployments).

Pure-function reference implementations of the aggregation math
(aggregate_recon_daily / aggregate_ingest_daily and helpers) live at the top
of this module and are Spark-free so they can be unit-tested without a Spark
session; the Spark transforms below mirror them expression-for-expression.
The pyspark import is guarded so this module stays importable on driver-less
CI boxes (same motivation as cac_analytics.py).

Run (packages are defaulted by run-scheduled.sh):

  docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
    --master spark://spark-master:7077 \
    --packages org.apache.iceberg:iceberg-spark-runtime-3.5_2.12:1.6.1,org.apache.iceberg:iceberg-aws-bundle:1.6.1 \
    /opt/spark-jobs/receipt_recon_gold.py
"""

import os

try:  # driver-less CI boxes have no pyspark — pure functions stay importable.
    from pyspark.sql import SparkSession, Window
    from pyspark.sql import functions as F
except ImportError:  # pragma: no cover - exercised implicitly on CI lint boxes
    SparkSession = Window = None
    F = None

FINDINGS_TABLE = os.getenv(
    "RECEIPT_RECON_FINDINGS_TABLE", "iceberg.bronze.receipt_recon_findings"
)
DOCUMENTS_TABLE = os.getenv(
    "RECEIPT_DOCUMENTS_TABLE", "iceberg.bronze.receipt_documents"
)
# SPEC-W64: the two new bronze tables (analytics-pipeline G-B router arms).
RESOLUTIONS_TABLE = os.getenv(
    "RECEIPT_RECON_RESOLUTIONS_TABLE", "iceberg.bronze.recon_resolutions"
)
INGEST_TABLE = os.getenv(
    "RECEIPT_INGEST_TABLE", "iceberg.bronze.receipt_ingest"
)
TARGET_NAMESPACE = "iceberg.gold"
RECON_DAILY_TABLE = "iceberg.gold.receipt_recon_daily"
INGEST_DAILY_TABLE = "iceberg.gold.receipt_ingest_daily"

# SPEC-W62 G2 accepted values (kept as documentation/eyeball anchors; the
# bronze mapper + upstream CHECK constraints enforce them).
FINDING_KINDS = (
    "receipt_without_payment",
    "payment_without_receipt",
    "amount_mismatch",
    "currency_unknown",
)
CONFIDENCES = ("exact", "heuristic", "none")
# SPEC-W64: ReceiptIngested channel enum (dbt silver accepted_values).
CHANNELS = ("field_pwa", "whatsapp")
RECEIPT_KIND = "receipt"

# ---------------------------------------------------------------------------
# Pure aggregation math (Spark-free reference implementation — unit-tested)
# ---------------------------------------------------------------------------

def _row_day(row):
    """Day for a bronze row: explicit `day` else derived from occurred_at."""
    day = row.get("day")
    if day is None:
        ts = row.get("occurred_at")
        day = ts.date() if hasattr(ts, "date") else ts
    return day


def _dedupe_events(event_rows):
    """One row per event_id; latest occurred_at wins (at-least-once sink
    hygiene — mirrors the Spark Window dedupe and the other gold jobs).
    Rows without tenant_id or occurred_at are dropped as unkeyable/timeless
    (same hygiene filter as silver_clean_bookings.py); rows without an
    event_id are kept (they cannot be deduped)."""
    keyed = {}
    out = []
    for row in event_rows:
        if row.get("tenant_id") is None or row.get("occurred_at") is None:
            continue
        key = row.get("event_id")
        if key is None:
            out.append(row)
            continue
        current = keyed.get(key)
        if current is None or (row["occurred_at"],) >= (current["occurred_at"],):
            keyed[key] = row
    out.extend(keyed.values())
    return out


def _mismatch_abs(expected, captured):
    """abs(expected - captured) when both sides exist, else 0."""
    if expected is None or captured is None:
        return 0
    return abs(int(expected) - int(captured))


def aggregate_recon_daily(finding_rows, resolution_rows=None):
    """Reference aggregation for gold.receipt_recon_daily.

    finding_rows: dicts with the bronze contract keys (event_id, tenant_id,
    finding_kind, confidence, expected_amount_minor, captured_amount_minor,
    occurred_at, finding_id (W64); `day` as a date or derived from
    occurred_at.date()).

    resolution_rows (SPEC-W64, optional): dicts with the bronze
    recon_resolutions keys (event_id, tenant_id, finding_id, occurred_at).
    Each deduped resolution whose finding_id matches a (deduped) finding
    increments THAT finding's bucket (tenant_id, day, finding_kind,
    confidence) — attribution is by the finding, so resolved_count lands on
    the finding's day/kind/confidence. Resolutions with an unknown finding_id
    are unattributable and NOT counted; resolution_rows=None (missing bronze
    table) degrades to the W62 posture (resolved_count = 0).

    Returns a sorted list of dicts {tenant_id, day, finding_kind, confidence,
    findings_count, resolved_count, expected_sum_minor, captured_sum_minor,
    mismatch_abs_sum_minor} — one row per (tenant_id, day, finding_kind,
    confidence).
    """
    metrics = {}
    finding_bucket = {}  # finding_id -> bucket key (for resolution attribution)
    for row in _dedupe_events(finding_rows):
        key = (row["tenant_id"], _row_day(row), row.get("finding_kind"), row.get("confidence"))
        bucket = metrics.setdefault(
            key,
            {"findings": 0, "resolved": 0, "expected": 0, "captured": 0, "mismatch_abs": 0},
        )
        bucket["findings"] += 1
        bucket["expected"] += int(row.get("expected_amount_minor") or 0)
        bucket["captured"] += int(row.get("captured_amount_minor") or 0)
        bucket["mismatch_abs"] += _mismatch_abs(
            row.get("expected_amount_minor"), row.get("captured_amount_minor")
        )
        finding_id = row.get("finding_id")
        if finding_id is not None:
            finding_bucket[finding_id] = key

    if resolution_rows is not None:
        for row in _dedupe_events(resolution_rows):
            key = finding_bucket.get(row.get("finding_id"))
            if key is not None:
                metrics[key]["resolved"] += 1

    out = []
    for key in sorted(metrics, key=lambda k: (str(k[0]), str(k[1]), str(k[2]), str(k[3]))):
        tenant_id, day, finding_kind, confidence = key
        bucket = metrics[key]
        out.append(
            {
                "tenant_id": tenant_id,
                "day": day,
                "finding_kind": finding_kind,
                "confidence": confidence,
                "findings_count": bucket["findings"],
                "resolved_count": bucket["resolved"],
                "expected_sum_minor": bucket["expected"],
                "captured_sum_minor": bucket["captured"],
                "mismatch_abs_sum_minor": bucket["mismatch_abs"],
            }
        )
    return out


def aggregate_ingest_daily(document_rows, channel_rows=None):
    """Reference aggregation for gold.receipt_ingest_daily.

    document_rows: dicts with the bronze contract keys (event_id, tenant_id,
    document_id, kind, page_count, amount_minor, occurred_at; `day` as a date
    or derived from occurred_at.date()).

    channel_rows (SPEC-W64, optional): dicts with the bronze receipt_ingest
    keys (event_id, tenant_id, document_id, channel, occurred_at). Builds a
    document_id -> channel attribution (latest occurred_at wins, after the
    event_id dedupe — mirrors the Spark Window); documents without an ingest
    event get channel=None. channel_rows=None (missing bronze table)
    degrades to all-NULL channel — the W62 posture, with the column still
    present.

    Returns a sorted list of dicts {tenant_id, day, channel, documents,
    pages, receipts, receipts_with_amount} — one row per (tenant_id, day,
    channel) (W64 grain).
    """
    channel_by_document = {}
    if channel_rows is not None:
        for row in _dedupe_events(channel_rows):
            document_id = row.get("document_id")
            if document_id is None:
                continue
            current = channel_by_document.get(document_id)
            if current is None or (row["occurred_at"],) >= (current[0],):
                channel_by_document[document_id] = (row["occurred_at"], row.get("channel"))
        channel_by_document = {k: v[1] for k, v in channel_by_document.items()}

    metrics = {}
    for row in _dedupe_events(document_rows):
        channel = channel_by_document.get(row.get("document_id"))
        key = (row["tenant_id"], _row_day(row), channel)
        bucket = metrics.setdefault(
            key, {"documents": 0, "pages": 0, "receipts": 0, "with_amount": 0}
        )
        bucket["documents"] += 1
        bucket["pages"] += int(row.get("page_count") or 0)
        kind = row.get("kind")
        if kind is not None and str(kind).strip().lower() == RECEIPT_KIND:
            bucket["receipts"] += 1
            if row.get("amount_minor") is not None:
                bucket["with_amount"] += 1

    out = []
    for key in sorted(metrics, key=lambda k: (str(k[0]), str(k[1]), str(k[2]))):
        tenant_id, day, channel = key
        bucket = metrics[key]
        out.append(
            {
                "tenant_id": tenant_id,
                "day": day,
                "channel": channel,
                "documents": bucket["documents"],
                "pages": bucket["pages"],
                "receipts": bucket["receipts"],
                "receipts_with_amount": bucket["with_amount"],
            }
        )
    return out


# ---------------------------------------------------------------------------
# Spark session + target tables
# ---------------------------------------------------------------------------

def build_spark() -> "SparkSession":
    """Spark session wired to the Iceberg REST catalog + MinIO (S3FileIO)."""
    return (
        SparkSession.builder.appName("opendesk-receipt-recon-gold")
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


def ensure_target_tables(spark: "SparkSession") -> None:
    """Create the gold namespace + both contract tables (idempotent).

    Partitioned by day: re-runs dynamically overwrite only the day partitions
    present in the input (same idiom as cac_analytics.py).
    """
    spark.sql(f"CREATE NAMESPACE IF NOT EXISTS {TARGET_NAMESPACE}")
    # Contract columns, verbatim.
    spark.sql(
        f"""
        CREATE TABLE IF NOT EXISTS {RECON_DAILY_TABLE} (
            tenant_id              STRING,
            day                    DATE,
            finding_kind           STRING,
            confidence             STRING,
            findings_count         BIGINT,
            resolved_count         BIGINT,
            expected_sum_minor     BIGINT,
            captured_sum_minor     BIGINT,
            mismatch_abs_sum_minor BIGINT
        ) USING iceberg
        PARTITIONED BY (day)
        """
    )
    # SPEC-W64: receipt_ingest_daily gains `channel` (grain is now
    # tenant/day/channel). For tables created pre-W64 the CREATE is a no-op,
    # so evolve the schema in place (Iceberg ADD COLUMN keeps old files
    # valid; old rows read NULL channel).
    spark.sql(
        f"""
        CREATE TABLE IF NOT EXISTS {INGEST_DAILY_TABLE} (
            tenant_id            STRING,
            day                  DATE,
            channel              STRING,
            documents            BIGINT,
            pages                BIGINT,
            receipts             BIGINT,
            receipts_with_amount BIGINT
        ) USING iceberg
        PARTITIONED BY (day)
        """
    )
    if "channel" not in spark.table(INGEST_DAILY_TABLE).columns:
        spark.sql(f"ALTER TABLE {INGEST_DAILY_TABLE} ADD COLUMN channel STRING")
        print(f"[gold] evolved {INGEST_DAILY_TABLE}: added channel column (W64)")


# ---------------------------------------------------------------------------
# Inputs (Spark) — degrade-graceful: missing bronze table => None => skip
# that output (or degrade that signal to the W62 posture: resolved_count=0,
# channel NULL), log a warning, exit 0 (SPEC-W62 "Scheduling", SPEC-W64).
# ---------------------------------------------------------------------------

def read_recon_findings(spark: "SparkSession"):
    """Deduped, hygiene-filtered recon findings with a `day` column, or None
    when the bronze table does not exist yet (coder A's G2 routing lands
    in-wave; a fresh lakehouse may not have it)."""
    try:
        df = spark.table(FINDINGS_TABLE)
    except Exception as exc:
        print(f"[receipt-recon] WARNING: findings table unreadable at "
              f"{FINDINGS_TABLE} ({exc.__class__.__name__}); skipping "
              f"{RECON_DAILY_TABLE} output")
        return None
    return (
        df.filter(F.col("tenant_id").isNotNull() & F.col("occurred_at").isNotNull())
        # At-least-once sink hygiene: one row per event_id, latest wins.
        .withColumn(
            "_rn",
            F.row_number().over(
                Window.partitionBy("event_id").orderBy(F.col("occurred_at").desc_nulls_last())
            ),
        )
        .filter(F.col("_rn") == 1)
        .drop("_rn")
        .withColumn("day", F.to_date("occurred_at"))
    )


def read_receipt_documents(spark: "SparkSession"):
    """Deduped, hygiene-filtered receipt document events with a `day` column,
    or None when the bronze table does not exist yet."""
    try:
        df = spark.table(DOCUMENTS_TABLE)
    except Exception as exc:
        print(f"[receipt-recon] WARNING: receipt documents table unreadable at "
              f"{DOCUMENTS_TABLE} ({exc.__class__.__name__}); skipping "
              f"{INGEST_DAILY_TABLE} output")
        return None
    return (
        df.filter(F.col("tenant_id").isNotNull() & F.col("occurred_at").isNotNull())
        .withColumn(
            "_rn",
            F.row_number().over(
                Window.partitionBy("event_id").orderBy(F.col("occurred_at").desc_nulls_last())
            ),
        )
        .filter(F.col("_rn") == 1)
        .drop("_rn")
        .withColumn("day", F.to_date("occurred_at"))
    )


def read_recon_resolutions(spark: "SparkSession"):
    """SPEC-W64: deduped, hygiene-filtered resolution events, or None when
    bronze.recon_resolutions does not exist yet (pre-W64 deployment —
    resolved_count then degrades to 0)."""
    try:
        df = spark.table(RESOLUTIONS_TABLE)
    except Exception as exc:
        print(f"[receipt-recon] WARNING: recon resolutions table unreadable at "
              f"{RESOLUTIONS_TABLE} ({exc.__class__.__name__}); resolved_count "
              f"degrades to 0 in {RECON_DAILY_TABLE}")
        return None
    return (
        df.filter(
            F.col("tenant_id").isNotNull()
            & F.col("occurred_at").isNotNull()
            & F.col("finding_id").isNotNull()
        )
        .withColumn(
            "_rn",
            F.row_number().over(
                Window.partitionBy("event_id").orderBy(F.col("occurred_at").desc_nulls_last())
            ),
        )
        .filter(F.col("_rn") == 1)
        .drop("_rn")
    )


def read_receipt_ingest(spark: "SparkSession"):
    """SPEC-W64: document_id -> channel attribution frame (deduped ingest
    events), or None when bronze.receipt_ingest does not exist yet
    (pre-W64 deployment — channel then degrades to NULL)."""
    try:
        df = spark.table(INGEST_TABLE)
    except Exception as exc:
        print(f"[receipt-recon] WARNING: receipt ingest table unreadable at "
              f"{INGEST_TABLE} ({exc.__class__.__name__}); channel degrades "
              f"to NULL in {INGEST_DAILY_TABLE}")
        return None
    return (
        df.filter(
            F.col("tenant_id").isNotNull()
            & F.col("occurred_at").isNotNull()
            & F.col("document_id").isNotNull()
        )
        .withColumn(
            "_rn",
            F.row_number().over(
                Window.partitionBy("event_id").orderBy(F.col("occurred_at").desc_nulls_last())
            ),
        )
        .filter(F.col("_rn") == 1)
        .drop("_rn")
        # One channel per document (latest ingest event wins — mirrors the
        # pure function's channel_by_document map).
        .withColumn(
            "_rn",
            F.row_number().over(
                Window.partitionBy("document_id").orderBy(F.col("occurred_at").desc_nulls_last())
            ),
        )
        .filter(F.col("_rn") == 1)
        .drop("_rn")
        .select(
            F.col("document_id").alias("_ingest_document_id"),
            F.lower(F.trim(F.col("channel"))).alias("_ingest_channel"),
        )
    )


# ---------------------------------------------------------------------------
# Gold tables (Spark transforms — mirror the pure reference functions)
# ---------------------------------------------------------------------------

def compute_recon_daily(findings: "DataFrame", resolutions: "DataFrame | None" = None) -> "DataFrame":
    """One row per (tenant_id, day, finding_kind, confidence).

    Mirrors aggregate_recon_daily() expression-for-expression. SPEC-W64:
    resolved_count is a REAL count of bronze.recon_resolutions events joined
    to their finding on finding_id (attributed to the finding's bucket).
    Degrade-graceful: resolutions=None (missing bronze table) OR a findings
    frame without the W64 finding_id column (pre-W64 bronze) falls back to
    resolved_count = 0 — the W62 posture.
    """
    both = F.col("expected_amount_minor").isNotNull() & F.col("captured_amount_minor").isNotNull()
    base = (
        findings.groupBy("tenant_id", "day", "finding_kind", "confidence")
        .agg(
            F.count("*").alias("findings_count"),
            F.sum(F.coalesce(F.col("expected_amount_minor"), F.lit(0))).alias("expected_sum_minor"),
            F.sum(F.coalesce(F.col("captured_amount_minor"), F.lit(0))).alias("captured_sum_minor"),
            F.sum(
                F.when(both, F.abs(F.col("expected_amount_minor") - F.col("captured_amount_minor")))
                .otherwise(F.lit(0))
            ).alias("mismatch_abs_sum_minor"),
        )
    )
    if resolutions is None or "finding_id" not in findings.columns:
        if resolutions is not None:
            print("[receipt-recon] WARNING: findings frame has no finding_id "
                  "column (pre-W64 bronze); resolved_count degrades to 0")
        return base.withColumn("resolved_count", F.lit(0).cast("bigint")).select(
            "tenant_id", "day", "finding_kind", "confidence",
            "findings_count", "resolved_count",
            "expected_sum_minor", "captured_sum_minor", "mismatch_abs_sum_minor",
        )
    # Attribution join: each resolution counts towards ITS finding's bucket
    # (finding's tenant/day/kind/confidence) — mirrors finding_bucket in the
    # pure function. finding_id is globally unique, so the join key alone is
    # sufficient.
    keys = findings.select(
        F.col("finding_id").alias("_finding_id"),
        F.col("tenant_id").alias("_tenant_id"),
        F.col("day").alias("_day"),
        F.col("finding_kind").alias("_finding_kind"),
        F.col("confidence").alias("_confidence"),
    )
    resolved = (
        resolutions.join(keys, resolutions["finding_id"] == keys["_finding_id"], "inner")
        .groupBy("_tenant_id", "_day", "_finding_kind", "_confidence")
        .agg(F.count("*").alias("_resolved"))
    )
    return (
        base.join(
            resolved,
            (base["tenant_id"] == resolved["_tenant_id"])
            & (base["day"] == resolved["_day"])
            # NULL-safe on the bronze-nullable enum columns (mirrors the pure
            # function, which attributes into (None-kind/confidence) buckets).
            & base["finding_kind"].eqNullSafe(resolved["_finding_kind"])
            & base["confidence"].eqNullSafe(resolved["_confidence"]),
            "left",
        )
        .select(
            "tenant_id", "day", "finding_kind", "confidence",
            "findings_count",
            F.coalesce(F.col("_resolved"), F.lit(0)).cast("bigint").alias("resolved_count"),
            "expected_sum_minor", "captured_sum_minor", "mismatch_abs_sum_minor",
        )
    )


def compute_ingest_daily(documents: "DataFrame", ingest: "DataFrame | None" = None) -> "DataFrame":
    """One row per (tenant_id, day, channel) — W64 grain. Mirrors
    aggregate_ingest_daily(). channel is attributed from bronze.receipt_ingest
    via document_id; ingest=None (missing bronze table) degrades to NULL
    channel for every document (the column is still written)."""
    if ingest is not None:
        documents = documents.join(
            ingest,
            documents["document_id"] == ingest["_ingest_document_id"],
            "left",
        ).withColumn("channel", F.col("_ingest_channel")).drop(
            "_ingest_document_id", "_ingest_channel"
        )
    else:
        documents = documents.withColumn("channel", F.lit(None).cast("string"))
    is_receipt = F.lower(F.trim(F.col("kind"))) == F.lit(RECEIPT_KIND)
    return (
        documents.groupBy("tenant_id", "day", "channel")
        .agg(
            F.count("*").alias("documents"),
            F.sum(F.coalesce(F.col("page_count"), F.lit(0))).alias("pages"),
            F.sum(F.when(is_receipt, 1).otherwise(0)).alias("receipts"),
            F.sum(
                F.when(is_receipt & F.col("amount_minor").isNotNull(), 1).otherwise(0)
            ).alias("receipts_with_amount"),
        )
        .select("tenant_id", "day", "channel", "documents", "pages", "receipts", "receipts_with_amount")
    )


def main() -> None:
    spark = build_spark()
    try:
        ensure_target_tables(spark)

        findings = read_recon_findings(spark)
        if findings is not None:
            resolutions = read_recon_resolutions(spark)
            recon_daily = compute_recon_daily(findings, resolutions)
            # Dynamic partition overwrite: idempotent re-runs replace only
            # touched day partitions.
            recon_daily.writeTo(RECON_DAILY_TABLE).overwritePartitions()
            print(f"[gold] wrote {recon_daily.count()} rows to {RECON_DAILY_TABLE}")

        documents = read_receipt_documents(spark)
        if documents is not None:
            ingest = read_receipt_ingest(spark)
            ingest_daily = compute_ingest_daily(documents, ingest)
            ingest_daily.writeTo(INGEST_DAILY_TABLE).overwritePartitions()
            print(f"[gold] wrote {ingest_daily.count()} rows to {INGEST_DAILY_TABLE}")
    finally:
        spark.stop()


if __name__ == "__main__":
    main()
