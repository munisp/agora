"""gnn_train tests (SPEC-W31 §1): min-size gate, convergence, versioned
artifacts, meta.json, per-tenant isolation, load_latest, determinism.
Requires torch (skipped cleanly on heuristic-only deployments)."""

from __future__ import annotations

import pytest

torch = pytest.importorskip("torch")
pytest.importorskip("torch_geometric")

pytestmark = pytest.mark.requires_torch

import json
import os
from datetime import datetime, timezone

from graph_ml.config import Settings
from graph_ml.extract import BookingRec, OfferingRec, PersonRec, ReferralRec, TenantGraph
from graph_ml.gnn import GNNInsufficientData
from graph_ml.gnn_data import build_graph_data
from graph_ml.gnn_train import SAGEConfig, load_latest, train_model, train_tenant

NOW = datetime(2026, 8, 5, 12, 0, 0, tzinfo=timezone.utc)
NUM_PERSONS = 30
NUM_OFFERINGS = 8


def make_toy_graph(tenant_id: str = "t1") -> TenantGraph:
    """30 persons x 8 offerings; 60 booked pairs + 30 referrals (>= gates)."""
    persons = [
        PersonRec(person_id=f"p{i:02d}", name=f"Person {i}") for i in range(NUM_PERSONS)
    ]
    offerings = [
        OfferingRec(offering_id=f"o{j}", name=f"Offering {j}", price_cents=1000 * (j + 1))
        for j in range(NUM_OFFERINGS)
    ]
    bookings = [
        BookingRec(
            f"p{i:02d}",
            f"b{i}a",
            f"o{i % NUM_OFFERINGS}",
            at="2026-07-20T00:00:00+00:00",
            price_cents=1000 * (i % NUM_OFFERINGS + 1),
        )
        for i in range(NUM_PERSONS)
    ] + [
        BookingRec(
            f"p{i:02d}",
            f"b{i}b",
            f"o{(i // 4 + 1) % NUM_OFFERINGS}",
            at="2026-07-28T00:00:00+00:00",
            price_cents=1000,
        )
        for i in range(NUM_PERSONS)
    ]
    referrals = [
        ReferralRec(from_person_id=f"p{i:02d}", to_person_id=f"p{(i + 1) % NUM_PERSONS:02d}")
        for i in range(NUM_PERSONS)
    ]
    return TenantGraph(
        tenant_id=tenant_id,
        persons=persons,
        offerings=offerings,
        bookings=bookings,
        referrals=referrals,
    )


def train_settings(tmp_path, **overrides) -> Settings:
    base = dict(
        model_dir=str(tmp_path),
        device="cpu",
        seed=42,
        gnn_epochs=60,
        gnn_hidden_dim=32,
        gnn_min_persons=20,
        gnn_min_edges=30,
    )
    base.update(overrides)
    return Settings(**base)


def test_min_size_gate_raises_for_too_few_persons(tmp_path):
    graph = make_toy_graph()
    graph.persons = graph.persons[:10]
    with pytest.raises(GNNInsufficientData, match="too small"):
        train_tenant(graph, "t1", train_settings(tmp_path))


def test_min_size_gate_raises_for_too_few_edges(tmp_path):
    graph = make_toy_graph()
    graph.bookings = graph.bookings[:10]
    graph.referrals = []
    with pytest.raises(GNNInsufficientData):
        train_tenant(graph, "t1", train_settings(tmp_path))


def test_gate_writes_no_artifacts(tmp_path):
    graph = make_toy_graph()
    graph.persons = graph.persons[:5]
    with pytest.raises(GNNInsufficientData):
        train_tenant(graph, "t1", train_settings(tmp_path))
    assert not os.path.exists(os.path.join(str(tmp_path), "t1"))


def test_training_converges_on_toy_graph():
    data = build_graph_data(make_toy_graph(), NOW)
    config = SAGEConfig(hidden_dim=32, epochs=60, seed=42, device="cpu")
    _model, losses = train_model(data, config)
    assert len(losses) == 60
    assert losses[-1] < losses[0]


def test_artifacts_and_meta_json_fields(tmp_path):
    result = train_tenant(make_toy_graph(), "t1", train_settings(tmp_path))
    assert result.model_version == "graphsage-v1"
    artifact_dir = os.path.join(str(tmp_path), "t1", "graphsage-v1")
    assert result.model_dir == artifact_dir
    assert os.path.isfile(os.path.join(artifact_dir, "model.pt"))
    with open(os.path.join(artifact_dir, "meta.json"), encoding="utf-8") as fh:
        meta = json.load(fh)
    assert meta["tenant_id"] == "t1"
    assert meta["model_version"] == "graphsage-v1"
    assert meta["hidden_dim"] == 32
    assert meta["epochs"] == 60
    assert meta["seed"] == 42
    assert meta["device"] == "cpu"
    assert meta["feature_dim"] > 0
    assert meta["node_counts"] == {"persons": NUM_PERSONS, "offerings": NUM_OFFERINGS}
    # 60 booking rows collapse to 56 unique person-offering pairs
    assert meta["edge_counts"] == {"booked": 56, "person_person": 30}
    assert meta["final_loss"] == result.final_loss
    assert meta["trained_at"] == result.trained_at


def test_version_increments_per_tenant(tmp_path):
    settings = train_settings(tmp_path)
    r1 = train_tenant(make_toy_graph(), "t1", settings)
    r2 = train_tenant(make_toy_graph(), "t1", settings)
    assert (r1.model_version, r2.model_version) == ("graphsage-v1", "graphsage-v2")


def test_per_tenant_isolation(tmp_path):
    settings = train_settings(tmp_path)
    train_tenant(make_toy_graph("t1"), "t1", settings)
    assert load_latest(str(tmp_path), "t2") is None  # t2 never sees t1's model
    train_tenant(make_toy_graph("t2"), "t2", settings)
    _sd, meta1, _fd = load_latest(str(tmp_path), "t1")
    _sd2, meta2, _fd2 = load_latest(str(tmp_path), "t2")
    assert meta1["tenant_id"] == "t1"
    assert meta2["tenant_id"] == "t2"
    assert meta1["model_version"] == meta2["model_version"] == "graphsage-v1"


def test_load_latest_none_without_artifacts(tmp_path):
    assert load_latest(str(tmp_path), "ghost") is None
    (tmp_path / "t1").mkdir()  # tenant dir but no versioned artifact
    assert load_latest(str(tmp_path), "t1") is None


def test_load_latest_round_trip_and_picks_highest_version(tmp_path):
    settings = train_settings(tmp_path)
    train_tenant(make_toy_graph(), "t1", settings)
    train_tenant(make_toy_graph(), "t1", settings)
    state_dict, meta, feature_dim = load_latest(str(tmp_path), "t1")
    assert meta["model_version"] == "graphsage-v2"
    assert feature_dim == meta["feature_dim"]
    assert any(key.endswith("weight") for key in state_dict)


def test_determinism_same_seed_identical_final_loss():
    data = build_graph_data(make_toy_graph(), NOW)
    config = SAGEConfig(hidden_dim=32, epochs=25, seed=42, device="cpu")
    _m1, losses1 = train_model(data, config)
    _m2, losses2 = train_model(data, config)
    assert losses1 == losses2  # exact equality on CPU (SPEC-W31 G4)


def test_determinism_different_seed_diverges():
    data = build_graph_data(make_toy_graph(), NOW)
    _m1, losses1 = train_model(data, SAGEConfig(hidden_dim=32, epochs=25, seed=42, device="cpu"))
    _m2, losses2 = train_model(data, SAGEConfig(hidden_dim=32, epochs=25, seed=7, device="cpu"))
    assert losses1 != losses2


def test_train_result_device_cpu(tmp_path):
    result = train_tenant(make_toy_graph(), "t1", train_settings(tmp_path))
    assert result.device == "cpu"
    assert result.epochs == 60
