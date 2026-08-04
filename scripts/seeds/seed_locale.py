#!/usr/bin/env python3
"""seed_locale.py — locale coverage validation report (SPEC-W17 Agent A).

Langflow/Wagtail are substituted per spec adaptation #4: locale seeding is a
VALIDATION REPORT over the existing pack i18n blocks (industries/*.yaml), not
a CMS. Scans every pack, extracts the top-level `i18n:` block with a stdlib
line-based parser (no PyYAML dependency), and prints a console table of locale
coverage (en/pcm/ha/yo/ig) per pack. The report row (table 'locale_coverage',
rowcount = packs scanned) is upserted into cac.seed_run_log on non-dry-runs.

--dry-run prints the table only, no DB. Non-zero exit on failure (contract B).
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import _lib  # noqa: E402

TABLE = "locale_coverage"  # report-only row in cac.seed_run_log
REPORT_TABLE = "cac.seed_run_log"
LOCALES = ["en", "pcm", "ha", "yo", "ig"]
PACKS_DIR = Path(__file__).resolve().parents[2] / "industries"


def parse_i18n_locales(path: Path) -> set[str]:
    """Stdlib line-based scan of a pack YAML for its top-level i18n block.

    Finds the `i18n:` top-level key, then collects its 2-space-indented
    locale keys (e.g. `  pcm:`) until the next top-level key. Sufficient for
    the pack files' regular structure and avoids a PyYAML dependency.
    """
    locales: set[str] = set()
    in_block = False
    for line in path.read_text(encoding="utf-8").splitlines():
        if not in_block:
            if line.rstrip() == "i18n:":
                in_block = True
            continue
        if line and not line[0].isspace():  # next top-level key ends the block
            break
        stripped = line.strip()
        if line.startswith("  ") and not line.startswith("   ") and stripped.endswith(":"):
            locales.add(stripped[:-1].strip())
    return locales


def scan_packs(packs_dir: Path = PACKS_DIR) -> list[tuple[str, set[str]]]:
    """[(pack_id, {locales})] sorted by pack id. Pure + deterministic."""
    out = []
    for path in sorted(packs_dir.glob("*.yaml")):
        out.append((path.stem, parse_i18n_locales(path)))
    return out


def render_table(packs: list[tuple[str, set[str]]]) -> str:
    header = f"{'pack':<28}" + "".join(f"{lc:>6}" for lc in LOCALES)
    lines = [header, "-" * len(header)]
    for pack_id, locales in packs:
        row = f"{pack_id:<28}" + "".join(
            f"{'x' if lc in locales else '-':>6}" for lc in LOCALES
        )
        lines.append(row)
    covered = {lc: sum(1 for _, s in packs if lc in s) for lc in LOCALES}
    lines.append("-" * len(header))
    lines.append(f"{'packs with locale':<28}" + "".join(f"{covered[lc]:>6}" for lc in LOCALES))
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    args = _lib.seed_argparser("Locale coverage report over industries/*.yaml i18n blocks").parse_args(argv)
    _lib.apply_scale_arg(args.scale)  # accepted per contract B; report is unscaled
    packs = scan_packs()
    print(render_table(packs))
    print(f"[seed_locale] packs={len(packs)}")
    if args.dry_run:
        print("[seed_locale] dry-run: no DB writes")
        return 0
    try:
        conn = _lib.get_conn()
        _lib.log_seed_run(TABLE, len(packs), conn)
        _lib.commit(conn)
    except Exception as exc:  # noqa: BLE001 - fail loud, non-zero exit
        print(f"[seed_locale] FAILED: {exc}", file=sys.stderr)
        return 1
    _lib.emit_seed_report(TABLE, len(packs), _lib.runner_id(), _lib.git_sha())
    print(f"[seed_locale] recorded coverage report in {REPORT_TABLE}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
