#!/usr/bin/env python3
"""seed_channel_costs.py — 32 channels x 24 months of unit-cost rows (768).

SUBSTITUTION (documented per spec): no real cost crawling is possible offline,
so each channel's monthly unit cost is a DETERMINISTIC pseudo-random walk in
NGN anchored on the channel's playbook base cost (seed_channels.py). The RNG
is seeded from sha256(channel_code), so the full 768-row series is byte-stable
across runs/machines. Walk: month-over-month multiplier ~ U(0.97, 1.06)
(mild inflation regime), start ~ base * U(0.95, 1.05).

Months: 24 consecutive months 2024-09 .. 2026-08 (fixed anchor so the series
aligns with Agent B's FX walk regime and stays deterministic).

Also writes a JSON manifest for Agent C's cac_gold.channel_unit_costs load:
$SEED_MANIFEST_DIR/channel_unit_costs.json (default /var/tmp/seed_manifests),
non-dry-run only. Manifest row fields (consumer contract): month,
channel_code, channel_name, channel_class, unit_cost_ngn, currency.

--dry-run prints counts, no DB, no manifest. Non-zero exit on failure.
"""

from __future__ import annotations

import hashlib
import json
import random
import sys
from datetime import date
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _lib  # noqa: E402
import seed_channels  # noqa: E402

TABLE = "cac.channel_unit_costs"
COLUMNS = ["id", "channel_id", "month", "unit_cost_ngn"]
MONTHS = 24
START = date(2024, 9, 1)  # fixed anchor, documented above
EXPECTED_ROWS = seed_channels.EXPECTED_CHANNELS * MONTHS  # 768


def month_at(i: int) -> date:
    """START + i months (first of month)."""
    y = START.year + (START.month - 1 + i) // 12
    m = (START.month - 1 + i) % 12 + 1
    return date(y, m, 1)


def walk_costs(channel_code: str, base_cost_ngn: float) -> list[float]:
    """Deterministic pseudo-random walk of MONTHS unit costs for one channel."""
    seed = int.from_bytes(hashlib.sha256(channel_code.encode("utf-8")).digest()[:8], "big")
    rng = random.Random(seed)
    cost = base_cost_ngn * rng.uniform(0.95, 1.05)
    out = []
    for _ in range(MONTHS):
        cost *= rng.uniform(0.97, 1.06)
        out.append(round(cost, 2))
    return out


def build_rows(scale: float = 1.0) -> list[dict[str, object]]:
    """Pure + deterministic. scale ignored: fixed 32 x 24 reference grid."""
    rows: list[dict[str, object]] = []
    for ch in seed_channels.build_rows():
        code = str(ch["channel_code"])
        for i, cost in enumerate(walk_costs(code, float(ch["base_cost_ngn"]))):
            month = month_at(i)
            rows.append(
                {
                    "id": _lib.deterministic_id(f"channel_cost:{code}:{month:%Y-%m}"),
                    "channel_id": ch["id"],
                    "month": month.isoformat(),
                    "unit_cost_ngn": cost,
                }
            )
    return rows


MANIFEST_FILENAME = "channel_unit_costs.json"
DEFAULT_MANIFEST_DIR = "/var/tmp/seed_manifests"


def manifest_rows(rows: list[dict[str, object]]) -> list[dict[str, object]]:
    """Project DB rows into the Agent C consumer contract:
    {month, channel_code, channel_name, channel_class, unit_cost_ngn, currency}."""
    channels = {str(ch["id"]): ch for ch in seed_channels.build_rows()}
    return [
        {
            "month": r["month"],
            "channel_code": channels[str(r["channel_id"])]["channel_code"],
            "channel_name": channels[str(r["channel_id"])]["name"],
            "channel_class": channels[str(r["channel_id"])]["channel_class"],
            "unit_cost_ngn": r["unit_cost_ngn"],
            "currency": "NGN",
        }
        for r in rows
    ]


def write_manifest(rows: list[dict[str, object]]) -> Path:
    """JSON manifest for Agent C's seed_gold_load (cac_gold.channel_unit_costs).

    Path: $SEED_MANIFEST_DIR/channel_unit_costs.json (default
    /var/tmp/seed_manifests/channel_unit_costs.json).
    """
    out_dir = Path(__import__("os").environ.get("SEED_MANIFEST_DIR", DEFAULT_MANIFEST_DIR))
    out_dir.mkdir(parents=True, exist_ok=True)
    path = out_dir / MANIFEST_FILENAME
    payload = {
        "table": TABLE,
        "rowcount": len(rows),
        "generator": "seed_channel_costs.py (deterministic walk, not crawled)",
        "rows": manifest_rows(rows),
    }
    path.write_text(json.dumps(payload, indent=2), encoding="utf-8")
    return path


def main(argv: list[str] | None = None) -> int:
    args = _lib.seed_argparser("Seed cac.channel_unit_costs (32 channels x 24 months)").parse_args(argv)
    scale = _lib.apply_scale_arg(args.scale)
    rows = build_rows(scale)
    print(f"[seed_channel_costs] rows={len(rows)} (expected {EXPECTED_ROWS})")
    if args.dry_run:
        print("[seed_channel_costs] dry-run: no DB writes")
        return 0
    try:
        conn = _lib.get_conn()
        _lib.delete_by_ids(conn, TABLE, [str(r["id"]) for r in rows])
        _lib.upsert_rows(conn, TABLE, COLUMNS, rows)
        _lib.log_seed_run(TABLE, len(rows), conn)
        _lib.commit(conn)
        manifest = write_manifest(rows)
    except Exception as exc:  # noqa: BLE001 - fail loud, non-zero exit
        print(f"[seed_channel_costs] FAILED: {exc}", file=sys.stderr)
        return 1
    _lib.emit_seed_report(TABLE, len(rows), _lib.runner_id(), _lib.git_sha())
    print(f"[seed_channel_costs] seeded {len(rows)} rows into {TABLE}; manifest {manifest}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
