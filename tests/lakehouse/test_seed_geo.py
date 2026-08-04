"""Pure-function tests for the W17 lakehouse seed jobs (SPEC-W17, Agent C).

Runs DB-free and Spark-free on a clean venv: the modules under test guard
their pyspark imports, and every assertion below exercises only the pure
cores (deterministic generation, rejection sampling, manifest math).

Gates covered:
  * determinism — identical rows across two runs;
  * cardinality — exactly 50,000 geo points at scale 1.0 (and 2,500 at the
    CI scale 0.05); coverage stations = lgas x int(200 x scale);
  * geography — every point inside the Nigeria bbox; sampled points inside
    their anchor polygon;
  * station codes — NG-<STATE>-<nnn> shape, per-state contiguous sequences;
  * gold load math — shadow_mid/spread_bps, JSONL+JSON-array manifests,
    deterministic run-log ids;
  * import guard — importing the three jobs pulls in NO pyspark (subprocess
    check, so a polluted test session can't false-pass).
"""

from __future__ import annotations

import json
import os
import subprocess
import sys

import pytest

JOBS_DIR = os.path.join(
    os.path.dirname(__file__), "..", "..", "infra", "lakehouse", "spark", "jobs"
)
sys.path.insert(0, os.path.abspath(JOBS_DIR))
SEEDS_DIR = os.path.abspath(
    os.path.join(os.path.dirname(__file__), "..", "..", "scripts", "seeds")
)
sys.path.insert(0, SEEDS_DIR)  # writer modules for the round-trip tests

import seed_coverage  # noqa: E402
import seed_geo_points  # noqa: E402
import seed_gold_load  # noqa: E402
import seed_channel_costs  # noqa: E402  (Agent A writer — round-trip only, not modified)
import seed_fx  # noqa: E402  (Agent B writer — round-trip only, not modified)

MIN_LAT, MAX_LAT, MIN_LNG, MAX_LNG = seed_geo_points.NIGERIA_BBOX


# ---------------------------------------------------------------------------
# geo points
# ---------------------------------------------------------------------------

def test_geo_points_exact_count_at_scale_1():
    rows = seed_geo_points.generate_points(scale=1.0)
    assert len(rows) == 50_000


def test_geo_points_scale_math():
    assert len(seed_geo_points.generate_points(scale=0.05)) == 2_500
    assert len(seed_geo_points.generate_points(scale=0.0)) == 0


def test_geo_points_deterministic():
    a = seed_geo_points.generate_points(scale=0.01)
    b = seed_geo_points.generate_points(scale=0.01)
    assert a == b
    assert len({r["point_id"] for r in a}) == len(a)  # unique ids


def test_geo_points_inside_nigeria_bbox():
    rows = seed_geo_points.generate_points(scale=1.0)
    assert rows, "expected points at scale 1"
    for r in rows:
        assert MIN_LAT <= r["lat"] <= MAX_LAT, r
        assert MIN_LNG <= r["lng"] <= MAX_LNG, r
        assert r["is_synthetic"] is True
        assert r["geom"].startswith("POINT(")


def test_sampled_points_inside_anchor_polygon():
    anchors = seed_geo_points.default_anchors()
    rows = seed_geo_points.generate_points(anchors=anchors, scale=0.01)
    by_anchor = {}
    for r in rows:
        by_anchor.setdefault(r["anchor_id"], []).append(r)
    for anchor in anchors:
        ring = seed_geo_points.synthesize_polygon(anchor)
        for r in by_anchor.get(anchor["anchor_id"], []):
            # rejection sampling only accepts in-polygon points
            assert seed_geo_points.point_in_polygon(r["lng"], r["lat"], ring)


def test_polygon_synthesis_deterministic_and_closed():
    anchor = seed_geo_points.default_anchors()[0]
    ring1 = seed_geo_points.synthesize_polygon(anchor)
    ring2 = seed_geo_points.synthesize_polygon(anchor)
    assert ring1 == ring2
    assert ring1[0] == ring1[-1]  # closed ring
    assert len(ring1) == 9  # 8 vertices + closing vertex


def test_geojson_export_shape():
    fc = seed_geo_points.anchors_to_geojson(seed_geo_points.default_anchors()[:3])
    assert fc["type"] == "FeatureCollection"
    assert len(fc["features"]) == 3
    feat = fc["features"][0]
    assert feat["geometry"]["type"] == "Polygon"
    assert feat["properties"]["is_synthetic"] is True


def test_allocate_counts_sums_exactly():
    anchors = seed_geo_points.default_anchors()
    for total in (1, 37, 50_000):
        counts = seed_geo_points.allocate_counts(anchors, total)
        assert sum(counts) == total
        assert all(c >= 0 for c in counts)


# ---------------------------------------------------------------------------
# coverage grid
# ---------------------------------------------------------------------------

TOY_LGAS = [
    {"lga_id": "lga-1", "name": "Ikeja", "state": "Lagos", "lat": 6.60, "lng": 3.35},
    {"lga_id": "lga-2", "name": "Eti-Osa", "state": "Lagos", "lat": 6.45, "lng": 3.60},
    {"lga_id": "lga-3", "name": "Calabar", "state": "Cross River", "lat": 4.98, "lng": 8.33},
]


def test_coverage_cardinality_and_determinism():
    a = seed_coverage.generate_stations(lgas=TOY_LGAS, scale=1.0)
    b = seed_coverage.generate_stations(lgas=TOY_LGAS, scale=1.0)
    assert a == b
    assert len(a) == 3 * 200
    assert len({r["station_id"] for r in a}) == len(a)
    assert len(seed_coverage.generate_stations(lgas=TOY_LGAS, scale=0.05)) == 3 * 10


def test_coverage_station_code_format_and_sequences():
    import re

    rows = seed_coverage.generate_stations(lgas=TOY_LGAS, scale=1.0)
    pattern = re.compile(r"^NG-[A-Z0-9]+-\d{3,}$")
    by_state: dict[str, list[int]] = {}
    for r in rows:
        assert pattern.match(r["station_code"]), r["station_code"]
        assert r["station_code"].startswith(f"NG-{seed_coverage.state_slug(r['state'])}-")
        seq = int(r["station_code"].rsplit("-", 1)[1])
        by_state.setdefault(r["state"], []).append(seq)
    for state, seqs in by_state.items():
        assert sorted(seqs) == list(range(1, len(seqs) + 1))  # contiguous per state


def test_coverage_band_and_frequency_ranges():
    rows = seed_coverage.generate_stations(lgas=TOY_LGAS, scale=1.0)
    bands = {r["band"] for r in rows}
    assert bands <= {"FM", "AM"}
    for r in rows:
        if r["band"] == "FM":
            assert 87.5 <= r["frequency"] <= 108.0
        else:
            assert 0.531 <= r["frequency"] <= 1.602
        assert MIN_LAT <= r["lat"] <= MAX_LAT
        assert MIN_LNG <= r["lng"] <= MAX_LNG
        assert r["is_synthetic"] is True


# ---------------------------------------------------------------------------
# gold load pure math
# ---------------------------------------------------------------------------

def test_fx_shadow_math():
    rows = seed_gold_load.normalize_fx_rows(
        [{"day": "2024-01-01", "official_ngn": 900.0, "parallel_ngn": 1200.0}]
    )
    assert rows[0]["shadow_mid"] == 1050.0
    assert rows[0]["spread_bps"] == pytest.approx(3333.33, abs=0.01)
    assert rows[0]["is_synthetic"] is True


def test_fx_alias_fields_accepted():
    rows = seed_gold_load.normalize_fx_rows(
        [{"day": "2024-01-02", "rate_official": 1000.0, "rate_parallel": 1000.0}]
    )
    assert rows[0]["spread_bps"] == 0.0


def test_cost_rows_projection():
    rows = seed_gold_load.normalize_cost_rows(
        [{"month": "2024-03-01", "channel_code": "ussd", "unit_cost_ngn": "450.5"}]
    )
    assert rows[0]["channel_name"] == "ussd"
    assert rows[0]["currency"] == "NGN"
    assert rows[0]["unit_cost_ngn"] == 450.5


def test_load_manifest_array_and_jsonl(tmp_path):
    arr = tmp_path / "a.json"
    arr.write_text(json.dumps([{"x": 1}, {"x": 2}]))
    assert seed_gold_load.load_manifest(str(arr)) == [{"x": 1}, {"x": 2}]
    jl = tmp_path / "b.json"
    jl.write_text('{"x": 1}\n{"x": 2}\n')
    assert seed_gold_load.load_manifest(str(jl)) == [{"x": 1}, {"x": 2}]
    assert seed_gold_load.load_manifest(str(tmp_path / "missing.json")) == []


def test_load_manifest_envelope_unwrapped(tmp_path):
    """The writers' real format: pretty-printed dict envelope with "rows"."""
    p = tmp_path / "env.json"
    p.write_text(json.dumps({"table": "t", "rowcount": 2, "rows": [{"x": 1}, {"x": 2}]}, indent=2))
    assert seed_gold_load.load_manifest(str(p)) == [{"x": 1}, {"x": 2}]


def test_load_manifest_envelope_without_rows_fails_loud(tmp_path):
    p = tmp_path / "bad.json"
    p.write_text(json.dumps({"table": "t", "rowcount": 5}))
    with pytest.raises(ValueError, match="rows"):
        seed_gold_load.load_manifest(str(p))
    p.write_text(json.dumps({"rows": "not-a-list"}))
    with pytest.raises(ValueError, match="rows"):
        seed_gold_load.load_manifest(str(p))


# ---------------------------------------------------------------------------
# writer -> loader round-trips (REAL manifests from the actual writers,
# generated DB-free via their pure build/write functions — loader-side fix
# for the verifier-reproduced JSONDecodeError on both envelopes)
# ---------------------------------------------------------------------------

def test_roundtrip_channel_costs_writer_to_loader(tmp_path, monkeypatch):
    monkeypatch.setenv("SEED_MANIFEST_DIR", str(tmp_path))
    rows = seed_channel_costs.build_rows(1.0)  # fixed 32 channels x 24 months grid
    path = seed_channel_costs.write_manifest(rows)  # envelope, pretty-printed
    assert os.path.dirname(str(path)) == str(tmp_path)

    loaded = seed_gold_load.load_manifest(str(path))
    assert len(loaded) == 32 * 24 == 768
    for r in loaded:
        assert set(r) == {
            "month", "channel_code", "channel_name",
            "channel_class", "unit_cost_ngn", "currency",
        }
        assert r["currency"] == "NGN"
    # and they must flow through the gold projection unchanged in count
    assert len(seed_gold_load.normalize_cost_rows(loaded)) == 768


def test_roundtrip_fx_writer_to_loader(tmp_path, monkeypatch):
    from datetime import date

    monkeypatch.setenv("SEED_MANIFEST_DIR", str(tmp_path))
    # small DB-free series: exactly the >=365-point contiguity gate window
    rows = seed_fx.build_fx_series(date(2021, 8, 1), date(2022, 7, 31))
    manifest = seed_fx.build_fx_manifest(rows, scale=0.05)
    path = seed_fx.write_fx_manifest(manifest)
    assert os.path.dirname(str(path)) == str(tmp_path)

    loaded = seed_gold_load.load_manifest(str(path))
    assert len(loaded) == 365 == len(rows)
    for r in loaded:
        assert set(r) == {"day", "official_ngn", "parallel_ngn"}
        assert r["parallel_ngn"] >= r["official_ngn"]  # writer contract
    normalized = seed_gold_load.normalize_fx_rows(loaded)
    assert len(normalized) == 365
    assert all(n["spread_bps"] is not None and n["spread_bps"] >= 0 for n in normalized)


def test_seed_run_log_row_deterministic():
    r1 = seed_gold_load.seed_run_log_row("t", 10, "runner", "abc123")
    r2 = seed_gold_load.seed_run_log_row("t", 10, "runner", "abc123")
    assert r1 == r2
    assert r1["rowcount"] == 10 and r1["status"] == "ok"
    # run identity = (table, git_sha, runner_id) — a different runner is a new run
    assert (
        r1["run_id"]
        != seed_gold_load.seed_run_log_row("t", 10, "runner-2", "abc123")["run_id"]
    )


def test_dry_run_cli_driverless(capsys):
    """--dry-run prints counts with no Spark stack and no writes (contract B)."""
    assert seed_geo_points.main(["--dry-run", "--scale", "0.01"]) == 0
    assert "DRY-RUN: 500 rows" in capsys.readouterr().out
    assert seed_coverage.main(["--dry-run", "--scale", "1.0"]) == 0
    out = capsys.readouterr().out
    assert "DRY-RUN" in out and "7400 rows" in out  # 37 fallback LGAs x 200
    assert seed_gold_load.main(["--dry-run"]) == 0
    assert "DRY-RUN" in capsys.readouterr().out


# ---------------------------------------------------------------------------
# import guard — no pyspark at test time
# ---------------------------------------------------------------------------

@pytest.mark.parametrize(
    "module", ["seed_geo_points", "seed_coverage", "seed_gold_load"]
)
def test_module_imports_without_pyspark(module):
    code = (
        "import sys; sys.path.insert(0, {dir!r}); "
        "import {mod}; "
        "assert 'pyspark' not in sys.modules, 'pyspark leaked at import time'; "
        "print('OK {mod}')"
    ).format(dir=os.path.abspath(JOBS_DIR), mod=module)
    proc = subprocess.run(
        [sys.executable, "-c", code], capture_output=True, text=True, timeout=120
    )
    assert proc.returncode == 0, proc.stderr
    assert f"OK {module}" in proc.stdout
