"""seed_geo_points — synthetic settlement points for cac_silver.geo_points
(SPEC-W17, Agent C; adaptation of "Section 8" geo seeding to the Iceberg
lakehouse — NOT Delta Lake, per standing OpenDesk ruling).

Generates 50,000 x SEED_SCALE synthetic settlement points anchored on
Nigerian LGA polygons and writes them as GeoParquet plus an Iceberg mirror
table `iceberg.cac_silver.geo_points` (geometry as WKT — Iceberg has no
geometry type, same ruling as cac_analytics.py).

LGA polygon source, in order:
  1. SEED_LGA_GEOJSON_PATH / SEED_LGA_CENTROIDS_PATH extract on MinIO
     (GeoJSON FeatureCollection or parquet with lga_id/name/state/centroid),
     exported from Postgres cac.lgas (Agent A) or an offline Geofabrik/OSM
     mirror when one is mounted;
  2. FALLBACK (ASSUMPTION — annotated): when no Geofabrik/OSM mirror is
     available offline, the job GENERATES the polygons itself: a small
     deterministic convex polygon around each anchor centroid
     (synthesize_polygon), and writes that generated polygon set out as a
     GeoJSON FeatureCollection (SEED_GEOJSON_OUT_PATH) so downstream tools can
     inspect exactly what was sampled against. With no extract at all, the
     embedded 36-states + FCT centroid list below is used as anchors. These
     are synthetic anchor geometries, NOT real administrative boundaries.

Pure-function core (everything above the Spark section) is Spark-free and
unit-testable on driver-less CI boxes: deterministic seeding, point-in-polygon
rejection sampling, exact-count allocation. The pyspark import is guarded so
importing this module never requires pyspark (same pattern as
cac_analytics.py / the lazy sedona import in sedona_common.py).

Idempotency: point_ids are deterministic (sha256 of salt|natural-key, same
construction as scripts/seeds/_lib.py contract A); the Iceberg table is
identity-partitioned by state and reseeds use dynamic partition overwrite
(overwritePartitions), so a reseed replaces exactly the states it emits.

Run:

  docker exec opendesk-spark-master /opt/bitnami/spark/bin/spark-submit \
    --master spark://spark-master:7077 \
    /opt/spark-jobs/seed_geo_points.py [--scale 0.05] [--dry-run]
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import random
import sys
from datetime import datetime, timezone

try:  # driver-less CI boxes have no pyspark — pure functions stay importable.
    from pyspark.sql import DataFrame, SparkSession
    from pyspark.sql import functions as F
    from pyspark.sql import types as T
except ImportError:  # pragma: no cover - exercised implicitly on CI lint boxes
    DataFrame = SparkSession = None
    F = T = None

# ---------------------------------------------------------------------------
# Constants / env
# ---------------------------------------------------------------------------

# Nigeria mainland bounding box (EPSG:4326). Every seeded point MUST fall
# inside this box (test-enforced).
NIGERIA_BBOX = (4.27, 13.89, 2.66, 14.68)  # (min_lat, max_lat, min_lng, max_lng)

DEFAULT_TOTAL = 50_000
MASTER_SEED = "seed-geo-points-v1"

# Mirrors scripts/seeds/_lib.py contract A: SEED_SALT env with a fixed dev
# default. NOTE (cross-agent): _lib.py is Agent A's file; the constant name
# and construction below intentionally match contract A so ids line up when
# both sides hash the same natural key. Dev default documented in .env.example.
SEED_SALT = os.getenv("SEED_SALT", "opendesk-dev-seed-salt-change-in-prod")

SEED_SCALE = float(os.getenv("SEED_SCALE", "1.0"))

GEO_TABLE = os.getenv("SEED_GEO_POINTS_TABLE", "iceberg.cac_silver.geo_points")
GEOPARQUET_PATH = os.getenv(
    "SEED_GEOPARQUET_PATH", "s3://lake/extracts/seed_geo_points_geoparquet/"
)
GEOJSON_OUT_PATH = os.getenv(
    "SEED_GEOJSON_OUT_PATH", "s3://lake/extracts/seed_lga_polygons.geojson"
)
LGA_GEOJSON_PATH = os.getenv("SEED_LGA_GEOJSON_PATH", "")  # optional input
LGA_CENTROIDS_PATH = os.getenv(
    "SEED_LGA_CENTROIDS_PATH", "s3://lake/extracts/lga_centroids/"
)
H3_RESOLUTION = int(os.getenv("SEED_H3_RESOLUTION", "8"))

# Embedded fallback anchors: 36 states + FCT with approximate capital
# centroids and geo-political zone. Well-known public reference data; used
# only when no LGA extract is available (ASSUMPTION documented above).
FALLBACK_ANCHORS = [
    # (name, state, zone, lat, lng)
    ("Abia", "Abia", "SE", 5.53, 7.49),
    ("Adamawa", "Adamawa", "NE", 9.20, 12.48),
    ("Akwa Ibom", "Akwa Ibom", "SS", 5.03, 7.93),
    ("Anambra", "Anambra", "SE", 6.22, 7.07),
    ("Bauchi", "Bauchi", "NE", 10.31, 9.84),
    ("Bayelsa", "Bayelsa", "SS", 4.93, 6.26),
    ("Benue", "Benue", "NC", 7.73, 8.54),
    ("Borno", "Borno", "NE", 11.85, 13.16),
    ("Cross River", "Cross River", "SS", 4.98, 8.33),
    ("Delta", "Delta", "SS", 6.20, 6.70),
    ("Ebonyi", "Ebonyi", "SE", 6.32, 8.11),
    ("Edo", "Edo", "SS", 6.34, 5.63),
    ("Ekiti", "Ekiti", "SW", 7.62, 5.22),
    ("Enugu", "Enugu", "SE", 6.45, 7.50),
    ("FCT", "FCT", "NC", 9.06, 7.49),
    ("Gombe", "Gombe", "NE", 10.29, 11.17),
    ("Imo", "Imo", "SE", 5.49, 7.03),
    ("Jigawa", "Jigawa", "NW", 11.76, 9.34),
    ("Kaduna", "Kaduna", "NW", 10.52, 7.44),
    ("Kano", "Kano", "NW", 12.00, 8.59),
    ("Katsina", "Katsina", "NW", 12.99, 7.60),
    ("Kebbi", "Kebbi", "NW", 12.45, 4.20),
    ("Kogi", "Kogi", "NC", 7.80, 6.74),
    ("Kwara", "Kwara", "NC", 8.50, 4.55),
    ("Lagos", "Lagos", "SW", 6.52, 3.38),
    ("Nasarawa", "Nasarawa", "NC", 8.49, 8.52),
    ("Niger", "Niger", "NC", 9.61, 6.56),
    ("Ogun", "Ogun", "SW", 7.15, 3.35),
    ("Ondo", "Ondo", "SW", 7.25, 5.19),
    ("Osun", "Osun", "SW", 7.77, 4.56),
    ("Oyo", "Oyo", "SW", 7.38, 3.90),
    ("Plateau", "Plateau", "NC", 9.90, 8.89),
    ("Rivers", "Rivers", "SS", 4.82, 7.03),
    ("Sokoto", "Sokoto", "NW", 13.06, 5.24),
    ("Taraba", "Taraba", "NE", 8.89, 11.36),
    ("Yobe", "Yobe", "NE", 11.75, 11.96),
    ("Zamfara", "Zamfara", "NW", 12.17, 6.66),
]


# ---------------------------------------------------------------------------
# Pure-function core (Spark-free — unit-testable without a Spark session)
# ---------------------------------------------------------------------------

def deterministic_id(natural_key: str, salt: str = SEED_SALT) -> str:
    """sha256(SEED_SALT + "|" + natural_key) hex — mirrors contract A (_lib)."""
    return hashlib.sha256(f"{salt}|{natural_key}".encode("utf-8")).hexdigest()


def default_anchors() -> list[dict]:
    """The embedded 36-states + FCT fallback anchors as dicts."""
    return [
        {
            "anchor_id": deterministic_id(f"lga-anchor:{name}"),
            "name": name,
            "state": state,
            "zone": zone,
            "lat": lat,
            "lng": lng,
        }
        for (name, state, zone, lat, lng) in FALLBACK_ANCHORS
    ]


def _hash_unit_interval(*parts: str) -> float:
    """Deterministic pseudo-random float in [0, 1) from string parts."""
    digest = hashlib.sha256("|".join(parts).encode("utf-8")).hexdigest()
    return int(digest[:12], 16) / float(0xFFFFFFFFFFFF)


def synthesize_polygon(anchor: dict) -> list[tuple[float, float]]:
    """Deterministic convex polygon around an anchor centroid.

    ASSUMPTION (offline substitute for Geofabrik/OSM LGA boundaries): an
    8-vertex irregular convex ring whose radius derives from the anchor name
    hash, sized 0.05-0.30 degrees and clipped to the Nigeria bbox. Vertices
    are (lng, lat) pairs forming a closed ring. Synthetic geometry — NOT a
    real administrative boundary.
    """
    min_lat, max_lat, min_lng, max_lng = NIGERIA_BBOX
    base_r = 0.05 + 0.25 * _hash_unit_interval("radius", anchor["name"])
    ring: list[tuple[float, float]] = []
    n = 8
    for i in range(n):
        angle = 2.0 * math.pi * i / n
        jitter = 0.7 + 0.6 * _hash_unit_interval("vtx", anchor["name"], str(i))
        lat = anchor["lat"] + base_r * jitter * math.sin(angle)
        # longitude degrees shrink with latitude (cos factor) so the ring is
        # roughly isotropic on the ground.
        lng = anchor["lng"] + base_r * jitter * math.cos(angle) / max(
            math.cos(math.radians(anchor["lat"])), 0.2
        )
        lat = min(max(lat, min_lat + 0.01), max_lat - 0.01)
        lng = min(max(lng, min_lng + 0.01), max_lng - 0.01)
        ring.append((lng, lat))
    ring.append(ring[0])  # close the ring
    return ring


def anchors_to_geojson(anchors: list[dict]) -> dict:
    """FeatureCollection of the (possibly synthesized) anchor polygons."""
    features = []
    for a in anchors:
        ring = a.get("polygon") or synthesize_polygon(a)
        features.append(
            {
                "type": "Feature",
                "properties": {
                    "anchor_id": a["anchor_id"],
                    "name": a["name"],
                    "state": a["state"],
                    "zone": a.get("zone", ""),
                    "is_synthetic": True,
                },
                "geometry": {
                    "type": "Polygon",
                    "coordinates": [[[lng, lat] for (lng, lat) in ring]],
                },
            }
        )
    return {"type": "FeatureCollection", "features": features}


def point_in_polygon(lng: float, lat: float, ring: list[tuple[float, float]]) -> bool:
    """Planar ray-casting point-in-polygon over a closed (lng, lat) ring.

    Planar is correct here because Sedona's ST_Within on EPSG:4326 lon/lat is
    likewise planar (documented in geo_analytics.py) — the pure core and the
    Spark layer agree by construction.
    """
    inside = False
    n = len(ring)
    j = n - 1
    for i in range(n):
        xi, yi = ring[i]
        xj, yj = ring[j]
        if ((yi > lat) != (yj > lat)) and (
            lng < (xj - xi) * (lat - yi) / ((yj - yi) or 1e-12) + xi
        ):
            inside = not inside
        j = i
    return inside


def random_point_in_polygon(
    rng: random.Random, ring: list[tuple[float, float]], max_attempts: int = 200
) -> tuple[float, float]:
    """Rejection-sample a (lat, lng) uniformly inside the ring's bbox.

    Falls back to the ring centroid if rejection sampling does not converge
    (only possible for degenerate rings); the centroid of a closed ring is
    always representable and is clipped to the Nigeria bbox by the caller.
    """
    lngs = [p[0] for p in ring]
    lats = [p[1] for p in ring]
    min_lng, max_lng = min(lngs), max(lngs)
    min_lat, max_lat = min(lats), max(lats)
    for _ in range(max_attempts):
        lng = rng.uniform(min_lng, max_lng)
        lat = rng.uniform(min_lat, max_lat)
        if point_in_polygon(lng, lat, ring):
            return lat, lng
    centroid_lat = sum(lats) / len(lats)
    centroid_lng = sum(lngs) / len(lngs)
    return centroid_lat, centroid_lng


def allocate_counts(anchors: list[dict], total: int) -> list[int]:
    """Distribute exactly `total` points across anchors, largest remainder.

    Weights are deterministic pseudo-population weights derived from the
    anchor name hash (Lagos weighted up via a deterministic metro bump), so
    allocations are stable across runs and machines.
    """
    if not anchors or total <= 0:
        return [0] * len(anchors)
    weights = []
    for a in anchors:
        w = 0.2 + _hash_unit_interval("pop", a["name"])
        if a["state"] in ("Lagos", "Kano", "FCT"):  # deterministic metro bump
            w *= 3.0
        weights.append(w)
    wsum = sum(weights)
    raw = [total * w / wsum for w in weights]
    counts = [int(math.floor(r)) for r in raw]
    remainder = total - sum(counts)
    # Largest remainder, tie-broken deterministically by anchor_id.
    order = sorted(
        range(len(anchors)),
        key=lambda i: (-(raw[i] - counts[i]), anchors[i]["anchor_id"]),
    )
    for i in order[:remainder]:
        counts[i] += 1
    return counts


def generate_points(
    anchors: list[dict] | None = None,
    total: int = DEFAULT_TOTAL,
    scale: float = 1.0,
    salt: str = SEED_SALT,
    master_seed: str = MASTER_SEED,
) -> list[dict]:
    """Generate exactly int(total * scale) deterministic settlement points.

    Each row: point_id, anchor_id, anchor_name, state, lat, lng, geom (WKT),
    h3_cell (None — computed by the Spark layer via ST_H3CellIDs),
    is_synthetic=True, seeded_at=None (stamped by the writer). Deterministic:
    same inputs -> identical rows, across runs and machines.
    """
    anchors = anchors if anchors is not None else default_anchors()
    target = int(total * scale)
    counts = allocate_counts(anchors, target)
    min_lat, max_lat, min_lng, max_lng = NIGERIA_BBOX
    rows: list[dict] = []
    for anchor, count in zip(anchors, counts):
        if count <= 0:
            continue
        ring = anchor.get("polygon") or synthesize_polygon(anchor)
        for i in range(count):
            rng = random.Random(
                int(
                    hashlib.sha256(
                        f"{master_seed}|{anchor['anchor_id']}|{i}".encode("utf-8")
                    ).hexdigest()[:16],
                    16,
                )
            )
            lat, lng = random_point_in_polygon(rng, ring)
            lat = min(max(lat, min_lat), max_lat)
            lng = min(max(lng, min_lng), max_lng)
            point_id = deterministic_id(
                f"geo-point:{anchor['anchor_id']}:{i}", salt=salt
            )
            rows.append(
                {
                    "point_id": point_id,
                    "anchor_id": anchor["anchor_id"],
                    "anchor_name": anchor["name"],
                    "state": anchor["state"],
                    "lat": round(lat, 6),
                    "lng": round(lng, 6),
                    "geom": f"POINT({lng:.6f} {lat:.6f})",
                    "h3_cell": None,
                    "is_synthetic": True,
                    "seeded_at": None,
                }
            )
    return rows


# ---------------------------------------------------------------------------
# Spark layer (guarded — only runs under spark-submit with the lakehouse up)
# ---------------------------------------------------------------------------

GEO_SCHEMA = (
    T.StructType(
        [
            T.StructField("point_id", T.StringType()),
            T.StructField("anchor_id", T.StringType()),
            T.StructField("anchor_name", T.StringType()),
            T.StructField("state", T.StringType()),
            T.StructField("lat", T.DoubleType()),
            T.StructField("lng", T.DoubleType()),
            T.StructField("geom", T.StringType()),
            T.StructField("h3_cell", T.LongType()),
            T.StructField("is_synthetic", T.BooleanType()),
            T.StructField("seeded_at", T.TimestampType()),
        ]
    )
    if T is not None
    else None
)


def load_anchors(spark) -> list[dict]:
    """Best-effort LGA anchors from an extract; embedded fallback otherwise.

    Graceful-degradation pattern mirrors cac_analytics.py: a missing or
    unreadable extract logs a warning and falls back to the embedded state
    anchors instead of failing the pipeline.
    """
    if LGA_GEOJSON_PATH:
        try:
            import urllib.request

            with urllib.request.urlopen(LGA_GEOJSON_PATH) as resp:  # nosec - internal extract
                fc = json.loads(resp.read().decode("utf-8"))
            anchors = []
            for feat in fc.get("features", []):
                props = feat.get("properties", {})
                name = props.get("name") or props.get("lga_name")
                if not name:
                    continue
                coords = feat["geometry"]["coordinates"][0]
                ring = [(float(x), float(y)) for x, y in coords]
                lngs = [p[0] for p in ring]
                lats = [p[1] for p in ring]
                anchors.append(
                    {
                        "anchor_id": deterministic_id(f"lga:{name}"),
                        "name": name,
                        "state": props.get("state", ""),
                        "zone": props.get("zone", ""),
                        "lat": sum(lats) / len(lats),
                        "lng": sum(lngs) / len(lngs),
                        "polygon": ring,
                    }
                )
            if anchors:
                print(f"[seed_geo_points] loaded {len(anchors)} anchors from {LGA_GEOJSON_PATH}")
                return anchors
        except Exception as exc:  # noqa: BLE001 - graceful degradation
            print(f"[seed_geo_points] WARN: GeoJSON extract unreadable ({exc}); falling back")
    try:
        df = spark.read.parquet(LGA_CENTROIDS_PATH)
        anchors = [
            {
                "anchor_id": row.lga_id,
                "name": row.name,
                "state": row.state,
                "zone": getattr(row, "zone", "") or "",
                "lat": float(row.lat),
                "lng": float(row.lng),
            }
            for row in df.collect()
        ]
        if anchors:
            print(f"[seed_geo_points] loaded {len(anchors)} anchors from {LGA_CENTROIDS_PATH}")
            return anchors
    except Exception as exc:  # noqa: BLE001 - graceful degradation
        print(f"[seed_geo_points] WARN: centroid extract unreadable ({exc}); embedded fallback")
    anchors = default_anchors()
    print(
        f"[seed_geo_points] ASSUMPTION: no Geofabrik/OSM mirror — using "
        f"{len(anchors)} embedded state anchors with synthesized polygons"
    )
    return anchors


def ensure_target_table(spark) -> None:
    """Same CREATE TABLE IF NOT EXISTS idiom as cac_analytics.ensure_target_tables."""
    spark.sql("CREATE NAMESPACE IF NOT EXISTS iceberg.cac_silver")
    spark.sql(
        f"""
        CREATE TABLE IF NOT EXISTS {GEO_TABLE} (
            point_id      STRING,
            anchor_id     STRING,
            anchor_name   STRING,
            state         STRING,
            lat           DOUBLE,
            lng           DOUBLE,
            geom          STRING,
            h3_cell       BIGINT,
            is_synthetic  BOOLEAN,
            seeded_at     TIMESTAMP
        ) USING iceberg
        PARTITIONED BY (state)
        """
    )


def run_job(scale: float = SEED_SCALE, dry_run: bool = False) -> int:
    """Spark entrypoint. Returns the number of points generated.

    --dry-run is driver-less friendly (contract B): if no Spark/Sedona stack
    is importable it still prints counts from the embedded fallback anchors.
    """
    # Imported lazily so this module stays importable on driver-less lint/CI
    # boxes (same motivation as sedona_common's lazy sedona import).
    try:
        from sedona_common import build_sedona_context

        sedona = build_sedona_context(app_name="opendesk-seed-geo-points")
        spark = sedona.sparkSession if hasattr(sedona, "sparkSession") else sedona
    except ImportError:
        if not dry_run:
            raise
        spark = None
        print("[seed_geo_points] WARN: no Spark stack — dry-run on embedded anchors")

    anchors = load_anchors(spark) if spark is not None else default_anchors()
    rows = generate_points(anchors=anchors, scale=scale)
    print(f"[seed_geo_points] generated {len(rows)} points (scale={scale})")

    if dry_run:
        print(f"[seed_geo_points] DRY-RUN: {len(rows)} rows, no writes")
        return len(rows)

    # Emit the polygon GeoJSON so the synthesized ASSUMPTION geometries are
    # inspectable downstream.
    fc = anchors_to_geojson(anchors)
    try:
        spark.sparkContext.parallelize([json.dumps(fc)]).saveAsTextFile(GEOJSON_OUT_PATH)
    except Exception as exc:  # noqa: BLE001 - report IO must not fail the seed
        print(f"[seed_geo_points] WARN: could not write GeoJSON out ({exc})")

    df = spark.createDataFrame(rows, schema=GEO_SCHEMA).withColumn(
        "seeded_at", F.current_timestamp()
    )
    # H3 res-N cell via the pinned Sedona H3 family (ST_H3CellIDs, >= 1.6.0 —
    # same idiom as geo_analytics.assign_cells).
    df = df.withColumn(
        "h3_cell",
        F.expr(f"ST_H3CellIDs(ST_GeomFromWKT(geom), {H3_RESOLUTION})[0]"),
    )

    # 1. GeoParquet export (Sedona 'geoparquet' writer; geometry column kept
    #    alongside the WKT string). Full overwrite of the export dir.
    try:
        (
            df.withColumn("geometry", F.expr("ST_GeomFromWKT(geom)"))
            .write.format("geoparquet")
            .mode("overwrite")
            .save(GEOPARQUET_PATH)
        )
    except Exception as exc:  # noqa: BLE001 - older Sedona without the format
        print(f"[seed_geo_points] WARN: geoparquet writer unavailable ({exc}); plain parquet")
        df.write.mode("overwrite").parquet(GEOPARQUET_PATH)

    # 2. Iceberg mirror table — dynamic partition overwrite per state, same
    #    idempotent idiom as the W13 gold jobs.
    ensure_target_table(spark)
    spark.sql(f"REFRESH TABLE {GEO_TABLE}")
    df.select(*[f.name for f in GEO_SCHEMA.fields]).writeTo(GEO_TABLE).overwritePartitions()
    print(f"[seed_geo_points] wrote {len(rows)} rows to {GEO_TABLE}")
    return len(rows)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Seed cac_silver.geo_points")
    parser.add_argument("--scale", type=float, default=SEED_SCALE)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args(argv)
    try:
        run_job(scale=args.scale, dry_run=args.dry_run)
    except Exception as exc:  # noqa: BLE001 - fail loud per contract B
        print(f"[seed_geo_points] FAILED: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
