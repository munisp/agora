"""seed_coverage — synthetic radio-coverage grid for cac_silver.coverage
(SPEC-W17, Agent C).

Generates 774 LGAs x 200 synthetic radio-station rows (scaled by SEED_SCALE)
with deterministic station codes NG-<STATE>-<nnn>, band FM/AM, frequency and
transmitter-site coordinates, and loads them into
`iceberg.cac_silver.coverage` (identity-partitioned by state, dynamic
partition overwrite on reseed — same idempotent idiom as the W13 gold jobs).

SUBSTITUTION (documented per SPEC-W17): a real NBC (National Broadcasting
Commission) station register is unavailable offline, so stations are
synthetic and deterministic. Below-the-line radio rows let the CAC channel
mix (Agent A's seed_channels.py) be spatially joined against LGAs.

The LGA list comes from an optional parquet extract of Postgres cac.lgas
(Agent A; SEED_LGA_CENTROIDS_PATH). Offline / pre-Agent-A runs fall back to
the embedded 36-states + FCT anchors from seed_geo_points (ASSUMPTION,
logged loudly) so the job stays runnable DB-free; the pure generator accepts
any LGA list, which is how the tests drive it.

Pure-function core is Spark-free and unit-testable without a Spark session;
the pyspark import is guarded (same pattern as cac_analytics.py).

Run:

  docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
    --master spark://spark-master:7077 \
    /opt/spark-jobs/seed_coverage.py [--scale 0.05] [--dry-run]
"""

from __future__ import annotations

import argparse
import hashlib
import os
import random
import sys

try:  # driver-less CI boxes have no pyspark — pure functions stay importable.
    from pyspark.sql import DataFrame, SparkSession
    from pyspark.sql import functions as F
    from pyspark.sql import types as T
except ImportError:  # pragma: no cover - exercised implicitly on CI lint boxes
    DataFrame = SparkSession = None
    F = T = None

# Sibling pure module (same jobs dir); only its constants/pure helpers are
# used, so this import is Spark-free as well.
try:
    from seed_geo_points import (
        NIGERIA_BBOX,
        default_anchors,
        deterministic_id,
        _hash_unit_interval,
    )
except ImportError:  # pragma: no cover - allows package-style imports
    from .seed_geo_points import (  # type: ignore[no-redef]
        NIGERIA_BBOX,
        default_anchors,
        deterministic_id,
        _hash_unit_interval,
    )

# ---------------------------------------------------------------------------
# Constants / env
# ---------------------------------------------------------------------------

DEFAULT_PER_LGA = 200
MASTER_SEED = "seed-coverage-v1"

SEED_SALT = os.getenv("SEED_SALT", "opendesk-dev-seed-salt-change-in-prod")
SEED_SCALE = float(os.getenv("SEED_SCALE", "1.0"))

COVERAGE_TABLE = os.getenv("SEED_COVERAGE_TABLE", "iceberg.cac_silver.coverage")
LGA_CENTROIDS_PATH = os.getenv(
    "SEED_LGA_CENTROIDS_PATH", "s3://lake/extracts/lga_centroids/"
)

FM_MIN_MHZ, FM_MAX_MHZ = 87.5, 108.0
AM_MIN_MHZ, AM_MAX_MHZ = 0.531, 1.602
AM_SHARE = 0.15  # deterministic ~15% AM allocation


# ---------------------------------------------------------------------------
# Pure-function core (Spark-free)
# ---------------------------------------------------------------------------

def state_slug(state: str) -> str:
    """Station-code state token: uppercase alnum, e.g. 'Cross River' -> CROSSRIVER."""
    return "".join(ch for ch in state.upper() if ch.isalnum())


def fallback_lgas() -> list[dict]:
    """Embedded state anchors reshaped as LGA-like rows (offline fallback)."""
    return [
        {
            "lga_id": a["anchor_id"],
            "name": a["name"],
            "state": a["state"],
            "lat": a["lat"],
            "lng": a["lng"],
        }
        for a in default_anchors()
    ]


def generate_stations(
    lgas: list[dict] | None = None,
    per_lga: int = DEFAULT_PER_LGA,
    scale: float = 1.0,
    salt: str = SEED_SALT,
    master_seed: str = MASTER_SEED,
) -> list[dict]:
    """Generate deterministic station rows for every LGA.

    Exactly int(per_lga * scale) rows per LGA. Station codes are
    NG-<STATE>-<nnn> with <nnn> a per-state running sequence (zero-padded,
    width grows past 999 if needed), assigned in sorted LGA order so codes
    are stable across runs. Band is AM when the row hash falls in the first
    AM_SHARE of the unit interval, else FM; frequency/power/site-jitter all
    derive from the same deterministic per-row RNG.
    """
    lgas = lgas if lgas is not None else fallback_lgas()
    count_per_lga = int(per_lga * scale)
    min_lat, max_lat, min_lng, max_lng = NIGERIA_BBOX
    ordered = sorted(lgas, key=lambda l: (state_slug(l["state"]), l["name"]))
    rows: list[dict] = []
    seq_by_state: dict[str, int] = {}
    for lga in ordered:
        slug = state_slug(lga["state"])
        for i in range(count_per_lga):
            seq_by_state[slug] = seq_by_state.get(slug, 0) + 1
            seq = seq_by_state[slug]
            rng = random.Random(
                int(
                    hashlib.sha256(
                        f"{master_seed}|{lga['lga_id']}|{i}".encode("utf-8")
                    ).hexdigest()[:16],
                    16,
                )
            )
            band_pick = _hash_unit_interval("band", lga["lga_id"], str(i))
            if band_pick < AM_SHARE:
                band = "AM"
                frequency = round(
                    AM_MIN_MHZ
                    + round(rng.uniform(0, (AM_MAX_MHZ - AM_MIN_MHZ) / 0.009)) * 0.009,
                    3,
                )
            else:
                band = "FM"
                frequency = round(
                    FM_MIN_MHZ
                    + round(rng.uniform(0, (FM_MAX_MHZ - FM_MIN_MHZ) / 0.1)) * 0.1,
                    1,
                )
            power_kw = round(rng.uniform(1.0, 100.0), 1)
            lat = min(max(lga["lat"] + rng.uniform(-0.15, 0.15), min_lat), max_lat)
            lng = min(max(lga["lng"] + rng.uniform(-0.15, 0.15), min_lng), max_lng)
            station_id = deterministic_id(
                f"station:{lga['lga_id']}:{i}", salt=salt
            )
            rows.append(
                {
                    "station_id": station_id,
                    "station_code": f"NG-{slug}-{seq:03d}",
                    "lga_id": lga["lga_id"],
                    "lga_name": lga["name"],
                    "state": lga["state"],
                    "band": band,
                    "frequency": frequency,
                    "power_kw": power_kw,
                    "lat": round(lat, 6),
                    "lng": round(lng, 6),
                    "geom": f"POINT({lng:.6f} {lat:.6f})",
                    "is_synthetic": True,
                    "seeded_at": None,
                }
            )
    return rows


# ---------------------------------------------------------------------------
# Spark layer (guarded)
# ---------------------------------------------------------------------------

COVERAGE_SCHEMA = (
    T.StructType(
        [
            T.StructField("station_id", T.StringType()),
            T.StructField("station_code", T.StringType()),
            T.StructField("lga_id", T.StringType()),
            T.StructField("lga_name", T.StringType()),
            T.StructField("state", T.StringType()),
            T.StructField("band", T.StringType()),
            T.StructField("frequency", T.DoubleType()),
            T.StructField("power_kw", T.DoubleType()),
            T.StructField("lat", T.DoubleType()),
            T.StructField("lng", T.DoubleType()),
            T.StructField("geom", T.StringType()),
            T.StructField("is_synthetic", T.BooleanType()),
            T.StructField("seeded_at", T.TimestampType()),
        ]
    )
    if T is not None
    else None
)


def load_lgas(spark) -> list[dict]:
    """cac.lgas centroid extract if present; embedded fallback otherwise."""
    try:
        df = spark.read.parquet(LGA_CENTROIDS_PATH)
        lgas = [
            {
                "lga_id": row.lga_id,
                "name": row.name,
                "state": row.state,
                "lat": float(row.lat),
                "lng": float(row.lng),
            }
            for row in df.collect()
        ]
        if lgas:
            print(f"[seed_coverage] loaded {len(lgas)} LGAs from {LGA_CENTROIDS_PATH}")
            return lgas
    except Exception as exc:  # noqa: BLE001 - graceful degradation
        print(f"[seed_coverage] WARN: LGA extract unreadable ({exc}); embedded fallback")
    lgas = fallback_lgas()
    print(
        f"[seed_coverage] ASSUMPTION: no cac.lgas extract — using {len(lgas)} "
        "embedded state anchors as LGA stand-ins"
    )
    return lgas


def ensure_target_table(spark) -> None:
    spark.sql("CREATE NAMESPACE IF NOT EXISTS iceberg.cac_silver")
    spark.sql(
        f"""
        CREATE TABLE IF NOT EXISTS {COVERAGE_TABLE} (
            station_id    STRING,
            station_code  STRING,
            lga_id        STRING,
            lga_name      STRING,
            state         STRING,
            band          STRING,
            frequency     DOUBLE,
            power_kw      DOUBLE,
            lat           DOUBLE,
            lng           DOUBLE,
            geom          STRING,
            is_synthetic  BOOLEAN,
            seeded_at     TIMESTAMP
        ) USING iceberg
        PARTITIONED BY (state)
        """
    )


def run_job(scale: float = SEED_SCALE, dry_run: bool = False) -> int:
    """Spark entrypoint. Returns the number of station rows generated.

    --dry-run is driver-less friendly (contract B): without a Spark stack it
    still prints counts from the embedded fallback LGAs.
    """
    try:
        from sedona_common import build_sedona_context  # lazy import, CI-safe

        sedona = build_sedona_context(app_name="opendesk-seed-coverage")
        spark = sedona.sparkSession if hasattr(sedona, "sparkSession") else sedona
    except ImportError:
        if not dry_run:
            raise
        spark = None
        print("[seed_coverage] WARN: no Spark stack — dry-run on embedded LGAs")

    lgas = load_lgas(spark) if spark is not None else fallback_lgas()
    rows = generate_stations(lgas=lgas, scale=scale)
    print(f"[seed_coverage] generated {len(rows)} station rows (scale={scale})")

    if dry_run:
        print(f"[seed_coverage] DRY-RUN: {len(rows)} rows, no writes")
        return len(rows)

    df = spark.createDataFrame(rows, schema=COVERAGE_SCHEMA).withColumn(
        "seeded_at", F.current_timestamp()
    )
    ensure_target_table(spark)
    spark.sql(f"REFRESH TABLE {COVERAGE_TABLE}")
    df.select(*[f.name for f in COVERAGE_SCHEMA.fields]).writeTo(
        COVERAGE_TABLE
    ).overwritePartitions()
    print(f"[seed_coverage] wrote {len(rows)} rows to {COVERAGE_TABLE}")
    return len(rows)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Seed cac_silver.coverage")
    parser.add_argument("--scale", type=float, default=SEED_SCALE)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args(argv)
    try:
        run_job(scale=args.scale, dry_run=args.dry_run)
    except Exception as exc:  # noqa: BLE001 - fail loud per contract B
        print(f"[seed_coverage] FAILED: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
