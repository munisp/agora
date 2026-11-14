#!/usr/bin/env python3
"""Offline GeoNames NG locality converter (SPEC-W68 WS2 / G-GEO1-5).

Converts an operator-downloaded GeoNames Nigeria dump into the existing
gazetteer import CSV shape (``kind,code,name,parent_code,lat,lng,source`` —
``services/geo-service/src/gazetteer.rs`` ``parse_csv_records``) so the
locality tier can be imported through the AV-scanned
``POST /v1/geo/gazetteer/import`` endpoint. The converter is **offline by
design**: it reads only local files passed on the command line and never
talks to the network or to geo-service.

Inputs (all local paths, NOT committed to the repo):

- ``NG.zip`` (or an extracted ``NG.txt``) from
  https://download.geonames.org/export/dump/NG.zip
- ``admin1CodesASCII.txt`` and ``admin2Codes.txt`` (same dump directory) —
  GeoNames admin codes are numeric-ish keys (``NG.<a1>``, ``NG.<a1>.<a2>``);
  these files map them to names.
- ``scripts/seeds/data/geonames_ng_admin_overrides.csv`` (committed,
  hand-maintained): ``level,geonames_key,geo_places_code`` — consulted
  FIRST at both admin levels; ``geonames_key`` may be the GeoNames key
  (``NG.11`` / ``NG.11.01``) or the (ascii)name as it appears in the
  admin-codes files.

Codes mirror the pinned slug rule in ``gazetteer.rs::slug`` EXACTLY
(uppercase ASCII, space and ``/`` become ``-``, all other non-alphanumeric
ASCII stripped, non-ASCII stripped) and are derived from GeoNames
``asciiname``; the display ``name`` keeps the UTF-8 original. Locality codes
are ``<seeded LGA code>-<slug(asciiname)>`` with a ``-<geonameid>`` suffix
on ALL rows of a collision group (distinct geonameids, same code).

Attribution: GeoNames data is licensed under Creative Commons Attribution
4.0 (CC-BY 4.0, https://creativecommons.org/licenses/by/4.0/). Every output
row carries ``source = "geonames NG dump <YYYY-MM-DD> (CC-BY 4.0)
geonameid=<id>"``; keep the notice in docs/geospatial-terrio.md §7.

Exit codes: 0 = conversion written with 37/37 admin1 resolved (unresolved
admin2 rows are SKIPPED and reported); 1 = usage/IO error; 2 = completeness
gate failed (fewer than 37/37 admin1 resolved) — no output is written.
"""

from __future__ import annotations

import argparse
import csv
import io
import sys
import zipfile
from pathlib import Path

EXPECTED_STATE_COUNT = 37
COUNTRY = "NG"
CSV_HEADER = ["kind", "code", "name", "parent_code", "lat", "lng", "source"]
SOURCE_TEMPLATE = "geonames NG dump {date} (CC-BY 4.0) geonameid={gid}"

REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_GAZETTEER = (
    REPO_ROOT / "services" / "geo-service" / "seeds" / "nigeria_gazetteer.csv"
)
DEFAULT_OVERRIDES = (
    REPO_ROOT / "scripts" / "seeds" / "data" / "geonames_ng_admin_overrides.csv"
)
DEFAULT_OUT = REPO_ROOT / "scripts" / "seeds" / "out" / "nigeria_localities.csv"

# GeoNames main dump column indices (tab-separated, 19 columns).
COL_GEONAMEID = 0
COL_NAME = 1
COL_ASCIINAME = 2
COL_LAT = 4
COL_LNG = 5
COL_FEATURE_CLASS = 6
COL_ADMIN1 = 10
COL_ADMIN2 = 11
COL_MODDATE = 18


def slug(name: str) -> str:
    """Mirror of ``gazetteer.rs::slug`` (PINNED, SPEC-W63 §1).

    trim; ' ' and '/' -> '-'; ASCII-only uppercase; keep only ASCII
    alphanumerics and '-'. Rust ``to_ascii_uppercase`` leaves non-ASCII
    bytes unchanged (they are then filtered out), so we uppercase ASCII
    only — ``str.upper()`` would wrongly map e.g. 'ß'.
    """
    out = []
    for ch in name.strip():
        if ch == " " or ch == "/":
            out.append("-")
            continue
        if "a" <= ch <= "z":
            ch = chr(ord(ch) - 32)
        if ch.isascii() and (ch.isalnum() or ch == "-"):
            out.append(ch)
    return "".join(out)


def _open_geonames_text(path: Path) -> io.TextIOBase:
    """Return a text stream for an NG.zip (single .txt member) or NG.txt."""
    if zipfile.is_zipfile(path):
        zf = zipfile.ZipFile(path)
        members = [n for n in zf.namelist() if n.lower().endswith(".txt")]
        if len(members) != 1:
            zf.close()
            raise SystemExit(
                f"{path}: expected exactly one .txt member in the zip, "
                f"found {members or 'none'}"
            )
        # Caller keeps the returned wrapper alive; closing it closes the zip.
        return io.TextIOWrapper(
            zf.open(members[0]), encoding="utf-8", newline=""
        )
    return path.open("r", encoding="utf-8", newline="")


def read_geonames_rows(path: Path):
    """Yield parsed P-class rows + track the dump date (max moddate)."""
    rows = []
    dump_date = ""
    with _open_geonames_text(path) as fh:
        for line in fh:
            line = line.rstrip("\r\n")
            if not line:
                continue
            cols = line.split("\t")
            if len(cols) <= COL_MODDATE:
                continue  # malformed short row — skip, not our data
            moddate = cols[COL_MODDATE].strip()
            if moddate > dump_date:
                dump_date = moddate
            if cols[COL_FEATURE_CLASS] != "P":
                continue
            rows.append(
                {
                    "geonameid": cols[COL_GEONAMEID].strip(),
                    "name": cols[COL_NAME].strip(),
                    "asciiname": cols[COL_ASCIINAME].strip(),
                    "lat": cols[COL_LAT].strip(),
                    "lng": cols[COL_LNG].strip(),
                    "admin1": cols[COL_ADMIN1].strip(),
                    "admin2": cols[COL_ADMIN2].strip(),
                }
            )
    return rows, (dump_date or "unknown-date")


def load_admin_names(path: Path, prefix: str) -> dict:
    """Load admin1CodesASCII.txt / admin2Codes.txt.

    Returns {key: (name, asciiname)} for keys starting with ``NG.``.
    Both files are tab-separated: key, name, asciiname, geonameid.
    """
    out = {}
    if not path.exists():
        return out
    with path.open("r", encoding="utf-8", newline="") as fh:
        for line in fh:
            line = line.rstrip("\r\n")
            if not line:
                continue
            cols = line.split("\t")
            if len(cols) < 3:
                continue
            key = cols[0].strip()
            if key.startswith(prefix + "."):
                out[key] = (cols[1].strip(), cols[2].strip())
    return out


def load_overrides(path: Path) -> dict:
    """Load the committed override CSV: {(level, geonames_key): code}.

    Blank lines and ``#`` comment lines are skipped. Codes are normalized
    (trim + ASCII-uppercase) like ``normalize_code`` in gazetteer.rs.
    """
    out = {}
    if not path.exists():
        return out
    with path.open("r", encoding="utf-8", newline="") as fh:
        for row in csv.reader(fh):
            if not row or not any(cell.strip() for cell in row):
                continue
            if row[0].lstrip().startswith("#"):
                continue
            if row[0].strip().lower() == "level":  # header row
                continue
            if len(row) < 3:
                raise SystemExit(f"{path}: malformed override row {row!r}")
            level, key, code = row[0].strip(), row[1].strip(), row[2].strip()
            if level not in ("admin1", "admin2"):
                raise SystemExit(f"{path}: unknown override level {level!r}")
            out[(level, key)] = code.upper()
    return out


def load_seeded_codes(gazetteer: Path):
    """Load seeded state/LGA codes from nigeria_gazetteer.csv.

    Returns (state_by_slug, lga_by_state):
      state_by_slug: {slug(state name): state code}
      lga_by_state:  {state code: {slug(lga name): lga code}}
    """
    state_by_slug = {}
    lga_by_state = {}
    with gazetteer.open("r", encoding="utf-8", newline="") as fh:
        for row in csv.DictReader(fh):
            kind = (row.get("kind") or "").strip()
            code = (row.get("code") or "").strip().upper()
            name = (row.get("name") or "").strip()
            parent = (row.get("parent_code") or "").strip().upper()
            if kind == "state":
                state_by_slug[slug(name)] = code
            elif kind == "lga":
                lga_by_state.setdefault(parent, {})[slug(name)] = code
    return state_by_slug, lga_by_state


def resolve_admin_key(level, key_code, names, overrides, by_slug):
    """Override first (by GeoNames key, then name, then asciiname), then
    the default slug(asciiname) join. Returns resolved code or None."""
    name, asciiname = names if names else ("", "")
    for cand in (key_code, name, asciiname):
        if cand and (level, cand) in overrides:
            return overrides[(level, cand)]
    if asciiname:
        return by_slug.get(slug(asciiname))
    return None


def convert(
    ng_path: Path,
    admin1_path: Path,
    admin2_path: Path,
    overrides_path: Path,
    gazetteer_path: Path,
    dump_date_override: str = "",
):
    """Run the conversion. Returns (rows, report_lines, ok)."""
    overrides = load_overrides(overrides_path)
    state_by_slug, lga_by_state = load_seeded_codes(gazetteer_path)
    admin1_names = load_admin_names(admin1_path, COUNTRY)
    admin2_names = load_admin_names(admin2_path, COUNTRY)
    p_rows, dump_date = read_geonames_rows(ng_path)
    if dump_date_override:
        dump_date = dump_date_override

    # --- admin1 resolution -------------------------------------------------
    admin1_keys = set(admin1_names)
    admin1_seen_in_dump = {}
    for r in p_rows:
        key = f"{COUNTRY}.{r['admin1']}" if r["admin1"] else f"{COUNTRY}."
        admin1_seen_in_dump[key] = admin1_seen_in_dump.get(key, 0) + 1
        admin1_keys.add(key)

    admin1_resolved = {}  # key -> state code
    unresolved_admin1 = {}  # key -> dump row count
    for key in sorted(admin1_keys):
        code = resolve_admin_key(
            "admin1", key, admin1_names.get(key), overrides, state_by_slug
        )
        if code:
            admin1_resolved[key] = code
        else:
            unresolved_admin1[key] = admin1_seen_in_dump.get(key, 0)
    covered_states = set(admin1_resolved.values())
    states_covered = len(
        covered_states & set(state_by_slug.values())
    )

    # --- admin2 resolution + row build -------------------------------------
    unresolved_admin2 = {}  # (admin2 key, name) -> skipped row count
    skipped = 0
    built = []  # (code, geonameid, csv row dict)
    for r in p_rows:
        a1_key = f"{COUNTRY}.{r['admin1']}" if r["admin1"] else f"{COUNTRY}."
        state_code = admin1_resolved.get(a1_key)
        if not state_code:
            skipped += 1
            continue
        a2_key = f"{a1_key}.{r['admin2']}" if r["admin2"] else f"{a1_key}."
        lga_code = resolve_admin_key(
            "admin2",
            a2_key,
            admin2_names.get(a2_key),
            overrides,
            lga_by_state.get(state_code, {}),
        )
        if not lga_code:
            # SKIPPED — never state-parented (locality -> LGA only).
            skipped += 1
            name = (admin2_names.get(a2_key) or ("", ""))[1]
            ukey = (a2_key, name or "?")
            unresolved_admin2[ukey] = unresolved_admin2.get(ukey, 0) + 1
            continue
        code = f"{lga_code}-{slug(r['asciiname'])}"
        built.append((code, r["geonameid"], r, lga_code))

    # --- collision rule: suffix ALL rows of a colliding code ---------------
    by_code = {}
    for code, gid, r, lga_code in built:
        by_code.setdefault(code, []).append(gid)
    collision_groups = 0
    suffixed_rows = 0
    final = []
    for code, gid, r, lga_code in built:
        gids = by_code[code]
        if len(set(gids)) > 1:
            code = f"{code}-{gid}"
            suffixed_rows += 1
        final.append(
            {
                "kind": "locality",
                "code": code,
                "name": r["name"],
                "parent_code": lga_code,
                "lat": r["lat"],
                "lng": r["lng"],
                "source": SOURCE_TEMPLATE.format(date=dump_date, gid=gid),
            }
        )
    collision_groups = sum(
        1 for gids in by_code.values() if len(set(gids)) > 1
    )

    final.sort(key=lambda row: row["code"])

    # --- completeness report ------------------------------------------------
    report = []
    report.append("GeoNames NG locality conversion — completeness report")
    report.append(f"input P-class rows: {len(p_rows)}")
    report.append(
        f"admin1 resolved {states_covered}/{EXPECTED_STATE_COUNT}"
    )
    if unresolved_admin1:
        report.append("UNRESOLVED admin1 keys:")
        for key in sorted(unresolved_admin1):
            name = (admin1_names.get(key) or ("?", ""))[0]
            report.append(
                f"  {key} ({name}): {unresolved_admin1[key]} rows"
            )
    if unresolved_admin2:
        report.append("unresolved admin2 keys (rows SKIPPED):")
        for (key, name) in sorted(unresolved_admin2):
            report.append(
                f"  {key} ({name}): {unresolved_admin2[(key, name)]} rows"
            )
    else:
        report.append("unresolved admin2 keys: none")
    report.append(
        f"collision groups: {collision_groups} "
        f"(rows suffixed with -<geonameid>: {suffixed_rows})"
    )
    report.append(f"rows skipped: {skipped}")
    report.append(f"final locality rows: {len(final)}")

    ok = states_covered == EXPECTED_STATE_COUNT
    if not ok:
        missing = sorted(set(state_by_slug.values()) - covered_states)
        report.append("uncovered seeded states: " + ", ".join(missing))
        report.append(
            f"HARD FAIL: admin1 resolution is {states_covered}/"
            f"{EXPECTED_STATE_COUNT} — 37/37 required; output NOT written."
        )
    return final, report, ok


def write_csv(rows, out_path: Path):
    out_path.parent.mkdir(parents=True, exist_ok=True)
    with out_path.open("w", encoding="utf-8", newline="") as fh:
        writer = csv.DictWriter(
            fh, fieldnames=CSV_HEADER, lineterminator="\n"
        )
        writer.writeheader()
        writer.writerows(rows)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(
        description=__doc__.splitlines()[0],
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    ap.add_argument(
        "--ng",
        default="NG.zip",
        help="path to NG.zip (or extracted NG.txt); falls back to ./NG.txt "
        "when the default zip is absent",
    )
    ap.add_argument("--admin1", default="admin1CodesASCII.txt")
    ap.add_argument("--admin2", default="admin2Codes.txt")
    ap.add_argument("--overrides", default=str(DEFAULT_OVERRIDES))
    ap.add_argument("--gazetteer", default=str(DEFAULT_GAZETTEER),
                    help="seeded gazetteer CSV (source of state/LGA codes)")
    ap.add_argument("--out", default=str(DEFAULT_OUT))
    ap.add_argument(
        "--dump-date",
        default="",
        help="YYYY-MM-DD override for the per-row source string "
        "(default: max modification date seen in the dump)",
    )
    args = ap.parse_args(argv)

    ng_path = Path(args.ng)
    if not ng_path.exists() and args.ng == "NG.zip" and Path("NG.txt").exists():
        ng_path = Path("NG.txt")
    for label, p in (
        ("NG dump", ng_path),
        ("admin1 codes", Path(args.admin1)),
        ("admin2 codes", Path(args.admin2)),
        ("overrides", Path(args.overrides)),
        ("seeded gazetteer", Path(args.gazetteer)),
    ):
        if not p.exists():
            print(f"error: {label} file not found: {p}", file=sys.stderr)
            return 1

    rows, report, ok = convert(
        ng_path,
        Path(args.admin1),
        Path(args.admin2),
        Path(args.overrides),
        Path(args.gazetteer),
        dump_date_override=args.dump_date,
    )
    for line in report:
        print(line)
    if not ok:
        return 2
    write_csv(rows, Path(args.out))
    print(f"wrote {len(rows)} rows -> {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
