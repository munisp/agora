"""Tests for Agent B synthetic-entity seeds — SPEC-W17.

Covers: cardinality × SEED_SCALE math, +23480[0-9]XXXXXXX phone regex,
deterministic ids, FunnelEvent envelopes validated against the REAL W13
consumer parser (services/analytics-pipeline cac_events.parse_funnel_event),
stage progression with drop-off, FX contiguity + monotonicity-of-date +
≥365-point gate, the TigerBeetle manifest account_type=90 assertion, upsert
SQL shape through a fake conn, and DB-free --dry-run for every script.
No database required.
"""

from __future__ import annotations

import json
import re
import sys
from datetime import date
from decimal import Decimal
from pathlib import Path

import pytest

import _lib
import seed_agents
import seed_customers
import seed_events
import seed_fx

# The W13 consumer parser is the authoritative FunnelEvent schema check.
ANALYTICS_DIR = Path(__file__).resolve().parents[2] / "services" / "analytics-pipeline"
if str(ANALYTICS_DIR) not in sys.path:
    sys.path.insert(0, str(ANALYTICS_DIR))
from analytics_pipeline import cac_events  # noqa: E402

PHONE_RE = re.compile(r"^\+23480[0-9]\d{7}$")
W13_DATA_KEYS = {
    "event_id", "tenant_id", "entity_type", "entity_id", "event_name",
    "event_ts", "channel", "campaign_id", "lga_id", "amount_ngn",
    "idempotency_key",
}


@pytest.fixture()
def lgas() -> list[dict[str, str]]:
    # DB-free reference set via Agent A's CSV (774 rows).
    return seed_agents.resolve_lgas(None)


@pytest.fixture()
def channel_codes() -> list[str]:
    return seed_customers.resolve_channel_codes(None)


# --- cardinality × scale math ------------------------------------------------
def test_scale_math_matches_lib(monkeypatch):
    monkeypatch.setenv("SEED_SCALE", "0.05")
    assert _lib.scaled(5_000) == 250
    assert _lib.scaled(200_000) == 10_000
    assert int(seed_agents.CARDINALITY * 0.05) == _lib.scaled(seed_agents.CARDINALITY)
    assert int(seed_customers.CARDINALITY * 0.05) == _lib.scaled(seed_customers.CARDINALITY)
    # events: 50/customer × scale, floored at 1
    assert max(1, int(seed_events.EVENTS_PER_CUSTOMER * 0.05)) == 2
    monkeypatch.setenv("SEED_SCALE", "1.0")
    assert _lib.scaled(5_000) == 5_000
    assert _lib.scaled(200_000) == 200_000


# --- seed_agents ---------------------------------------------------------------
def test_agents_cardinality_and_deterministic_ids(lgas):
    a = seed_agents.build_rows(40, lgas)
    b = seed_agents.build_rows(40, lgas)
    assert len(a) == 40
    assert [r["id"] for r in a] == [r["id"] for r in b]  # stable across runs
    assert len({r["id"] for r in a}) == 40
    for i, r in enumerate(a):
        assert r["id"] == _lib.deterministic_id(seed_agents.natural_key(i))
        assert r["is_synthetic"] if "is_synthetic" in r else True
        assert r["active"] is True
        assert r["lga_id"] in {l["id"] for l in lgas}
        assert r["state"] in {l["state"] for l in lgas}
        # PII columns are digests only — never plaintext
        assert str(r["name_hash"]).split("$")[0] in {"argon2", "scrypt"}
        assert "+234" not in str(r["name_hash"]) and "+234" not in str(r["phone_hash"])


def test_phone_format_regex():
    for i in range(500):
        phone = seed_agents.make_phone(seed_agents.seeded_rng("agent", i))
        assert PHONE_RE.match(phone), phone
        assert len(phone) == len("+23480DXXXXXXX")  # +234 80 [0-9] XXXXXXX


def test_agents_lga_natural_key_matches_agent_a():
    # FK integrity with seed_lgas: same natural key format 'lga:<state>:<lga>'.
    assert seed_agents.lga_ref("Lagos", "Ikeja", "South West")["id"] == _lib.deterministic_id("lga:Lagos:Ikeja")


def test_agents_upsert_sql_shape(fake_conn, lgas):
    rows = seed_agents.build_rows(3, lgas)
    _lib.delete_by_ids(fake_conn, seed_agents.TABLE, [r["id"] for r in rows])
    _lib.upsert_rows(fake_conn, seed_agents.TABLE, seed_agents.COLUMNS, rows)
    sql = fake_conn.sql
    assert "DELETE FROM cac.agents WHERE id = ANY(%s)" in sql
    assert "INSERT INTO cac.agents" in sql
    assert "ON CONFLICT (id) DO UPDATE" in sql
    # every COLUMNS entry is a real DDL column
    assert set(seed_agents.COLUMNS) <= {"id", "name_hash", "phone_hash", "state", "lga_id", "active"}


def test_tb_manifest_account_type_90(lgas):
    rows = seed_agents.build_rows(7, lgas)
    manifest = seed_agents.build_tb_manifest(rows, 1.0)
    accounts = manifest["accounts"]
    assert len(accounts) == len(rows)  # one account per agent
    assert manifest["account_type"] == 90
    for acc, row in zip(accounts, rows):
        assert acc["account_type"] == 90  # §8.8 synthetic accounts
        assert acc["code"] == 302  # W14 AccountAgentFloat
        assert isinstance(acc["id"], int) and 0 <= acc["id"] < 2**128
        assert acc["user_data_128"] == row["id"]
        assert acc["is_synthetic"] is True
    assert len({a["id"] for a in accounts}) == len(accounts)  # unique TB ids


# --- seed_customers ------------------------------------------------------------
def test_customers_cardinality_determinism_and_schema(lgas, channel_codes):
    a = seed_customers.build_rows(60, lgas, channel_codes, today=date(2026, 8, 1))
    b = seed_customers.build_rows(60, lgas, channel_codes, today=date(2026, 8, 1))
    assert len(a) == 60
    assert [r["id"] for r in a] == [r["id"] for r in b]
    assert [r["channel_id"] for r in a] == [r["channel_id"] for r in b]  # stable assignment
    valid_channels = {seed_customers.channel_code_to_id(c) for c in channel_codes}
    valid_lgas = {l["id"] for l in lgas}
    for i, r in enumerate(a):
        assert r["id"] == _lib.deterministic_id(seed_customers.natural_key(i))
        assert r["channel_id"] in valid_channels
        assert r["lga_id"] in valid_lgas
        assert date(2024, 8, 2) <= r["acquired_on"] <= date(2026, 8, 1)
        assert str(r["phone_hash"]).split("$")[0] in {"argon2", "scrypt"}
        assert "+234" not in str(r["phone_hash"])
    assert set(seed_customers.COLUMNS) <= {
        "id", "name_hash", "phone_hash", "channel_id", "lga_id", "acquired_on",
    }


def test_customers_channel_distribution_spreads(lgas, channel_codes):
    rows = seed_customers.build_rows(1_500, lgas, channel_codes, today=date(2026, 8, 1))
    distinct = {r["channel_id"] for r in rows}
    assert len(distinct) >= 28  # ~all 32 channels reached at n=1500
    # field/agent-led channels dominate (top channel is one of the heavy four)
    from collections import Counter

    counts = Counter(r["channel_id"] for r in rows)
    heavy = {seed_customers.channel_code_to_id(c) for c in ("agent-network", "pos-agents", "ussd", "field-reps-door-to-door")}
    assert counts.most_common(1)[0][0] in heavy


def test_pidgin_strings_embedded_and_drive_generation():
    assert len(seed_customers.PIDGIN_NOTES) >= 8
    assert any("dey" in s or "na " in s or "wetin" in s.lower() for s in seed_customers.PIDGIN_NOTES)
    # pcm routes to the full cross-cutting name list; ha/yo/ig route to blocks
    assert seed_customers._NAME_BLOCKS["ha"]
    assert "pcm" in seed_customers.LANGUAGES


# --- seed_events (W13 FunnelEvent contract E) -----------------------------------
def _sample_customers(n: int, lgas, channel_codes):
    return seed_customers.build_rows(n, lgas, channel_codes, today=date(2025, 1, 1))


def _envelope_stream(lgas, channel_codes, customers=25, per_customer=6):
    ords = {l["id"]: i + 1 for i, l in enumerate(lgas)}
    for c in _sample_customers(customers, lgas, channel_codes):
        cust = {
            "id": c["id"],
            "channel": next(k for k in channel_codes if seed_customers.channel_code_to_id(k) == c["channel_id"]),
            "lga_id": c["lga_id"],
            "acquired_on": c["acquired_on"],
        }
        yield from seed_events.iter_customer_envelopes(cust, per_customer, ords)


def test_funnel_event_envelope_matches_w13_schema(lgas, channel_codes):
    envelopes = list(_envelope_stream(lgas, channel_codes))
    assert envelopes
    for env in envelopes:
        assert env["specversion"] == "1.0"
        assert env["type"] == cac_events.FUNNEL_EVENT_TYPE == "com.opendesk.cac.FunnelEvent"
        assert set(env["data"].keys()) == W13_DATA_KEYS
        parsed = cac_events.parse_funnel_event(env)  # REAL W13 consumer parser
        assert parsed is not None, env
        assert parsed.event_name in cac_events.EVENT_NAMES
        assert parsed.entity_type == "customer"
        assert parsed.tenant_id == seed_events.SEED_TENANT_ID
        assert parsed.idempotency_key == env["data"]["event_id"]
        assert parsed.lga_id is None or isinstance(parsed.lga_id, int)
        if parsed.event_name in ("converted", "first_txn"):
            assert parsed.amount_ngn is not None and parsed.amount_ngn > Decimal("0")
        else:
            assert parsed.amount_ngn is None
        assert parsed.channel in set(channel_codes)


def test_funnel_stage_progression_and_dropoff(lgas, channel_codes):
    envelopes = list(_envelope_stream(lgas, channel_codes, customers=200, per_customer=50))
    assert len(envelopes) == 200 * 50
    rank = {name: i for i, (name, _) in enumerate(seed_events.STAGES)}
    rank["lost"] = len(seed_events.STAGES)
    by_customer: dict[str, list] = {}
    for env in envelopes:
        by_customer.setdefault(env["data"]["entity_id"], []).append(env)
    # per-customer: stage order non-decreasing in time
    for envs in by_customer.values():
        ranks = [rank[e["data"]["event_name"]] for e in envs]
        assert ranks == sorted(ranks)
        stamps = [e["data"]["event_ts"] for e in envs]
        assert stamps == sorted(stamps)
    # aggregate: realistic drop-off across the funnel
    from collections import Counter

    counts = Counter(e["data"]["event_name"] for e in envelopes)
    assert counts["lead_created"] > counts["contacted"] > counts["opted_in"]
    assert counts["opted_in"] > counts["qualified"] > counts["converted"]
    assert counts["first_txn"] <= counts["converted"]
    assert 0 < counts["converted"] < counts["qualified"]


def test_funnel_event_ids_deterministic(lgas, channel_codes):
    a = [e["id"] for e in _envelope_stream(lgas, channel_codes, customers=5, per_customer=4)]
    b = [e["id"] for e in _envelope_stream(lgas, channel_codes, customers=5, per_customer=4)]
    assert a == b  # idempotent producer: byte-identical replay
    assert len(set(a)) == len(a)


def test_events_outbox_roundtrip(tmp_path, lgas, channel_codes):
    out = tmp_path / "outbox.jsonl"
    envs = list(_envelope_stream(lgas, channel_codes, customers=3, per_customer=3))
    n = seed_events.write_outbox(envs, str(out))
    assert n == len(envs)
    lines = out.read_text().splitlines()
    assert len(lines) == n
    for line in lines:
        assert cac_events.parse_funnel_event(json.loads(line)) is not None


# --- seed_fx ---------------------------------------------------------------------
def test_fx_contiguity_monotonic_dates_and_anchors():
    rows = seed_fx.build_fx_series()
    assert len(rows) == 1_826  # 2021-08-01 .. 2026-07-31 inclusive
    assert len(rows) >= seed_fx.MIN_POINTS_GATE
    dates = [r["series_date"] for r in rows]
    assert dates == sorted(dates)  # monotonic dates
    for prev, cur in zip(dates, dates[1:]):
        assert (cur - prev).days == 1  # contiguous
    assert 380.0 <= rows[0]["usd_ngn_official"] <= 440.0  # ≈₦410 start (2021-08)
    assert 1_400.0 <= rows[-1]["usd_ngn_official"] <= 1_700.0  # ≈₦1,550 regime (2026-08)
    for r in rows:
        assert r["usd_ngn_parallel"] >= r["usd_ngn_official"]  # spread always positive
        assert r["is_synthetic"] if "is_synthetic" in r else True
    assert rows[0]["id"] == _lib.deterministic_id("fx:2021-08-01")


def test_fx_gate_fails_loud_on_short_series():
    with pytest.raises(RuntimeError, match="contiguity gate"):
        seed_fx.build_fx_series(date(2024, 1, 1), date(2024, 6, 30))  # 182 points < 365


def test_fx_manifest_shape_and_path(tmp_path, monkeypatch):
    monkeypatch.setenv("SEED_MANIFEST_DIR", str(tmp_path))
    rows = seed_fx.build_fx_series()
    manifest = seed_fx.build_fx_manifest(rows, 0.05)
    assert manifest["gold_target"] == "cac_gold.usd_shadow_prices"
    assert manifest["rowcount"] == len(rows)
    for mrow, srow in zip(manifest["rows"], rows):
        assert set(mrow.keys()) == {"day", "official_ngn", "parallel_ngn"}  # EXACT Agent C contract
        assert mrow["day"] == srow["series_date"].isoformat()
        assert mrow["official_ngn"] == srow["usd_ngn_official"]
        assert mrow["parallel_ngn"] == srow["usd_ngn_parallel"]
    path = seed_fx.write_fx_manifest(manifest)
    assert path == tmp_path / "fx_series.json"
    loaded = json.loads(path.read_text())
    assert loaded["rows"][0]["day"] == "2021-08-01"


# --- DB-free --dry-run for every script ------------------------------------------
def test_dry_runs_are_db_free(capsys):
    assert seed_fx.main(["--dry-run"]) == 0
    assert seed_events.main(["--dry-run", "--scale", "0.05"]) == 0
    assert seed_agents.main(["--dry-run", "--scale", "0.002"]) == 0  # 10 agents
    assert seed_customers.main(["--dry-run", "--scale", "0.0005"]) == 0  # 100 customers
    out = capsys.readouterr().out
    assert "dry-run" in out
    assert "com.opendesk.cac.FunnelEvent" in out  # sample envelope printed
