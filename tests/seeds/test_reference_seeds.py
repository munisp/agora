"""Tests for Agent A reference-data seeds — SPEC-W17.

Covers: determinism (same ids across runs), cardinality (774 / 8,812 / 32 /
768), upsert SQL shape through a fake conn, and DB-free --dry-run for every
script. No database required.
"""

from __future__ import annotations

import csv

import _lib
import seed_channel_costs
import seed_channels
import seed_lgas
import seed_locale
import seed_wards

ZONES = {"North Central", "North East", "North West", "South East", "South South", "South West"}


# --- LGA CSV + seed_lgas ---------------------------------------------------------
def test_lga_csv_has_exactly_774_real_rows():
    with open(seed_lgas.DATA_CSV, newline="", encoding="utf-8") as fh:
        rows = list(csv.DictReader(fh))
    assert len(rows) == 774
    assert set(rows[0].keys()) == {"lga_name", "state", "zone"}
    assert len({r["state"] for r in rows}) == 37  # 36 states + FCT
    assert all(r["zone"] in ZONES for r in rows)
    assert len({(r["state"], r["lga_name"]) for r in rows}) == 774  # no dups


def test_lgas_cardinality_and_determinism():
    a, b = seed_lgas.build_rows(), seed_lgas.build_rows()
    assert len(a) == 774
    assert [r["id"] for r in a] == [r["id"] for r in b]  # same ids across runs
    assert len({r["id"] for r in a}) == 774
    ikeja = next(r for r in a if r["lga_name"] == "Ikeja")
    assert ikeja["id"] == _lib.deterministic_id("lga:Lagos:Ikeja")
    assert ikeja["zone"] == "South West"


# --- seed_wards -------------------------------------------------------------------
def test_wards_exactly_8812_and_deterministic():
    a, b = seed_wards.build_rows(1.0), seed_wards.build_rows(1.0)
    assert len(a) == 8812
    assert [r["id"] for r in a] == [r["id"] for r in b]
    assert len({r["id"] for r in a}) == 8812
    lga_ids = {r["id"] for r in seed_lgas.build_rows()}
    assert {r["lga_id"] for r in a} <= lga_ids  # FK integrity (in-memory)


def test_wards_distribution_is_11_or_12_per_lga_and_stable_names():
    rows = seed_wards.build_rows(1.0)
    per_lga: dict[str, int] = {}
    for r in rows:
        per_lga[r["lga_id"]] = per_lga.get(r["lga_id"], 0) + 1
    assert set(per_lga.values()) == {11, 12}
    assert sum(1 for v in per_lga.values() if v == 12) == 8812 - 11 * 774  # 298
    sample = next(r for r in rows if "Ikeja" in r["ward_name"])
    assert sample["ward_name"].startswith("Ward 01 — Ikeja")


def test_wards_scale_down():
    rows = seed_wards.build_rows(0.05)
    assert len(rows) == int(8812 * 0.05)  # 440


# --- seed_channels ------------------------------------------------------------------
def test_channels_32_hand_curated():
    rows = seed_channels.build_rows()
    assert len(rows) == 32
    codes = [r["channel_code"] for r in rows]
    assert len(set(codes)) == 32
    for must in ("ussd", "sms", "whatsapp-business", "voice-ivr", "radio-network-fm",
                 "agent-network", "pos-agents", "cooperatives", "churches", "mosques",
                 "market-associations"):
        assert must in codes
    classes = {r["channel_class"] for r in rows}
    assert classes == {"above-the-line", "below-the-line"}
    assert all(float(r["base_cost_ngn"]) > 0 for r in rows)
    ids = [r["id"] for r in rows]
    assert ids == [r["id"] for r in seed_channels.build_rows()]  # deterministic


# --- seed_channel_costs ----------------------------------------------------------------
def test_channel_costs_768_rows_24_months_each():
    rows = seed_channel_costs.build_rows()
    assert len(rows) == 768
    per_channel: dict[str, list[str]] = {}
    for r in rows:
        per_channel.setdefault(str(r["channel_id"]), []).append(str(r["month"]))
    assert len(per_channel) == 32
    assert all(len(months) == 24 and len(set(months)) == 24 for months in per_channel.values())
    assert all(float(r["unit_cost_ngn"]) > 0 for r in rows)


def test_channel_costs_manifest_consumer_contract(monkeypatch, tmp_path):
    """Agent C consumer contract: $SEED_MANIFEST_DIR/channel_unit_costs.json with
    row fields exactly {month, channel_code, channel_name, channel_class,
    unit_cost_ngn, currency}."""
    import json

    monkeypatch.setenv("SEED_MANIFEST_DIR", str(tmp_path / "seed_manifests"))
    rows = seed_channel_costs.build_rows()
    path = seed_channel_costs.write_manifest(rows)
    assert path.name == "channel_unit_costs.json"
    assert path.parent.name == "seed_manifests"
    payload = json.loads(path.read_text(encoding="utf-8"))
    assert payload["rowcount"] == 768 and len(payload["rows"]) == 768
    for r in payload["rows"]:
        assert set(r.keys()) == {
            "month", "channel_code", "channel_name", "channel_class",
            "unit_cost_ngn", "currency",
        }
        assert r["currency"] == "NGN"
        assert r["channel_class"] in {"above-the-line", "below-the-line"}
        assert float(r["unit_cost_ngn"]) > 0
    codes = {r["channel_code"] for r in payload["rows"]}
    assert len(codes) == 32 and "ussd" in codes


def test_channel_costs_manifest_default_dir(monkeypatch):
    monkeypatch.delenv("SEED_MANIFEST_DIR", raising=False)
    assert seed_channel_costs.DEFAULT_MANIFEST_DIR == "/var/tmp/seed_manifests"
    assert seed_channel_costs.MANIFEST_FILENAME == "channel_unit_costs.json"


def test_channel_costs_deterministic_walk():
    a = [r["unit_cost_ngn"] for r in seed_channel_costs.build_rows()]
    b = [r["unit_cost_ngn"] for r in seed_channel_costs.build_rows()]
    assert a == b  # seeded per channel via sha256 — byte-stable across runs
    walk = seed_channel_costs.walk_costs("ussd", 25.0)
    assert len(walk) == 24 and walk != sorted(walk)  # a walk, not a flat line


# --- seed_locale ---------------------------------------------------------------------------
def test_locale_scan_covers_packs_and_locales():
    packs = seed_locale.scan_packs()
    assert len(packs) >= 30  # every industries/*.yaml pack scanned
    table = seed_locale.render_table(packs)
    assert "pcm" in table and "ha" in table and "yo" in table and "ig" in table
    agritech = dict(packs)["agritech"]
    assert {"pcm", "ha"} <= agritech  # known Hausa+Pidgin carrier (SPEC-W15)
    assert any("en" in locs for _, locs in packs)


# --- loader contract via fake conn (DB-free) --------------------------------------------
def test_seed_lgas_loader_contract(fake_conn, monkeypatch, tmp_path):
    monkeypatch.setenv("SEED_REPORT_PATH", str(tmp_path / "r.jsonl"))
    monkeypatch.setenv("SEED_KAFKA", "off")
    monkeypatch.setattr(_lib, "get_conn", lambda: fake_conn)
    assert seed_lgas.main([]) == 0
    sql = fake_conn.sql
    assert "DELETE FROM cac.lgas WHERE id = ANY(%s)" in sql
    assert "INSERT INTO cac.lgas" in sql and "ON CONFLICT (id) DO UPDATE" in sql
    assert "INSERT INTO cac.seed_run_log" in sql
    assert fake_conn.commits == 1


# --- --dry-run works DB-free for every script --------------------------------------------
def test_dry_run_db_free(monkeypatch, capsys):
    monkeypatch.delenv("DATABASE_URL", raising=False)  # prove no DB needed
    for mod in (seed_lgas, seed_wards, seed_channels, seed_channel_costs, seed_locale):
        assert mod.main(["--dry-run"]) == 0, mod.__name__
    out = capsys.readouterr().out
    assert "rows=774" in out and "rows=8812" in out
    assert "rows=32" in out and "rows=768" in out
    assert "dry-run" in out
