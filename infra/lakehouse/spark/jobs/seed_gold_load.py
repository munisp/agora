"""seed_gold_load — load seed manifests into cac_gold.* (SPEC-W17, Agent C).

Loads, into the Iceberg gold namespace (NOT Delta Lake — standing ruling):

  * Agent A's channel unit-cost manifest  -> iceberg.cac_gold.channel_unit_costs
      JSON rows: {month "YYYY-MM-DD" (first of month), channel_code,
                  channel_name, channel_class, unit_cost_ngn, currency}
  * Agent B's FX manifest (seed_fx.py — plain Python job, Langflow dropped
    per SPEC-W17 adaptation #4)     -> iceberg.cac_gold.usd_shadow_prices
      JSON rows: {day "YYYY-MM-DD", official_ngn, parallel_ngn}
      shadow_mid and spread_bps are derived here (pure, tested).

MANIFEST CONTRACT (cross-agent, FLAGGED): manifests are newline-delimited
JSON or a JSON array under SEED_MANIFEST_DIR (default
/var/tmp/seed_manifests/), files channel_unit_costs.json and fx_series.json —
the same JSONL outbox directory _lib.emit_seed_report uses when
SEED_KAFKA=off, so CI runs need no Kafka and no object store for the
handoff. Field names above are the contract; this job tolerates
`rate_official`/`rate_parallel` aliases from the FX writer.

Write idiom: MERGE/overwritePartitions from W13 cac_analytics.py — the gold
tables are partitioned by days(month)/days(day), so reseeds dynamically
overwrite exactly the temporal partitions present in the manifest
(idempotent). Every run appends one row per loaded table to
iceberg.cac_gold.seed_run_log (lakehouse mirror of Postgres cac.seed_run_log,
contract A log_seed_run).

Graceful degradation mirrors cac_analytics.py: a missing/unreadable manifest
logs a warning and loads the other table instead of failing the pipeline.
--dry-run prints rowcounts with no writes.

Run:

  docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
    --master spark://spark-master:7077 \
    /opt/spark-jobs/seed_gold_load.py [--dry-run]
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from datetime import datetime, timezone

try:  # driver-less CI boxes have no pyspark — pure functions stay importable.
    from pyspark.sql import DataFrame, SparkSession
    from pyspark.sql import functions as F
    from pyspark.sql import types as T
except ImportError:  # pragma: no cover - exercised implicitly on CI lint boxes
    DataFrame = SparkSession = None
    F = T = None

try:
    from seed_geo_points import deterministic_id
except ImportError:  # pragma: no cover - allows package-style imports
    from .seed_geo_points import deterministic_id  # type: ignore[no-redef]

# ---------------------------------------------------------------------------
# Constants / env
# ---------------------------------------------------------------------------

SEED_SALT = os.getenv("SEED_SALT", "opendesk-dev-seed-salt-change-in-prod")
SEED_SCALE = float(os.getenv("SEED_SCALE", "1.0"))

MANIFEST_DIR = os.getenv("SEED_MANIFEST_DIR", "/var/tmp/seed_manifests")
CHANNEL_COSTS_MANIFEST = os.getenv(
    "SEED_CHANNEL_COSTS_MANIFEST",
    os.path.join(MANIFEST_DIR, "channel_unit_costs.json"),
)
FX_MANIFEST = os.getenv(
    "SEED_FX_MANIFEST", os.path.join(MANIFEST_DIR, "fx_series.json")
)

COSTS_TABLE = os.getenv("SEED_GOLD_COSTS_TABLE", "iceberg.cac_gold.channel_unit_costs")
FX_TABLE = os.getenv("SEED_GOLD_FX_TABLE", "iceberg.cac_gold.usd_shadow_prices")
RUN_LOG_TABLE = os.getenv("SEED_GOLD_RUN_LOG_TABLE", "iceberg.cac_gold.seed_run_log")


# ---------------------------------------------------------------------------
# Pure-function core (Spark-free)
# ---------------------------------------------------------------------------

def load_manifest(path: str) -> list[dict]:
    """Read a seed manifest in any of the three on-the-wire shapes:

      1. the writers' pretty-printed JSON ENVELOPE — the real handoff format
         emitted by scripts/seeds/seed_channel_costs.write_manifest()
         ({"table", "rowcount", "generator", "rows": [...]}) and
         scripts/seeds/seed_fx.build_fx_manifest()
         ({"manifest_version", ..., "rows": [...]});
      2. a bare top-level JSON array of rows;
      3. newline-delimited JSON (one row object per line).

    Missing file -> [] (caller logs, graceful degradation). A dict envelope
    whose "rows" is missing or not a list raises ValueError — the file exists
    but is not a valid manifest, and silently loading zero rows would mask a
    broken handoff.
    """
    if not os.path.exists(path):
        return []
    with open(path, "r", encoding="utf-8") as fh:
        text = fh.read().strip()
    if not text:
        return []
    if text.startswith("{"):
        # Single-object envelope — or JSONL, whose lines also start with '{'.
        try:
            payload = json.loads(text)
        except json.JSONDecodeError:
            payload = None  # fall through to JSONL parsing below
        if isinstance(payload, dict):
            rows = payload.get("rows")
            if not isinstance(rows, list):
                raise ValueError(
                    f"manifest envelope at {path} has no 'rows' list "
                    f"(keys: {sorted(payload)})"
                )
            return list(rows)
        if payload is not None:
            raise ValueError(
                f"manifest at {path} is a JSON {type(payload).__name__}, "
                "expected an envelope object, a rows array, or JSONL"
            )
    elif text.startswith("["):
        return list(json.loads(text))
    return [json.loads(line) for line in text.splitlines() if line.strip()]


def normalize_cost_rows(rows: list[dict]) -> list[dict]:
    """Project channel-cost manifest rows onto the gold schema."""
    out = []
    for r in rows:
        out.append(
            {
                "month": str(r["month"])[:10],
                "channel_code": r["channel_code"],
                "channel_name": r.get("channel_name", r["channel_code"]),
                "channel_class": r.get("channel_class", "below-the-line"),
                "unit_cost_ngn": float(r["unit_cost_ngn"]),
                "currency": r.get("currency", "NGN"),
                "is_synthetic": True,
            }
        )
    return out


def normalize_fx_rows(rows: list[dict]) -> list[dict]:
    """Project FX manifest rows onto the gold schema, deriving shadow fields.

    shadow_mid = (official + parallel) / 2
    spread_bps = (parallel - official) / official * 1e4
    """
    out = []
    for r in rows:
        official = float(r.get("official_ngn", r.get("rate_official")))
        parallel = float(r.get("parallel_ngn", r.get("rate_parallel")))
        out.append(
            {
                "day": str(r["day"])[:10],
                "official_ngn": official,
                "parallel_ngn": parallel,
                "shadow_mid": round((official + parallel) / 2.0, 4),
                "spread_bps": round((parallel - official) / official * 1e4, 2)
                if official
                else None,
                "is_synthetic": True,
            }
        )
    return out


def git_sha() -> str:
    """Best-effort short git sha; 'unknown' outside a checkout (never fails)."""
    try:
        return (
            subprocess.check_output(
                ["git", "rev-parse", "--short", "HEAD"], stderr=subprocess.DEVNULL
            )
            .decode()
            .strip()
        )
    except Exception:  # noqa: BLE001 - best effort only
        return "unknown"


def seed_run_log_row(
    table_name: str,
    rowcount: int,
    runner_id: str,
    sha: str,
    scale: float = SEED_SCALE,
    status: str = "ok",
    salt: str = SEED_SALT,
) -> dict:
    """One cac_gold.seed_run_log row (contract A log_seed_run, lakehouse side)."""
    return {
        "run_id": deterministic_id(f"seed-run:{table_name}:{sha}:{runner_id}", salt=salt),
        "table_name": table_name,
        "rowcount": int(rowcount),
        "runner_id": runner_id,
        "git_sha": sha,
        "seed_scale": float(scale),
        "status": status,
    }


# ---------------------------------------------------------------------------
# Spark layer (guarded)
# ---------------------------------------------------------------------------

COSTS_SCHEMA = (
    T.StructType(
        [
            T.StructField("month", T.StringType()),
            T.StructField("channel_code", T.StringType()),
            T.StructField("channel_name", T.StringType()),
            T.StructField("channel_class", T.StringType()),
            T.StructField("unit_cost_ngn", T.DoubleType()),
            T.StructField("currency", T.StringType()),
            T.StructField("is_synthetic", T.BooleanType()),
        ]
    )
    if T is not None
    else None
)

FX_SCHEMA = (
    T.StructType(
        [
            T.StructField("day", T.StringType()),
            T.StructField("official_ngn", T.DoubleType()),
            T.StructField("parallel_ngn", T.DoubleType()),
            T.StructField("shadow_mid", T.DoubleType()),
            T.StructField("spread_bps", T.DoubleType()),
            T.StructField("is_synthetic", T.BooleanType()),
        ]
    )
    if T is not None
    else None
)

RUN_LOG_SCHEMA = (
    T.StructType(
        [
            T.StructField("run_id", T.StringType()),
            T.StructField("table_name", T.StringType()),
            T.StructField("rowcount", T.LongType()),
            T.StructField("runner_id", T.StringType()),
            T.StructField("git_sha", T.StringType()),
            T.StructField("seed_scale", T.DoubleType()),
            T.StructField("status", T.StringType()),
        ]
    )
    if T is not None
    else None
)


def ensure_target_tables(spark) -> None:
    """Same CREATE TABLE IF NOT EXISTS idiom as cac_analytics.ensure_target_tables."""
    spark.sql("CREATE NAMESPACE IF NOT EXISTS iceberg.cac_gold")
    spark.sql(
        f"""
        CREATE TABLE IF NOT EXISTS {COSTS_TABLE} (
            month         DATE,
            channel_code  STRING,
            channel_name  STRING,
            channel_class STRING,
            unit_cost_ngn DOUBLE,
            currency      STRING,
            is_synthetic  BOOLEAN,
            seeded_at     TIMESTAMP
        ) USING iceberg
        PARTITIONED BY (days(month))
        """
    )
    spark.sql(
        f"""
        CREATE TABLE IF NOT EXISTS {FX_TABLE} (
            day           DATE,
            official_ngn  DOUBLE,
            parallel_ngn  DOUBLE,
            shadow_mid    DOUBLE,
            spread_bps    DOUBLE,
            is_synthetic  BOOLEAN,
            seeded_at     TIMESTAMP
        ) USING iceberg
        PARTITIONED BY (days(day))
        """
    )
    spark.sql(
        f"""
        CREATE TABLE IF NOT EXISTS {RUN_LOG_TABLE} (
            run_id        STRING,
            table_name    STRING,
            rowcount      BIGINT,
            runner_id     STRING,
            git_sha       STRING,
            seed_scale    DOUBLE,
            status        STRING,
            seeded_at     TIMESTAMP
        ) USING iceberg
        PARTITIONED BY (days(seeded_at))
        """
    )


def _append_run_log(spark, rows: list[dict], runner_id: str, sha: str, dry_run: bool) -> None:
    log_rows = [
        seed_run_log_row(t, n, runner_id, sha, status="dry-run" if dry_run else "ok")
        for (t, n) in rows
    ]
    if not log_rows:
        return
    df = spark.createDataFrame(log_rows, schema=RUN_LOG_SCHEMA).withColumn(
        "seeded_at", F.current_timestamp()
    )
    df.select(*[f.name for f in RUN_LOG_SCHEMA.fields], "seeded_at").writeTo(
        RUN_LOG_TABLE
    ).append()


def run_job(dry_run: bool = False) -> dict:
    """Spark entrypoint. Returns {table: rowcount} actually loaded.

    --dry-run is fully driver-less (contract B): manifests are plain files,
    so counts print without any Spark session.
    """
    runner_id = os.getenv("SEED_RUNNER_ID", f"seed_gold_load-{os.getpid()}")
    sha = git_sha()
    loaded: dict[str, int] = {}

    raw_costs = load_manifest(CHANNEL_COSTS_MANIFEST)
    if not raw_costs:
        print(f"[seed_gold_load] WARN: no channel-cost manifest at {CHANNEL_COSTS_MANIFEST}")
    raw_fx = load_manifest(FX_MANIFEST)
    if not raw_fx:
        print(f"[seed_gold_load] WARN: no FX manifest at {FX_MANIFEST}")

    if dry_run:
        print(
            f"[seed_gold_load] DRY-RUN: channel_unit_costs={len(raw_costs)} "
            f"usd_shadow_prices={len(raw_fx)} — no writes"
        )
        return {COSTS_TABLE: len(raw_costs), FX_TABLE: len(raw_fx)}

    from sedona_common import build_sedona_context  # lazy import, CI-safe

    sedona = build_sedona_context(app_name="opendesk-seed-gold-load")
    spark = sedona.sparkSession if hasattr(sedona, "sparkSession") else sedona

    ensure_target_tables(spark)

    if raw_costs:
        costs = normalize_cost_rows(raw_costs)
        df = (
            spark.createDataFrame(costs, schema=COSTS_SCHEMA)
            .withColumn("month", F.to_date("month"))
            .withColumn("seeded_at", F.current_timestamp())
        )
        spark.sql(f"REFRESH TABLE {COSTS_TABLE}")
        df.writeTo(COSTS_TABLE).overwritePartitions()  # W13 idiom: dynamic partition overwrite
        loaded[COSTS_TABLE] = len(costs)
        print(f"[seed_gold_load] loaded {len(costs)} rows into {COSTS_TABLE}")

    if raw_fx:
        fx = normalize_fx_rows(raw_fx)
        df = (
            spark.createDataFrame(fx, schema=FX_SCHEMA)
            .withColumn("day", F.to_date("day"))
            .withColumn("seeded_at", F.current_timestamp())
        )
        spark.sql(f"REFRESH TABLE {FX_TABLE}")
        df.writeTo(FX_TABLE).overwritePartitions()
        loaded[FX_TABLE] = len(fx)
        print(f"[seed_gold_load] loaded {len(fx)} rows into {FX_TABLE}")

    _append_run_log(spark, sorted(loaded.items()), runner_id, sha, dry_run=False)
    return loaded


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Load seed manifests into cac_gold.*")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args(argv)
    try:
        run_job(dry_run=args.dry_run)
    except Exception as exc:  # noqa: BLE001 - fail loud per contract B
        print(f"[seed_gold_load] FAILED: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
