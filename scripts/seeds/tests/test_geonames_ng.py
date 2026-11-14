"""Tests for scripts/seeds/geonames_ng_localities.py (SPEC-W68 WS2, G-GEO1-5).

Fixture-driven: tiny synthetic NG.txt / admin1CodesASCII.txt /
admin2Codes.txt / override inputs built in tmp_path — no real GeoNames
download is needed or committed. The seeded gazetteer CSV used for code
resolution is the REAL services/geo-service/seeds/nigeria_gazetteer.csv
(37 states + 774 LGAs) so the slug-join is tested against shipped data.

Self-contained: inserts scripts/seeds on sys.path itself (no conftest).
End-to-end tests invoke the converter via subprocess (python3) so exit
codes and the stdout completeness report are exercised exactly as an
operator would see them.
"""

from __future__ import annotations

import ast
import csv
import subprocess
import sys
import zipfile
from pathlib import Path

import pytest

SEEDS_DIR = Path(__file__).resolve().parents[1]
REPO_ROOT = Path(__file__).resolve().parents[3]
if str(SEEDS_DIR) not in sys.path:
    sys.path.insert(0, str(SEEDS_DIR))

import geonames_ng_localities as conv  # noqa: E402

SCRIPT = SEEDS_DIR / "geonames_ng_localities.py"
GAZETTEER = (
    REPO_ROOT / "services" / "geo-service" / "seeds" / "nigeria_gazetteer.csv"
)
HEADER = ["kind", "code", "name", "parent_code", "lat", "lng", "source"]

# States whose GeoNames admin1 name differs from the seeded name; these are
# exactly the cases the committed override CSV covers.
ADMIN1_NAME_DRIFT = {
    "NG-FCT": "Federal Capital Territory",
    "NG-NASARAWA": "Nassarawa",
}


def _load_seed():
    states, lgas = [], []
    with GAZETTEER.open(newline="", encoding="utf-8") as fh:
        for row in csv.DictReader(fh):
            if row["kind"] == "state":
                states.append((row["code"], row["name"]))
            elif row["kind"] == "lga":
                lgas.append((row["code"], row["name"], row["parent_code"]))
    assert len(states) == 37
    return states, lgas


STATES, LGAS = _load_seed()


def _gn_row(gid, name, ascii_name, lat, lng, a1, a2,
            fclass="P", fcode="PPL", moddate="2024-05-17"):
    cols = [""] * 19
    cols[0] = str(gid)
    cols[1] = name
    cols[2] = ascii_name
    cols[4] = lat
    cols[5] = lng
    cols[6] = fclass
    cols[7] = fcode
    cols[8] = "NG"
    cols[10] = a1
    cols[11] = a2
    cols[14] = "1000"
    cols[17] = "Africa/Lagos"
    cols[18] = moddate
    return "\t".join(cols)


@pytest.fixture()
def fixture(tmp_path):
    """Synthetic GeoNames inputs covering all 37 seeded states.

    admin1 entries: every seeded state, with the FCT/Nasarawa name drift
    applied (override-CSV cases). admin2 entries: a handful of LGAs plus
    one name-drift LGA (resolved by a fixture override) and one unknown
    LGA (unresolved -> rows skipped).
    """
    a1_code = {}
    admin1_lines = []
    for i, (code, name) in enumerate(STATES, start=1):
        a1_code[code] = f"{i:02d}"
        gn_name = ADMIN1_NAME_DRIFT.get(code, name)
        admin1_lines.append(
            f"NG.{a1_code[code]}\t{gn_name}\t{gn_name}\t{9000 + i}"
        )
    (tmp_path / "admin1CodesASCII.txt").write_text(
        "\n".join(admin1_lines) + "\n", encoding="utf-8"
    )

    fct = a1_code["NG-FCT"]
    nas = a1_code["NG-NASARAWA"]
    lag = a1_code["NG-LAGOS"]
    admin2_lines = [
        f"NG.{fct}.01\tAbaji\tAbaji\t8101",
        f"NG.{fct}.02\tAbuja Municipal\tAbuja Municipal\t8102",
        # name drift vs seeded "Nasarawa Egon": resolved via override below
        f"NG.{nas}.01\tNassarawa Egon\tNassarawa Egon\t8103",
        f"NG.{lag}.01\tIkeja\tIkeja\t8104",
        # unknown to the seed: rows under it must be SKIPPED + reported
        f"NG.{lag}.99\tAtlantis Township\tAtlantis Township\t8199",
    ]
    (tmp_path / "admin2Codes.txt").write_text(
        "\n".join(admin2_lines) + "\n", encoding="utf-8"
    )

    override_lines = [
        "level,geonames_key,geo_places_code",
        "admin1,Federal Capital Territory,NG-FCT",
        "admin1,Nassarawa,NG-NASARAWA",
        "admin2,Nassarawa Egon,NG-NASARAWA-NASARAWA-EGON",
    ]
    (tmp_path / "overrides.csv").write_text(
        "\n".join(override_lines) + "\n", encoding="utf-8"
    )

    ng_rows = [
        # FCT via admin1 override; plain PPL
        _gn_row(1001, "Garki", "Garki", "9.03", "7.49", fct, "02"),
        # display name keeps UTF-8; code derives from asciiname
        _gn_row(1002, "Bùrén", "Buren", "9.10", "7.50", fct, "01"),
        # slug punctuation/space/slash handling in asciiname
        _gn_row(1003, "St. Mary's", "St. Mary's", "9.11", "7.51",
                fct, "01"),
        # LGA resolved only via the admin2 override
        _gn_row(1004, "Karu Road", "Karu Road", "8.99", "7.60",
                nas, "01", fcode="PPLX"),
        # unresolved admin2 -> skipped
        _gn_row(1005, "Lost Village", "Lost Village", "6.50", "3.40",
                lag, "99"),
        # non-P rows are ignored even with valid admin codes
        _gn_row(1006, "Mount Fake", "Mount Fake", "9.20", "7.70",
                fct, "01", fclass="T", fcode="MT"),
        # collision pair: distinct geonameids, same asciiname in one LGA
        _gn_row(1007, "New Site", "New Site", "9.30", "7.80", fct, "01"),
        _gn_row(1008, "New Site", "New Site", "9.31", "7.81", fct, "01"),
    ]
    (tmp_path / "NG.txt").write_text(
        "\n".join(ng_rows) + "\n", encoding="utf-8"
    )

    def run(*extra, ng="NG.txt"):
        return subprocess.run(
            [sys.executable, str(SCRIPT),
             "--ng", str(tmp_path / ng),
             "--admin1", str(tmp_path / "admin1CodesASCII.txt"),
             "--admin2", str(tmp_path / "admin2Codes.txt"),
             "--overrides", str(tmp_path / "overrides.csv"),
             "--gazetteer", str(GAZETTEER),
             "--out", str(tmp_path / "out.csv"),
             *extra],
            capture_output=True, text=True,
        )

    return tmp_path, run


def _read_out(tmp_path):
    with (tmp_path / "out.csv").open(newline="", encoding="utf-8") as fh:
        return list(csv.DictReader(fh))


# --------------------------------------------------------------------------
# slug parity with gazetteer.rs::slug
# --------------------------------------------------------------------------

@pytest.mark.parametrize(
    "raw,expected",
    [
        ("Aba", "ABA"),
        ("Aba North", "ABA-NORTH"),
        ("Aba/Owerri", "ABA-OWERRI"),
        ("St. Mary's", "ST-MARYS"),
        ("Area 2", "AREA-2"),
        ("Nasarawa Egon", "NASARAWA-EGON"),
        ("  padded  ", "PADDED"),
        ("Bùrén", "BRN"),          # non-ASCII stripped, ASCII uppercased
        ("Ìjẹshà", "JSH"),          # non-ASCII stripped
        ("ßabc", "ABC"),            # NOT 'SSABC': ascii-only uppercase
        ("a-b_c", "A-BC"),          # hyphen kept, underscore stripped
        ("100", "100"),             # digits kept
        ("!!!", ""),
        ("", ""),
    ],
)
def test_slug_parity(raw, expected):
    assert conv.slug(raw) == expected


# --------------------------------------------------------------------------
# happy path / report
# --------------------------------------------------------------------------

def test_happy_path_report_and_rows(fixture):
    tmp_path, run = fixture
    res = run()
    assert res.returncode == 0, res.stderr + res.stdout
    assert "admin1 resolved 37/37" in res.stdout
    assert "unresolved admin2 keys (rows SKIPPED):" in res.stdout
    assert "final locality rows: 6" in res.stdout
    assert "collision groups: 1 (rows suffixed with -<geonameid>: 2)" \
        in res.stdout

    rows = _read_out(tmp_path)
    assert len(rows) == 6  # 7 P-rows minus the unresolved-admin2 row
    for row in rows:
        assert row["kind"] == "locality"
        assert list(row.keys()) == HEADER
        assert row["source"].startswith("geonames NG dump 2024-05-17 (CC-BY 4.0) geonameid=")

    by_gid = {r["source"].rsplit("=", 1)[1]: r for r in rows}
    assert by_gid["1001"]["code"] == "NG-FCT-ABUJA-MUNICIPAL-GARKI"
    assert by_gid["1001"]["parent_code"] == "NG-FCT-ABUJA-MUNICIPAL"
    # UTF-8 display name preserved; code from asciiname
    assert by_gid["1002"]["name"] == "Bùrén"
    assert by_gid["1002"]["code"] == "NG-FCT-ABAJI-BUREN"
    assert by_gid["1003"]["code"] == "NG-FCT-ABAJI-ST-MARYS"
    # admin2 override resolution
    assert by_gid["1004"]["parent_code"] == "NG-NASARAWA-NASARAWA-EGON"
    assert by_gid["1004"]["code"] == "NG-NASARAWA-NASARAWA-EGON-KARU-ROAD"
    # collision: BOTH suffixed with -<geonameid>
    assert by_gid["1007"]["code"] == "NG-FCT-ABAJI-NEW-SITE-1007"
    assert by_gid["1008"]["code"] == "NG-FCT-ABAJI-NEW-SITE-1008"
    # unresolved admin2 row skipped entirely
    assert "1005" not in by_gid
    # never state-parented (parent is always a seeded LGA code)
    seeded_lga_codes = {c for c, _, _ in LGAS}
    assert all(r["parent_code"] in seeded_lga_codes for r in rows)


def test_unresolved_admin2_reported_with_count(fixture):
    tmp_path, run = fixture
    res = run()
    assert res.returncode == 0
    line = [l for l in res.stdout.splitlines()
            if l.strip().startswith("NG.") and "Atlantis" in l]
    assert line and line[0].rstrip().endswith("1 rows")
    assert "rows skipped: 1" in res.stdout


def test_non_p_class_filtered(fixture):
    tmp_path, run = fixture
    res = run()
    assert res.returncode == 0
    assert "input P-class rows: 7" in res.stdout  # 8 data rows, 1 is class T


def test_zip_input_accepted(fixture):
    tmp_path, run = fixture
    with zipfile.ZipFile(tmp_path / "NG.zip", "w") as zf:
        zf.write(tmp_path / "NG.txt", "NG.txt")
    res = run(ng="NG.zip")
    assert res.returncode == 0, res.stderr + res.stdout
    assert len(_read_out(tmp_path)) == 6


def test_deterministic_rerun_byte_identical(fixture):
    tmp_path, run = fixture
    assert run().returncode == 0
    first = (tmp_path / "out.csv").read_bytes()
    assert run().returncode == 0
    second = (tmp_path / "out.csv").read_bytes()
    assert first == second
    # rows sorted by code
    rows = _read_out(tmp_path)
    codes = [r["code"] for r in rows]
    assert codes == sorted(codes)


def test_output_parses_as_documented_csv_shape(fixture):
    tmp_path, run = fixture
    assert run().returncode == 0
    raw = (tmp_path / "out.csv").read_text(encoding="utf-8")
    assert raw.splitlines()[0] == ",".join(HEADER)
    seeded_lgas = {code for code, _, _ in LGAS}
    for row in _read_out(tmp_path):
        assert row["kind"] == "locality"
        assert row["parent_code"] in seeded_lgas
        lat, lng = float(row["lat"]), float(row["lng"])
        assert -90.0 <= lat <= 90.0
        assert -180.0 <= lng <= 180.0
        gid = row["source"].rsplit("=", 1)[1]
        assert row["source"] == (
            f"geonames NG dump 2024-05-17 (CC-BY 4.0) geonameid={gid}"
        )


def test_dump_date_override_flag(fixture):
    tmp_path, run = fixture
    res = run("--dump-date", "2025-01-02")
    assert res.returncode == 0
    assert _read_out(tmp_path)[0]["source"].startswith(
        "geonames NG dump 2025-01-02 (CC-BY 4.0)")


# --------------------------------------------------------------------------
# failure modes
# --------------------------------------------------------------------------

def test_unresolved_admin1_hard_fail(fixture):
    tmp_path, run = fixture
    # Drop one state from the admin1 file (and no override covers it).
    p = tmp_path / "admin1CodesASCII.txt"
    lines = p.read_text(encoding="utf-8").splitlines()
    kept = [l for l in lines if not l.startswith("NG.01\t")]
    assert len(kept) == 36
    p.write_text("\n".join(kept) + "\n", encoding="utf-8")
    res = run()
    assert res.returncode != 0
    assert "admin1 resolved 36/37" in res.stdout
    assert "HARD FAIL" in res.stdout
    assert not (tmp_path / "out.csv").exists()


def test_empty_input_hard_fails(tmp_path):
    (tmp_path / "NG.txt").write_text("", encoding="utf-8")
    (tmp_path / "admin1CodesASCII.txt").write_text("", encoding="utf-8")
    (tmp_path / "admin2Codes.txt").write_text("", encoding="utf-8")
    (tmp_path / "overrides.csv").write_text(
        "level,geonames_key,geo_places_code\n", encoding="utf-8")
    res = subprocess.run(
        [sys.executable, str(SCRIPT),
         "--ng", str(tmp_path / "NG.txt"),
         "--admin1", str(tmp_path / "admin1CodesASCII.txt"),
         "--admin2", str(tmp_path / "admin2Codes.txt"),
         "--overrides", str(tmp_path / "overrides.csv"),
         "--gazetteer", str(GAZETTEER),
         "--out", str(tmp_path / "out.csv")],
        capture_output=True, text=True,
    )
    assert res.returncode != 0
    assert "admin1 resolved 0/37" in res.stdout
    assert not (tmp_path / "out.csv").exists()


def test_missing_input_file_is_usage_error(tmp_path):
    res = subprocess.run(
        [sys.executable, str(SCRIPT),
         "--ng", str(tmp_path / "nope.zip"),
         "--admin1", str(tmp_path / "nope1.txt"),
         "--admin2", str(tmp_path / "nope2.txt"),
         "--overrides", str(tmp_path / "nope3.csv"),
         "--gazetteer", str(GAZETTEER),
         "--out", str(tmp_path / "out.csv")],
        capture_output=True, text=True,
    )
    assert res.returncode == 1
    assert "not found" in res.stderr


# --------------------------------------------------------------------------
# committed override CSV + stdlib-only gate
# --------------------------------------------------------------------------

def test_committed_override_csv_seeds_fct_and_nasarawa():
    committed = SEEDS_DIR / "data" / "geonames_ng_admin_overrides.csv"
    overrides = conv.load_overrides(committed)
    assert overrides[("admin1", "Federal Capital Territory")] == "NG-FCT"
    assert overrides[("admin1", "Nassarawa")] == "NG-NASARAWA"


def test_script_is_stdlib_only():
    tree = ast.parse(SCRIPT.read_text(encoding="utf-8"))
    imported = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            imported.update(a.name.split(".")[0] for a in node.names)
        elif isinstance(node, ast.ImportFrom) and node.level == 0:
            imported.add(node.module.split(".")[0])
    imported.discard("__future__")
    non_stdlib = sorted(m for m in imported
                        if m not in sys.stdlib_module_names)
    assert non_stdlib == []
