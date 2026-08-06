"""gnn_labels tests (SPEC-W33 §3 B3): labels.json join correctness —
including the CRITICAL A1 quirk that fraud-injected persons carry
``fraud: false`` on their persons.jsonl row so labels.json is the ONLY
ground truth — hard-negative handling, masking of other fraud scenarios,
deterministic node ordering, and edge-shape mapping.

Pure stdlib/numpy (no torch): these run in BOTH deployments."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from graph_ml.gnn_labels import (
    POSITIVE_SCENARIOS,
    dataset_fingerprint,
    load_labeled_graph,
)

# A known fraud-injected person whose person row LIES (the A1 quirk):
# persons.jsonl says fraud:false, labels.json says referral_ring fraud.
FRAUD_PERSON = "per-fraud00001"  # referral_ring member, row says fraud:false
SYBIL_PERSON = "per-sybil0001"  # sybil_cluster, row says fraud:true
BENIGN_PERSON = "per-benign001"  # benign_family_referral hard negative
GHOST_PERSON = "per-ghost00001"  # ghost_booking -> masked out
STRUCT_PERSON = "per-struct0001"  # structuring -> masked out
PLAIN_PERSON = "per-plain00001"  # no label at all -> negative
POS_AND_OTHER = "per-both000001"  # referral_ring + structuring -> positive wins

PERSON_IDS = [
    BENIGN_PERSON,
    FRAUD_PERSON,
    GHOST_PERSON,
    PLAIN_PERSON,
    POS_AND_OTHER,
    STRUCT_PERSON,
    SYBIL_PERSON,
]


def _person_row(pid: str, fraud: bool, scenario: str | None) -> dict:
    return {
        "person_id": pid,
        "persona": "market_trader",
        "name_hash": f"sha256:{pid}",
        "phone_hash": "sha256:x",
        "lga": "Ikeja",
        "state": "Lagos",
        "zone": "South West",
        "home_lat": 6.6,
        "home_lon": 3.3,
        "is_synthetic": True,
        "fraud": fraud,
        "scenario": scenario,
    }


def _write_dataset(root: Path) -> Path:
    d = root / "naija_txn" / "42"
    d.mkdir(parents=True)
    persons = [
        _person_row(FRAUD_PERSON, False, None),  # <-- THE QUIRK: row lies
        _person_row(SYBIL_PERSON, True, "sybil_cluster"),
        _person_row(BENIGN_PERSON, False, "benign_family_referral"),
        _person_row(GHOST_PERSON, False, None),  # quirk again (ghost staff)
        _person_row(STRUCT_PERSON, False, None),
        _person_row(PLAIN_PERSON, False, None),
        _person_row(POS_AND_OTHER, False, None),
    ]
    # Deliberately UNSORTED + padded with junk ids to prove ordering is by id.
    (d / "persons.jsonl").write_text(
        "".join(json.dumps(p, sort_keys=True) + "\n" for p in persons),
        encoding="utf-8",
    )
    edges = [
        {
            "edge_id": "edg-1",
            "edge_type": "REFERRED",
            "at": "2026-02-01T10:00:00Z",
            "from_person_id": FRAUD_PERSON,
            "to_person_id": PLAIN_PERSON,
            "program": "reward-referral",
            "fraud": True,
            "scenario": "referral_ring",
        },
        {
            "edge_id": "edg-2",
            "edge_type": "BOOKED",
            "at": "2026-02-01T10:05:00Z",
            "person_id": FRAUD_PERSON,
            "booking_id": "boo-1",
            "offering_id": "off-1",
            "status": "completed",
            "showed": True,
            "fraud": True,
            "scenario": "referral_ring",
        },
        {
            "edge_id": "edg-3",
            "edge_type": "BOOKED",
            "at": "2026-02-02T11:00:00Z",
            "person_id": PLAIN_PERSON,
            "booking_id": "boo-2",
            "offering_id": "off-2",
            "status": "completed",
            "showed": True,
            "fraud": False,
            "scenario": None,
        },
        {
            "edge_id": "edg-4",
            "edge_type": "CAPTURED",
            "at": "2026-02-03T08:00:00Z",
            "agent_id": SYBIL_PERSON,
            "contact_id": "con-1",
            "captured_at": "2026-02-03T08:00:00Z",
            "channel": "field-capture",
            "lat": 6.6,
            "lon": 3.3,
            "fraud": True,
            "scenario": "sybil_cluster",
        },
        {"edge_id": "edg-5", "edge_type": "FUTURE_TYPE", "at": "2026-02-03T09:00:00Z"},
    ]
    (d / "graph_edges.jsonl").write_text(
        "".join(json.dumps(e, sort_keys=True) + "\n" for e in edges),
        encoding="utf-8",
    )
    events = [
        {
            "event_id": "evt-1",
            "ts": "2026-02-01T10:05:00Z",
            "event_type": "booking",
            "person_id": FRAUD_PERSON,
            "amount_ngn": 1500.25,
            "counterparty": "off-1",
            "reference_id": "boo-1",
            "fraud": True,
            "scenario": "referral_ring",
        },
        {
            "event_id": "evt-2",
            "ts": "2026-02-02T11:00:00Z",
            "event_type": "booking",
            "person_id": PLAIN_PERSON,
            "amount_ngn": 3200.0,
            "counterparty": "off-2",
            "reference_id": "boo-2",
            "fraud": False,
            "scenario": None,
        },
    ]
    (d / "events.jsonl").write_text(
        "".join(json.dumps(e, sort_keys=True) + "\n" for e in events),
        encoding="utf-8",
    )
    labels_doc = {
        "seed": 42,
        "entries": [
            {"entity_id": FRAUD_PERSON, "scenario": "referral_ring",
             "fraud": True, "injected_at": "2026-02-01T10:00:00Z"},
            {"entity_id": SYBIL_PERSON, "scenario": "sybil_cluster",
             "fraud": True, "injected_at": "2026-02-01T09:00:00Z"},
            {"entity_id": BENIGN_PERSON, "scenario": "benign_family_referral",
             "fraud": False, "injected_at": "2026-02-04T09:00:00Z"},
            {"entity_id": GHOST_PERSON, "scenario": "ghost_booking",
             "fraud": True, "injected_at": "2026-02-05T09:00:00Z"},
            {"entity_id": STRUCT_PERSON, "scenario": "structuring",
             "fraud": True, "injected_at": "2026-02-06T09:00:00Z"},
            {"entity_id": POS_AND_OTHER, "scenario": "referral_ring",
             "fraud": True, "injected_at": "2026-02-07T09:00:00Z"},
            {"entity_id": POS_AND_OTHER, "scenario": "structuring",
             "fraud": True, "injected_at": "2026-02-07T10:00:00Z"},
            # non-person entities (events/edges) must be ignored by the join
            {"entity_id": "edg-1", "scenario": "referral_ring",
             "fraud": True, "injected_at": "2026-02-01T10:00:00Z"},
        ],
    }
    (d / "labels.json").write_text(json.dumps(labels_doc, indent=2) + "\n", encoding="utf-8")
    (d / "manifest.json").write_text(
        json.dumps({"seed": 42, "generated_at": "2026-01-01T00:00:00Z"}) + "\n",
        encoding="utf-8",
    )
    return d


@pytest.fixture()
def dataset_dir(tmp_path) -> Path:
    return _write_dataset(tmp_path)


def test_quirk_fraud_false_person_row_gets_label_1_only_via_labels_json(dataset_dir):
    """THE A1 QUIRK: FRAUD_PERSON's persons.jsonl row says fraud:false —
    only the labels.json join on entity_id may promote it to positive."""
    raw = {
        json.loads(line)["person_id"]: json.loads(line)
        for line in (dataset_dir / "persons.jsonl").read_text().splitlines()
    }
    assert raw[FRAUD_PERSON]["fraud"] is False  # the row lies (quirk setup)
    labeled = load_labeled_graph(dataset_dir)
    assert labeled.labels[FRAUD_PERSON] == 1  # ground truth = labels.json
    assert labeled.labels[SYBIL_PERSON] == 1
    assert POSITIVE_SCENARIOS == frozenset({"sybil_cluster", "referral_ring"})


def test_hard_negatives_and_unlabeled_are_zero(dataset_dir):
    labeled = load_labeled_graph(dataset_dir)
    assert labeled.labels[BENIGN_PERSON] == 0  # benign_* hard negative
    assert labeled.labels[PLAIN_PERSON] == 0  # unlabeled benign population


def test_other_fraud_scenarios_masked_out(dataset_dir):
    labeled = load_labeled_graph(dataset_dir)
    assert GHOST_PERSON not in labeled.labels
    assert STRUCT_PERSON not in labeled.labels
    assert set(labeled.masked_out) == {GHOST_PERSON, STRUCT_PERSON}
    # positive wins when a person carries both a positive and another
    # fraud-scenario label
    assert labeled.labels[POS_AND_OTHER] == 1
    assert POS_AND_OTHER not in labeled.masked_out


def test_label_counts(dataset_dir):
    labeled = load_labeled_graph(dataset_dir)
    assert labeled.num_positives == 3  # FRAUD + SYBIL + POS_AND_OTHER
    assert labeled.num_supervised == 5  # 7 persons - 2 masked out
    assert labeled.dataset_seed == 42
    assert labeled.dataset_generated_at == "2026-01-01T00:00:00Z"


def test_deterministic_node_ordering(dataset_dir):
    labeled = load_labeled_graph(dataset_dir)
    person_ids = [p.person_id for p in labeled.graph.persons]
    assert person_ids == sorted(person_ids)  # sorted regardless of file order
    offering_ids = [o.offering_id for o in labeled.graph.offerings]
    assert offering_ids == sorted(offering_ids)
    again = load_labeled_graph(dataset_dir)
    assert [p.person_id for p in again.graph.persons] == person_ids
    assert again.labels == labeled.labels
    assert again.masked_out == labeled.masked_out
    assert again.dataset_sha256 == labeled.dataset_sha256


def test_edge_shapes_map_to_tenant_graph(dataset_dir):
    graph = load_labeled_graph(dataset_dir).graph
    assert len(graph.referrals) == 1
    ref = graph.referrals[0]
    assert (ref.from_person_id, ref.to_person_id) == (FRAUD_PERSON, PLAIN_PERSON)
    assert ref.program == "reward-referral"
    assert len(graph.bookings) == 2
    by_id = {b.booking_id: b for b in graph.bookings}
    assert by_id["boo-1"].price_cents == 150025  # 1500.25 NGN joined from events
    assert by_id["boo-2"].price_cents == 320000
    assert by_id["boo-1"].status == "completed"
    assert len(graph.contacts) == 1
    contact = graph.contacts[0]
    assert contact.person_id == SYBIL_PERSON  # CAPTURED agent_id -> person_id
    assert contact.lead_id == "con-1"
    offerings = {o.offering_id: o for o in graph.offerings}
    assert offerings["off-1"].price_cents == 150025
    # unknown edge types are ignored, not fatal
    assert len(graph.referrals) + len(graph.bookings) + len(graph.contacts) == 4


def test_zero_price_fallback_without_events(dataset_dir):
    labeled = load_labeled_graph(dataset_dir, use_events=False)
    assert all(b.price_cents == 0 for b in labeled.graph.bookings)  # documented 0
    assert all(o.price_cents == 0 for o in labeled.graph.offerings)


def test_missing_required_file_raises(tmp_path):
    d = tmp_path / "empty"
    d.mkdir()
    with pytest.raises(FileNotFoundError, match="persons.jsonl"):
        load_labeled_graph(d)


def test_dataset_fingerprint_stable_and_sensitive(dataset_dir):
    fp1 = dataset_fingerprint(dataset_dir)
    fp2 = dataset_fingerprint(dataset_dir)
    assert fp1 == fp2 and len(fp1) == 64
    labels_path = dataset_dir / "labels.json"
    original = labels_path.read_bytes()
    try:
        labels_path.write_bytes(original + b" ")
        assert dataset_fingerprint(dataset_dir) != fp1
    finally:
        labels_path.write_bytes(original)
    assert dataset_fingerprint(dataset_dir) == fp1
