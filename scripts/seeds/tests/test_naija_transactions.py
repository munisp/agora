"""Tests for scripts/seeds/naija_transactions.py — SPEC-W33 §2 A1 gates.

GA1 determinism (same seed → byte-equal outputs, all five files), GA3
distribution bands (amount medians/p95, hour curve, salary window, round-number
bias), GA2 label completeness (every event labeled, every fraud event has a
scenario, benign hard negatives present and labeled false), GA4 PII-free
outputs (no plaintext names / +234 phones / BVN — W28-style salted SHA-256
only). Self-contained: inserts scripts/seeds on sys.path itself (no conftest).
"""

from __future__ import annotations

import json
import re
import subprocess
import sys
from datetime import datetime
from pathlib import Path

import pytest

SEEDS_DIR = Path(__file__).resolve().parents[1]
if str(SEEDS_DIR) not in sys.path:
    sys.path.insert(0, str(SEEDS_DIR))

import naija_transactions as nt  # noqa: E402

OUTPUT_NAMES = ("events.jsonl", "persons.jsonl", "graph_edges.jsonl", "labels.json", "manifest.json")


def _read_jsonl(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()]


def _hour(event: dict) -> int:
    return datetime.fromisoformat(event["ts"].replace("Z", "+00:00")).hour


def _median(xs: list[float]) -> float:
    s = sorted(xs)
    n = len(s)
    return s[n // 2] if n % 2 else (s[n // 2 - 1] + s[n // 2]) / 2


def _p95(xs: list[float]) -> float:
    s = sorted(xs)
    return s[int(0.95 * (len(s) - 1))]


# ---------------------------------------------------------------------------
# GA1 — determinism: same seed → byte-equal outputs
# ---------------------------------------------------------------------------
def test_determinism_same_seed_byte_equal(tmp_path: Path):
    params = dict(seed=123, days=30, persons=60, fraud_rate=0.02)
    nt.generate_dataset(**params, out_dir=tmp_path / "run1")
    nt.generate_dataset(**params, out_dir=tmp_path / "run2")
    for name in OUTPUT_NAMES:
        a = (tmp_path / "run1" / name).read_bytes()
        b = (tmp_path / "run2" / name).read_bytes()
        assert a == b, f"{name} not byte-equal across identical runs"
        assert a, f"{name} is empty"


def test_determinism_manifest_sha256_matches_files(tmp_path: Path):
    import hashlib

    nt.generate_dataset(seed=123, days=30, persons=60, fraud_rate=0.02, out_dir=tmp_path)
    manifest = json.loads((tmp_path / "manifest.json").read_text(encoding="utf-8"))
    assert manifest["seed"] == 123
    for name, hexdigest in manifest["sha256"].items():
        actual = hashlib.sha256((tmp_path / name).read_bytes()).hexdigest()
        assert actual == hexdigest, f"manifest sha256 mismatch for {name}"


def test_different_seed_changes_output(tmp_path: Path):
    nt.generate_dataset(seed=1, days=20, persons=40, out_dir=tmp_path / "s1")
    nt.generate_dataset(seed=2, days=20, persons=40, out_dir=tmp_path / "s2")
    assert (tmp_path / "s1" / "events.jsonl").read_bytes() != (
        tmp_path / "s2" / "events.jsonl"
    ).read_bytes()


def test_cli_smoke_subprocess(tmp_path: Path):
    """CLI writes <out>/naija_txn/<seed>/ with all outputs + manifest, exit 0."""
    out = tmp_path / "cli"
    proc = subprocess.run(
        [sys.executable, str(SEEDS_DIR / "naija_transactions.py"),
         "--seed", "7", "--out", str(out), "--persons", "40", "--days", "20"],
        capture_output=True, text=True, timeout=300,
    )
    assert proc.returncode == 0, proc.stderr
    ds = out / "naija_txn" / "7"
    for name in OUTPUT_NAMES:
        assert (ds / name).stat().st_size > 0, f"missing/empty {name}"


# ---------------------------------------------------------------------------
# GA3 — distribution bands (one shared moderate-size dataset)
# ---------------------------------------------------------------------------
@pytest.fixture(scope="module")
def dataset(tmp_path_factory: pytest.TempPathFactory) -> dict:
    out = tmp_path_factory.mktemp("naija-ds")
    manifest = nt.generate_dataset(seed=42, days=90, persons=400, fraud_rate=0.015, out_dir=out)
    return {
        "events": _read_jsonl(out / "events.jsonl"),
        "persons": _read_jsonl(out / "persons.jsonl"),
        "edges": _read_jsonl(out / "graph_edges.jsonl"),
        "labels": json.loads((out / "labels.json").read_text(encoding="utf-8"))["entries"],
        "manifest": manifest,
    }


def _amounts(events: list[dict], channel: str) -> list[float]:
    return [e["amount_ngn"] for e in events if e["event_type"] == channel and not e["fraud"]]


def test_transfer_amount_band(dataset):
    amounts = _amounts(dataset["events"], "transfer")
    assert len(amounts) > 1_000
    assert 7_000 <= _median(amounts) <= 10_500          # documented median ≈ ₦8,500
    assert 80_000 <= _p95(amounts) <= 180_000           # documented p95 ≈ ₦120k


def test_pos_and_agent_amount_bands(dataset):
    pos = _amounts(dataset["events"], "pos")
    assert 2_600 <= _median(pos) <= 3_900               # documented median ≈ ₦3,200
    cashin = _amounts(dataset["events"], "agent_cashin")
    assert 4_000 <= _median(cashin) <= 6_200            # documented median ≈ ₦5,000


def test_airtime_ussd_band(dataset):
    for channel in ("airtime", "ussd"):
        amounts = _amounts(dataset["events"], channel)
        assert amounts, channel
        assert min(amounts) >= 100 and max(amounts) <= 2_000  # ₦100–₦2,000 regime


def test_salary_band_and_window(dataset):
    salary = [e for e in dataset["events"] if e["event_type"] == "salary"]
    assert len(salary) > 100
    assert all(50_000 <= e["amount_ngn"] <= 450_000 for e in salary)
    days = {datetime.fromisoformat(e["ts"].replace("Z", "+00:00")).day for e in salary}
    assert days <= set(range(25, 32))                    # salary spike 25th–31st


def test_hour_curve_evening_peak_transfers_ussd(dataset):
    evening_events = [e for e in dataset["events"] if e["event_type"] in ("transfer", "ussd")]
    frac_evening = sum(1 for e in evening_events if 17 <= _hour(e) <= 21) / len(evening_events)
    assert 0.38 <= frac_evening <= 0.62                  # documented 17–21h peak (~49%)
    # 17–21h must be the modal 5h window for transfer/ussd traffic.
    frac_morning = sum(1 for e in evening_events if 6 <= _hour(e) <= 10) / len(evening_events)
    assert frac_evening > frac_morning


def test_hour_curve_pos_midday(dataset):
    pos = [e for e in dataset["events"] if e["event_type"] == "pos"]
    frac_midday = sum(1 for e in pos if 11 <= _hour(e) <= 15) / len(pos)
    frac_evening = sum(1 for e in pos if 17 <= _hour(e) <= 21) / len(pos)
    assert frac_midday > 0.30
    assert frac_midday > frac_evening                    # POS peaks midday, not evening


def test_round_number_bias(dataset):
    transfers = [a for a in _amounts(dataset["events"], "transfer") if a >= 1_000]
    frac_1000 = sum(1 for a in transfers if a % 1_000 == 0) / len(transfers)
    frac_500 = sum(1 for a in transfers if a % 500 == 0) / len(transfers)
    assert 0.28 <= frac_1000 <= 0.55                     # ₦1,000 multiples over-represented
    assert frac_500 > frac_1000                          # ₦500 multiples superset


def test_geography_metro_weighting_and_bbox(dataset):
    persons = dataset["persons"]
    metro = sum(1 for p in persons if p["state"] in ("Lagos", "Kano", "FCT", "Rivers"))
    frac_metro = metro / len(persons)
    assert frac_metro > 4 / 37 + 0.05                    # metro-weighted above uniform
    assert all(nt.NG_LAT_MIN <= p["home_lat"] <= nt.NG_LAT_MAX for p in persons)
    assert all(nt.NG_LON_MIN <= p["home_lon"] <= nt.NG_LON_MAX for p in persons)
    lgas = {p["lga"] for p in persons}
    assert len(lgas) > 30                                # real spread over the 774 LGAs


def test_salary_window_propensity(dataset):
    """25th–31st carries a disproportionate share of spending events."""
    spending = [e for e in dataset["events"]
                if e["event_type"] in ("transfer", "pos", "airtime", "ussd")]
    in_window = sum(1 for e in spending
                    if 25 <= datetime.fromisoformat(e["ts"].replace("Z", "+00:00")).day <= 31)
    frac = in_window / len(spending)
    assert frac > 7 / 30.5 * 1.05                        # above a flat daily rate


# ---------------------------------------------------------------------------
# GA2 — label completeness
# ---------------------------------------------------------------------------
def test_every_event_labeled_and_fraud_has_scenario(dataset):
    for e in dataset["events"]:
        assert isinstance(e["fraud"], bool)
        assert "scenario" in e
        if e["fraud"]:
            assert e["scenario"], f"fraud event {e['event_id']} missing scenario"
        else:
            assert e["scenario"] is None or e["scenario"].startswith("benign_")


def test_labels_json_covers_every_flagged_event(dataset):
    entries = {l["entity_id"]: l for l in dataset["labels"]}
    for e in dataset["events"]:
        if e["scenario"] is not None:
            label = entries.get(e["event_id"])
            assert label is not None, f"{e['event_id']} flagged but absent from labels.json"
            assert label["scenario"] == e["scenario"]
            assert label["fraud"] == e["fraud"]
    for l in dataset["labels"]:
        assert set(l) == {"entity_id", "scenario", "fraud", "injected_at"}  # SPEC shape


def test_all_scenarios_present_including_hard_negatives(dataset):
    scenarios = {l["scenario"] for l in dataset["labels"]}
    for name in ("referral_ring", "sybil_cluster", "velocity_burst",
                 "geo_impossibility", "ghost_booking", "structuring"):
        assert name in scenarios, f"missing fraud scenario {name}"
    benign = {s for s in scenarios if s.startswith("benign_")}
    assert benign, "no benign hard negatives"
    for l in dataset["labels"]:
        if l["scenario"].startswith("benign_"):
            assert l["fraud"] is False                  # hard negatives labeled false
    counts = dataset["manifest"]["counts"]
    assert counts["events_benign_hard_negative"] > 0
    assert counts["events_fraud"] > 0
    # every manifest scenario has a recorded rate (I3)
    for name, stats in dataset["manifest"]["per_scenario"].items():
        assert stats["instances"] >= 1 and stats["rate"] >= 0.0, name


def test_fraud_rate_approximately_honored(dataset):
    counts = dataset["manifest"]["counts"]
    actual = counts["events_fraud"] / counts["events"]
    assert 0.005 <= actual <= 0.03                      # requested 0.015 ± construction slack


def test_geo_impossibility_implies_speed_over_120kmh(dataset):
    """Injected geo-impossibility pairs really imply > MAX_TRAVEL_KMH travel."""
    events = [e for e in dataset["events"] if e["scenario"] == "geo_impossibility"]
    assert len(events) >= 2
    by_agent: dict[str, list[dict]] = {}
    for e in events:
        by_agent.setdefault(e["person_id"], []).append(e)
    # Each injected capture must participate in an impossible pair (instances
    # may share an agent, so pair membership is proven existentially: some
    # same-agent capture within 46 min implies > MAX_TRAVEL_KMH).
    for agent_events in by_agent.values():
        agent_events.sort(key=lambda e: e["ts"])
        for a in agent_events:
            ta = datetime.fromisoformat(a["ts"].replace("Z", "+00:00"))
            assert any(
                0 < abs((datetime.fromisoformat(b["ts"].replace("Z", "+00:00")) - ta).total_seconds()) <= 46 * 60
                and nt.haversine_km(a["lat"], a["lon"], b["lat"], b["lon"])
                / (abs((datetime.fromisoformat(b["ts"].replace("Z", "+00:00")) - ta).total_seconds()) / 3600)
                > nt.MAX_TRAVEL_KMH
                for b in agent_events if b is not a
            ), f"geo_impossibility event {a['event_id']} has no impossible pair"


# ---------------------------------------------------------------------------
# GA4 — PII-free outputs
# ---------------------------------------------------------------------------
@pytest.fixture(scope="module")
def dataset_dir(tmp_path_factory: pytest.TempPathFactory) -> Path:
    out = tmp_path_factory.mktemp("naija-pii")
    nt.generate_dataset(seed=99, days=30, persons=80, out_dir=out)
    return out


def test_no_plaintext_phones_or_bvn(dataset_dir: Path):
    phone_re = re.compile(rb"\+?234\d{10}|\b0[789]\d{9}\b")
    bvn_re = re.compile(rb"\b22\d{9}\b")  # BVN = 11 digits starting 22 (CBN format)
    for name in OUTPUT_NAMES:
        blob = (dataset_dir / name).read_bytes()
        assert b"+234" not in blob, f"raw +234 phone leaked into {name}"
        assert not phone_re.search(blob), f"phone-like number in {name}"
        assert not bvn_re.search(blob), f"BVN-like number in {name}"


def test_no_plaintext_names(dataset_dir: Path):
    names = {n.lower() for n in nt.FIRST_NAMES} | {n.lower() for n in nt.LAST_NAMES}
    for name in ("events.jsonl", "persons.jsonl", "graph_edges.jsonl", "labels.json"):
        tokens = set(re.findall(r"[A-Za-z]{4,}", (dataset_dir / name).read_text(encoding="utf-8").lower()))
        leaked = tokens & names
        assert not leaked, f"plaintext names leaked into {name}: {sorted(leaked)[:5]}"


def test_persons_keyed_by_opaque_id_hashed_pii(dataset_dir: Path):
    for p in _read_jsonl(dataset_dir / "persons.jsonl"):
        assert p["person_id"].startswith("per-")
        assert "name" not in p and "phone" not in p and "bvn" not in p
        assert p["phone_hash"].startswith("sha256:")
        assert p["name_hash"].startswith("sha256:")
        # W28-style: deterministic SHA-256(SEED_SALT | value), 64 hex chars.
        assert len(p["phone_hash"]) == len("sha256:") + 64
