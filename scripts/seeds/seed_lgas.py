#!/usr/bin/env python3
"""seed_lgas.py — seed cac.lgas with all 774 Nigerian LGAs (SPEC-W17 Agent A).

Source data: scripts/seeds/data/nigeria_lgas.csv (well-known public data,
written out in full: lga_name,state,zone). Reference cardinality is FIXED at
774 — --scale is accepted per contract B but is a no-op here (LGAs are
reference geography, not synthetic volume). Runs in <60s (single pass,
batched executemany-style upsert through _lib).

Loader idiom (contract B): deterministic id -> DELETE WHERE id IN (...) ->
INSERT ... ON CONFLICT (id) DO UPDATE -> emit report -> seed_run_log row.
--dry-run prints counts with no DB connection and no writes.
"""

from __future__ import annotations

import csv
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _lib  # noqa: E402

TABLE = "cac.lgas"
COLUMNS = ["id", "lga_name", "state", "zone"]
DATA_CSV = Path(__file__).resolve().parent / "data" / "nigeria_lgas.csv"
EXPECTED_ROWS = 774


def load_csv(path: Path = DATA_CSV) -> list[dict[str, str]]:
    with open(path, newline="", encoding="utf-8") as fh:
        return list(csv.DictReader(fh))


def build_rows(scale: float = 1.0) -> list[dict[str, str]]:
    """Pure + deterministic. scale ignored: reference cardinality is fixed."""
    rows = []
    for rec in load_csv():
        lga, state, zone = rec["lga_name"].strip(), rec["state"].strip(), rec["zone"].strip()
        rows.append(
            {
                "id": _lib.deterministic_id(f"lga:{state}:{lga}"),
                "lga_name": lga,
                "state": state,
                "zone": zone,
            }
        )
    return rows


def main(argv: list[str] | None = None) -> int:
    args = _lib.seed_argparser("Seed cac.lgas (774 Nigerian LGAs)").parse_args(argv)
    scale = _lib.apply_scale_arg(args.scale)
    rows = build_rows(scale)
    print(f"[seed_lgas] rows={len(rows)} (expected {EXPECTED_ROWS})")
    if args.dry_run:
        print("[seed_lgas] dry-run: no DB writes")
        return 0
    try:
        conn = _lib.get_conn()
        _lib.delete_by_ids(conn, TABLE, [r["id"] for r in rows])
        _lib.upsert_rows(conn, TABLE, COLUMNS, rows)
        _lib.log_seed_run(TABLE, len(rows), conn)
        _lib.commit(conn)
    except Exception as exc:  # noqa: BLE001 - fail loud, non-zero exit
        print(f"[seed_lgas] FAILED: {exc}", file=sys.stderr)
        return 1
    _lib.emit_seed_report(TABLE, len(rows), _lib.runner_id(), _lib.git_sha())
    print(f"[seed_lgas] seeded {len(rows)} rows into {TABLE}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
