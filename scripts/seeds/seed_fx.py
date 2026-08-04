#!/usr/bin/env python3
"""seed_fx.py — USD/NGN daily FX series into ``cac.fx_series`` (SPEC-W17, Agent B).

Series: 2021-08-01 .. 2026-07-31 (5 years, 1,826 contiguous daily points),
official + parallel-market rates in NGN per USD.

Model (documented substitution per SPEC-W17 header — no live crawling
offline; the uploaded spec's Langflow job becomes this plain Python job):
anchored geometric random walk. Log-price follows an OU-style noisy pull
toward a piecewise-log-linear anchor path through documented regime points:

    2021-08-01  ₦410   pre-unification official regime (series start)
    2022-12-31  ₦448   managed crawl
    2023-05-31  ₦465   pre-devaluation plateau
    2023-06-30  ₦750   June-2023 unification devaluation step
    2023-12-31  ₦900   post-step drift
    2024-03-01  ₦1,600 early-2024 spike
    2024-08-01  ₦1,450 partial retrace
    2025-06-30  ₦1,530 renewed crawl
    2026-08-01  ₦1,550 terminal regime (spec target ≈ ₦1,550 by 2026-08)

Parallel (street) rate = official × (1 + spread); spread mean-reverts around
0.08 pre-unification / 0.15 transition / 0.18 post-2024, clamped [0.02, 0.35].
Fully deterministic (fixed seed) so re-runs reproduce identical rows.

Gate (spec §Agent B): ≥365 contiguous daily points — enforced inside
``build_fx_series`` (raises, i.e. fails loud, on any gap).

Writes BOTH:
  1. ``cac.fx_series`` via the contract-B loader (delete + upsert,
     seed_run_log row, report event), and
  2. ``$SEED_MANIFEST_DIR/fx_series.json`` (default
     ``/var/tmp/seed_manifests/fx_series.json``) — the Iceberg-side manifest
     Agent C's ``seed_gold_load.py`` merges into
     ``cac_gold.usd_shadow_prices``. Row fields are EXACTLY
     ``{day, official_ngn, parallel_ngn}`` (COORDINATION contract with
     Agent C; the parallel rate is the USD shadow price).

``--scale`` is accepted for contract-B CLI parity and recorded in the
manifest, but the FX series is always written in full — scaling a date axis
would break the contiguity gate. ``--dry-run`` prints counts/gate result,
no writes. Fail loud (non-zero exit) on any exception.
"""

from __future__ import annotations

import json
import math
import os
import sys
from datetime import date, timedelta
from pathlib import Path
from typing import Mapping, Sequence

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _lib  # noqa: E402  (contract A lib, owned by Agent A)
import seed_agents  # noqa: E402

TABLE = "cac.fx_series"
COLUMNS = ["id", "series_date", "usd_ngn_official", "usd_ngn_parallel", "source"]
SOURCE_TAG = "synthetic-walk"  # matches DDL default; anchored walk, not crawled
START = date(2021, 8, 1)
END = date(2026, 7, 31)  # inclusive; 1,826 days
MIN_POINTS_GATE = 365
DEFAULT_MANIFEST_DIR = "/var/tmp/seed_manifests"
MANIFEST_NAME = "fx_series.json"

# (date, official NGN/USD) regime anchors — see module docstring.
ANCHORS: tuple[tuple[date, float], ...] = (
    (date(2021, 8, 1), 410.0),
    (date(2022, 12, 31), 448.0),
    (date(2023, 5, 31), 465.0),
    (date(2023, 6, 30), 750.0),
    (date(2023, 12, 31), 900.0),
    (date(2024, 3, 1), 1600.0),
    (date(2024, 8, 1), 1450.0),
    (date(2025, 6, 30), 1530.0),
    (date(2026, 8, 1), 1550.0),
)
OU_THETA = 0.08   # daily mean-reversion toward the anchor path
OU_SIGMA = 0.006  # daily log-noise
SPREAD_SIGMA = 0.01
SPREAD_MIN, SPREAD_MAX = 0.02, 0.35


def manifest_path() -> Path:
    """$SEED_MANIFEST_DIR/fx_series.json (default /var/tmp/seed_manifests)."""
    return Path(os.environ.get("SEED_MANIFEST_DIR", DEFAULT_MANIFEST_DIR)) / MANIFEST_NAME


def _spread_mean(d: date) -> float:
    if d < date(2023, 6, 14):
        return 0.08
    if d < date(2024, 3, 1):
        return 0.15
    return 0.18


def _anchor_log(d: date) -> float:
    """Log of the piecewise-log-linear anchor path at date ``d``."""
    if d <= ANCHORS[0][0]:
        return math.log(ANCHORS[0][1])
    for (d0, v0), (d1, v1) in zip(ANCHORS, ANCHORS[1:]):
        if d0 <= d <= d1:
            span = (d1 - d0).days
            frac = (d - d0).days / span if span else 1.0
            return math.log(v0) + frac * (math.log(v1) - math.log(v0))
    return math.log(ANCHORS[-1][1])


def build_fx_series(start: date = START, end: date = END) -> list[dict[str, object]]:
    """Pure builder: deterministic daily official/parallel rows.

    Enforces the spec gate: ≥365 contiguous daily points, strictly +1 day,
    parallel >= official on every row.
    """
    days = (end - start).days + 1
    rng = seed_agents.seeded_rng("fx-series", start.isoformat(), end.isoformat())
    noise = 0.0
    spread = _spread_mean(start)
    rows: list[dict[str, object]] = []
    for k in range(days):
        d = start + timedelta(days=k)
        noise = (1 - OU_THETA) * noise + OU_SIGMA * rng.gauss(0.0, 1.0)
        official = math.exp(_anchor_log(d) + noise)
        spread += 0.10 * (_spread_mean(d) - spread) + SPREAD_SIGMA * rng.gauss(0.0, 1.0)
        spread = min(SPREAD_MAX, max(SPREAD_MIN, spread))
        rows.append(
            {
                "id": _lib.deterministic_id(f"fx:{d.isoformat()}"),
                "series_date": d,
                "usd_ngn_official": round(official, 4),
                "usd_ngn_parallel": round(official * (1 + spread), 4),
                "source": SOURCE_TAG,
            }
        )
    # Gate: ≥365 contiguous daily points (fail loud).
    if len(rows) < MIN_POINTS_GATE:
        raise RuntimeError(f"fx contiguity gate: {len(rows)} points < {MIN_POINTS_GATE}")
    for prev, cur in zip(rows, rows[1:]):
        if (cur["series_date"] - prev["series_date"]).days != 1:
            raise RuntimeError(
                f"fx contiguity gate: gap between {prev['series_date']} and {cur['series_date']}"
            )
    return rows


def build_fx_manifest(rows: Sequence[Mapping[str, object]], scale: float) -> dict[str, object]:
    """Iceberg-side manifest for Agent C's gold load (COORDINATION contract).

    Row fields are EXACTLY {day, official_ngn, parallel_ngn}; the parallel
    rate is the USD shadow price for cac_gold.usd_shadow_prices.
    """
    return {
        "manifest_version": 1,
        "kind": "fx_series",
        "generated_by": "scripts/seeds/seed_fx.py",
        "seed_scale": scale,
        "source": SOURCE_TAG,
        "postgres_table": TABLE,
        "gold_target": "cac_gold.usd_shadow_prices",
        "anchors": [[d.isoformat(), v] for d, v in ANCHORS],
        "rowcount": len(rows),
        "rows": [
            {
                "day": r["series_date"].isoformat(),
                "official_ngn": r["usd_ngn_official"],
                "parallel_ngn": r["usd_ngn_parallel"],
            }
            for r in rows
        ],
    }


def write_fx_manifest(manifest: Mapping[str, object], path: Path | None = None) -> Path:
    path = path or manifest_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    return path


def main(argv: list[str] | None = None) -> int:
    args = _lib.seed_argparser("Seed cac.fx_series (USD/NGN daily, anchored random walk) + gold manifest").parse_args(argv)
    scale = _lib.apply_scale_arg(args.scale)
    try:
        rows = build_fx_series()  # contiguity gate enforced inside
    except Exception as exc:  # noqa: BLE001 - fail loud
        print(f"[seed_fx] FAILED: {exc}", file=sys.stderr)
        return 1
    manifest = build_fx_manifest(rows, scale)
    first, last = rows[0], rows[-1]
    print(
        f"[seed_fx] rows={len(rows)} {first['series_date']}..{last['series_date']} "
        f"(official ₦{first['usd_ngn_official']} -> ₦{last['usd_ngn_official']}; gate ≥{MIN_POINTS_GATE} OK); "
        f"manifest -> {manifest_path()}"
    )
    if args.dry_run:
        print("[seed_fx] dry-run: no DB/file writes (scale recorded only; series always full)")
        return 0
    try:
        conn = _lib.get_conn()
        _lib.delete_by_ids(conn, TABLE, [str(r["id"]) for r in rows])
        _lib.upsert_rows(conn, TABLE, COLUMNS, rows)
        _lib.log_seed_run(TABLE, len(rows), conn)
        _lib.commit(conn)
        write_fx_manifest(manifest)
    except Exception as exc:  # noqa: BLE001 - fail loud, non-zero exit
        print(f"[seed_fx] FAILED: {exc}", file=sys.stderr)
        return 1
    _lib.emit_seed_report(TABLE, len(rows), _lib.runner_id(), _lib.git_sha())
    print(f"[seed_fx] seeded {len(rows)} fx points into {TABLE}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
