"""gnn_data tests (SPEC-W31 §1): tensor shapes/dtypes, deterministic node
indexing, edge construction, name_embedding projection. Requires torch."""

from __future__ import annotations

import pytest

torch = pytest.importorskip("torch")
pytest.importorskip("torch_geometric")

pytestmark = pytest.mark.requires_torch

from datetime import datetime, timezone

from graph_ml.extract import BookingRec, OfferingRec, PersonRec, ReferralRec, TenantGraph
from graph_ml.gnn_data import (
    FEATURE_DIM,
    NAME_EMBED_DIM,
    PERSON_VECTOR_DIM,
    TYPE_DIM,
    build_graph_data,
    graph_stats,
)

NOW = datetime(2026, 8, 5, 12, 0, 0, tzinfo=timezone.utc)


def small_graph() -> TenantGraph:
    return TenantGraph(
        tenant_id="t1",
        persons=[
            PersonRec(person_id="p2", name="B"),
            PersonRec(person_id="p1", name="A", name_embedding=tuple([0.5] * 16)),
            PersonRec(person_id="p3", name="C"),
        ],
        offerings=[
            OfferingRec(offering_id="o2", name="Laundry", price_cents=3000),
            OfferingRec(offering_id="o1", name="Cleaning", price_cents=5000),
        ],
        bookings=[
            BookingRec("p1", "b1", "o1", at="2026-08-01T00:00:00+00:00", price_cents=5000),
            BookingRec("p2", "b2", "o1", at="2026-08-02T00:00:00+00:00", price_cents=5000),
            BookingRec("p2", "b3", "o2", at="2026-08-03T00:00:00+00:00", price_cents=3000),
            BookingRec("p2", "b4", "o2", at="2026-08-04T00:00:00+00:00", price_cents=3000),
        ],
        referrals=[ReferralRec(from_person_id="p1", to_person_id="p3", at=None)],
    )


def test_x_shape_and_dtype():
    data = build_graph_data(small_graph(), NOW)
    assert data.x.shape == (5, FEATURE_DIM)
    assert data.x.dtype == torch.float32
    assert FEATURE_DIM == TYPE_DIM + PERSON_VECTOR_DIM + NAME_EMBED_DIM
    assert data.edge_index.dtype == torch.long
    assert data.edge_index.shape[0] == 2


def test_deterministic_node_indexing_sorted_ids():
    data = build_graph_data(small_graph(), NOW)
    assert data.person_ids == ("p1", "p2", "p3")
    assert data.offering_ids == ("o1", "o2")
    # offerings are offset after persons in the homogeneous node space
    booked = {tuple(pair) for pair in data.booked_edge_index.t().tolist()}
    assert booked == {(0, 3), (1, 3), (1, 4)}


def test_deterministic_regardless_of_input_order():
    graph = small_graph()
    data_a = build_graph_data(graph, NOW)
    graph.persons.reverse()
    graph.offerings.reverse()
    graph.bookings.reverse()
    data_b = build_graph_data(graph, NOW)
    assert data_a.person_ids == data_b.person_ids
    assert data_a.offering_ids == data_b.offering_ids
    assert torch.equal(data_a.x, data_b.x)
    assert torch.equal(data_a.edge_index, data_b.edge_index)


def test_node_type_onehot_rows():
    data = build_graph_data(small_graph(), NOW)
    assert data.x[:3, 0].tolist() == [1.0, 1.0, 1.0]  # persons: is_person
    assert data.x[:3, 1].tolist() == [0.0, 0.0, 0.0]
    assert data.x[3:, 0].tolist() == [0.0, 0.0]
    assert data.x[3:, 1].tolist() == [1.0, 1.0]  # offerings: is_offering


def test_offering_price_scalar_log_scaled():
    import math

    data = build_graph_data(small_graph(), NOW)
    assert data.x[3, TYPE_DIM].item() == pytest.approx(math.log1p(5000))
    assert data.x[4, TYPE_DIM].item() == pytest.approx(math.log1p(3000))


def test_name_embedding_projected_and_normalized():
    data = build_graph_data(small_graph(), NOW)
    emb_slice = data.x[0, TYPE_DIM + PERSON_VECTOR_DIM :]  # p1 has an embedding
    assert emb_slice.shape == (NAME_EMBED_DIM,)
    assert torch.linalg.norm(emb_slice).item() == pytest.approx(1.0)
    # p2/p3 have no embedding -> zero padding
    assert data.x[1, TYPE_DIM + PERSON_VECTOR_DIM :].abs().sum().item() == 0.0


def test_edge_index_is_bidirectional():
    data = build_graph_data(small_graph(), NOW)
    pairs = {tuple(pair) for pair in data.edge_index.t().tolist()}
    for src, dst in list(pairs):
        assert (dst, src) in pairs
    # 3 booked pairs + 1 referral pair = 4 unique, x2 directions
    assert len(pairs) == 8


def test_person_person_edges_from_referrals_only():
    data = build_graph_data(small_graph(), NOW)
    assert data.person_edge_index.t().tolist() == [[0, 2]]  # p1 -> p3


def test_graph_stats_counts_unique_pairs():
    persons, edges = graph_stats(small_graph())
    assert persons == 3
    assert edges == 3 + 1  # 3 unique booked pairs + 1 referral
