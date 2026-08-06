"""TenantGraph -> PyG tensor conversion (SPEC-W31 §1 WS-A).

Imported lazily from ``gnn.py``/``gnn_train.py`` ONLY when torch is present —
the heuristic path never touches this module (SPEC-W31 §0 invariant 5).

Node layout (deterministic — SPEC-W31 §1: sorted ids, reproducible artifacts):
  * Person nodes first, sorted by ``person_id``
  * Offering nodes after, sorted by ``offering_id``

Node features (homogeneous ``feature_dim``):
  * 2-dim node-type one-hot ``[is_person, is_offering]``
  * Person: ``features.build_features(...).vector()`` (11 dims, log1p-scaled,
    all fields non-negative) + the stored Ollama ``name_embedding``
    projected/padded to ``NAME_EMBED_DIM`` dims (truncate/zero-pad).
  * Offering: ``log1p(price_cents)`` scalar + zero padding (no duration or
    type fields exist on ``extract.OfferingRec`` — padding keeps the fixed
    ``feature_dim``; documented honest zero).

Edges:
  * BOOKED (Person->Offering), unique pairs, both directions in ``edge_index``
  * Person->Person: REFERRED only. ``extract.TenantGraph`` exposes no other
    Person->Person relations (MESSAGED targets Campaigns, HAS_CONTACT targets
    Contacts), so referrals are the only real person-person edges.
"""

from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Any

import numpy as np

from .extract import TenantGraph
from .features import build_features

# dims of PersonFeatures.vector() (11) + projected name_embedding (8) + 2 type
NAME_EMBED_DIM = 8
TYPE_DIM = 2
PERSON_VECTOR_DIM = 11
FEATURE_DIM = TYPE_DIM + PERSON_VECTOR_DIM + NAME_EMBED_DIM


def graph_stats(graph: TenantGraph) -> tuple[int, int]:
    """(num_persons, num_edges) for the min-size gate.

    Edge count = unique Person->Offering BOOKED pairs + unique REFERRED pairs
    (the edges the GNN actually trains on).
    """
    booked = {(b.person_id, b.offering_id) for b in graph.bookings}
    referred = {(r.from_person_id, r.to_person_id) for r in graph.referrals}
    return len(graph.persons), len(booked) + len(referred)


@dataclass(frozen=True)
class TenantData:
    """Tensor payload for one tenant (torch tensors, CPU by default)."""

    x: Any  # torch.FloatTensor [num_nodes, FEATURE_DIM]
    edge_index: Any  # torch.LongTensor [2, E] (both directions)
    booked_edge_index: Any  # torch.LongTensor [2, B] person->offering positives
    person_edge_index: Any  # torch.LongTensor [2, R] person->person positives
    person_ids: tuple[str, ...]
    offering_ids: tuple[str, ...]
    feature_dim: int

    @property
    def num_persons(self) -> int:
        return len(self.person_ids)

    @property
    def num_offerings(self) -> int:
        return len(self.offering_ids)

    @property
    def num_nodes(self) -> int:
        return self.num_persons + self.num_offerings


def _person_row(person: Any, feature_vector: np.ndarray) -> np.ndarray:
    row = np.zeros(FEATURE_DIM, dtype=np.float32)
    row[0] = 1.0  # is_person
    vec = np.log1p(np.clip(np.asarray(feature_vector, dtype=np.float64), 0.0, None))
    row[TYPE_DIM : TYPE_DIM + PERSON_VECTOR_DIM] = vec[:PERSON_VECTOR_DIM]
    emb = np.asarray(getattr(person, "name_embedding", ()) or (), dtype=np.float64)
    if emb.size:
        n = min(NAME_EMBED_DIM, emb.size)
        norm = np.linalg.norm(emb[:n])
        row[TYPE_DIM + PERSON_VECTOR_DIM : TYPE_DIM + PERSON_VECTOR_DIM + n] = (
            emb[:n] / norm if norm > 0 else emb[:n]
        )
    return row


def _offering_row(offering: Any) -> np.ndarray:
    row = np.zeros(FEATURE_DIM, dtype=np.float32)
    row[1] = 1.0  # is_offering
    row[TYPE_DIM] = float(np.log1p(max(0, int(offering.price_cents or 0))))
    return row


def build_graph_data(graph: TenantGraph, now: datetime | None = None) -> TenantData:
    """Convert one TenantGraph to deterministic PyG-style tensors."""
    import torch  # lazy: this module is only used on the torch path

    person_ids = tuple(sorted(p.person_id for p in graph.persons))
    offering_ids = tuple(sorted(o.offering_id for o in graph.offerings))
    person_by_id = {p.person_id: p for p in graph.persons}
    offering_by_id = {o.offering_id: o for o in graph.offerings}
    node_of = {pid: i for i, pid in enumerate(person_ids)}
    node_of.update({oid: len(person_ids) + j for j, oid in enumerate(offering_ids)})

    features_by_person = {f.person_id: f for f in build_features(graph, now)}
    rows = [
        _person_row(person_by_id[pid], features_by_person[pid].vector())
        for pid in person_ids
    ]
    rows.extend(_offering_row(offering_by_id[oid]) for oid in offering_ids)
    x = (
        torch.from_numpy(np.stack(rows))
        if rows
        else torch.zeros((0, FEATURE_DIM), dtype=torch.float32)
    )

    booked_pairs = sorted(
        pair
        for pair in {(b.person_id, b.offering_id) for b in graph.bookings}
        if pair[0] in node_of and pair[1] in node_of
    )
    referral_pairs = sorted(
        pair
        for pair in {(r.from_person_id, r.to_person_id) for r in graph.referrals}
        if pair[0] in node_of and pair[1] in node_of
    )

    booked_idx = (
        torch.tensor(
            [[node_of[p], node_of[o]] for p, o in booked_pairs], dtype=torch.long
        ).t()
        if booked_pairs
        else torch.zeros((2, 0), dtype=torch.long)
    )
    person_idx = (
        torch.tensor(
            [[node_of[a], node_of[b]] for a, b in referral_pairs], dtype=torch.long
        ).t()
        if referral_pairs
        else torch.zeros((2, 0), dtype=torch.long)
    )

    undirected: list[tuple[int, int]] = []
    for src, dst in booked_pairs:
        undirected.append((node_of[src], node_of[dst]))
        undirected.append((node_of[dst], node_of[src]))
    for src, dst in referral_pairs:
        undirected.append((node_of[src], node_of[dst]))
        undirected.append((node_of[dst], node_of[src]))
    edge_index = (
        torch.tensor(undirected, dtype=torch.long).t()
        if undirected
        else torch.zeros((2, 0), dtype=torch.long)
    )

    return TenantData(
        x=x,
        edge_index=edge_index.contiguous(),
        booked_edge_index=booked_idx.contiguous(),
        person_edge_index=person_idx.contiguous(),
        person_ids=person_ids,
        offering_ids=offering_ids,
        feature_dim=FEATURE_DIM,
    )
