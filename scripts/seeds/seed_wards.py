#!/usr/bin/env python3
"""seed_wards.py — seed cac.wards with EXACTLY 8,812 rows (SPEC-W17 Agent A).

SUBSTITUTION (spec §8.2 row 2): real per-ward lists are unavailable offline,
so ward names are STABLE synthetics of the form "Ward NN — <LGA>", distributed
per LGA and summing to exactly 8,812 at scale 1.0 (the canonical national ward
total). Distribution: base = 8,812 // 774 = 11 wards per LGA, remainder 298,
so the first 298 LGAs (CSV order) carry 12. At scale < 1 the same
largest-remainder rule is applied to scaled(8812) (with target < 774 the first
`target` LGAs get 1 ward each); ward ids remain deterministic per natural key
so reseeding at a fixed scale is idempotent.

--dry-run prints counts, no DB. Non-zero exit on any failure (contract B).
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _lib  # noqa: E402
import seed_lgas  # noqa: E402

TABLE = "cac.wards"
COLUMNS = ["id", "lga_id", "ward_name"]
TOTAL_WARDS = 8812


def wards_per_lga(n_lgas: int, target: int) -> list[int]:
    """Largest-remainder distribution of `target` wards over `n_lgas` LGAs.

    Deterministic: the first `target % n_lgas` LGAs get one extra ward. When
    target < n_lgas (very small scales), the first `target` LGAs get 1 each.
    """
    if target >= n_lgas:
        base, rem = divmod(target, n_lgas)
        return [base + (1 if i < rem else 0) for i in range(n_lgas)]
    return [1 if i < target else 0 for i in range(n_lgas)]


def build_rows(scale: float = 1.0) -> list[dict[str, str]]:
    """Pure + deterministic: same inputs (csv, salt, scale) → same ids."""
    lgas = seed_lgas.build_rows()
    target = int(TOTAL_WARDS * scale)
    counts = wards_per_lga(len(lgas), target)
    rows: list[dict[str, str]] = []
    for lga, n in zip(lgas, counts):
        for nn in range(1, n + 1):
            key = f"ward:{lga['state']}:{lga['lga_name']}:{nn:02d}"
            rows.append(
                {
                    "id": _lib.deterministic_id(key),
                    "lga_id": lga["id"],
                    "ward_name": f"Ward {nn:02d} — {lga['lga_name']}",
                }
            )
    return rows


def main(argv: list[str] | None = None) -> int:
    args = _lib.seed_argparser("Seed cac.wards (8,812 synthetic wards)").parse_args(argv)
    scale = _lib.apply_scale_arg(args.scale)
    rows = build_rows(scale)
    print(f"[seed_wards] rows={len(rows)} (target {int(TOTAL_WARDS * scale)} at scale {scale})")
    if args.dry_run:
        print("[seed_wards] dry-run: no DB writes")
        return 0
    try:
        conn = _lib.get_conn()
        _lib.delete_by_ids(conn, TABLE, [r["id"] for r in rows])
        _lib.upsert_rows(conn, TABLE, COLUMNS, rows)
        _lib.log_seed_run(TABLE, len(rows), conn)
        _lib.commit(conn)
    except Exception as exc:  # noqa: BLE001 - fail loud, non-zero exit
        print(f"[seed_wards] FAILED: {exc}", file=sys.stderr)
        return 1
    _lib.emit_seed_report(TABLE, len(rows), _lib.runner_id(), _lib.git_sha())
    print(f"[seed_wards] seeded {len(rows)} rows into {TABLE}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
