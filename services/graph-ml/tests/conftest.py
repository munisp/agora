"""Shared fixtures: a deterministic 5-person tenant graph (SPEC-W29 §4 gate 4
cold-start fixture) with known hand-computable score/recommendation math.

Layout (all timestamps relative to NOW, ISO-8601 strings):
  offerings: o1 "Cleaning" 5000c, o2 "Laundry" 3000c, o3 "Deep Clean" 9000c
  p1: booked o1@-40d, o1@-25d, o2@-10d  (interval mean 15d, recency 10d)
      messages: responded@-30d, delivered@-8d  (response 1/2, converted 1/2)
      referrals: p1->p5 (out 1), p2->p1 (in 1)
  p2: booked o1@-10d, o3@-5d            (interval mean 5d, recency 5d)
  p3: booked o2@-20d                    (recency 20d)
  p4: no bookings; messages responded@-5d, replied@-3d (response 1.0);
      consents marketing+reminders; contact captured -60d
  p5: quarantined, no bookings, never messaged (full cold start)
Co-occurrence: bookers o1:2 o2:2 o3:1; cooc o1->o3 = 1  => P(o3|o1) = 0.5.
Tenant median interval = median(15, 5) = 10 days.
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import importlib.util

import pytest

from graph_ml.extract import (
    BookingRec,
    ConsentRec,
    ContactRec,
    MessageRec,
    OfferingRec,
    PersonRec,
    ReferralRec,
    TenantGraph,
)

NOW = datetime(2026, 8, 5, 12, 0, 0, tzinfo=timezone.utc)


def iso(days_ago: float) -> str:
    return (NOW - timedelta(days=days_ago)).isoformat()


@pytest.fixture()
def now() -> datetime:
    return NOW


@pytest.fixture()
def tenant_graph() -> TenantGraph:
    return TenantGraph(
        tenant_id="t1",
        persons=[
            PersonRec(person_id="p1", name="Ada", channels=("sms",)),
            PersonRec(person_id="p2", name="Bola", channels=("sms",)),
            PersonRec(person_id="p3", name="Chidi", channels=("whatsapp",)),
            PersonRec(person_id="p4", name="Dara", channels=("sms", "email")),
            PersonRec(person_id="p5", name="Efe", quarantine=True),
        ],
        offerings=[
            OfferingRec(offering_id="o1", name="Cleaning", price_cents=5000),
            OfferingRec(offering_id="o2", name="Laundry", price_cents=3000),
            OfferingRec(offering_id="o3", name="Deep Clean", price_cents=9000),
        ],
        bookings=[
            BookingRec("p1", "b1", "o1", "Cleaning", at=iso(40), price_cents=5000),
            BookingRec("p1", "b2", "o1", "Cleaning", at=iso(25), price_cents=5000),
            BookingRec("p1", "b3", "o2", "Laundry", at=iso(10), price_cents=3000),
            BookingRec("p2", "b4", "o1", "Cleaning", at=iso(10), price_cents=5000),
            BookingRec("p2", "b5", "o3", "Deep Clean", at=iso(5), price_cents=9000),
            BookingRec("p3", "b6", "o2", "Laundry", at=iso(20), price_cents=3000),
        ],
        referrals=[
            ReferralRec(from_person_id="p1", to_person_id="p5", at=iso(15), program="ref"),
            ReferralRec(from_person_id="p2", to_person_id="p1", at=iso(12), program="ref"),
        ],
        messages=[
            MessageRec("p1", "c1", at=iso(30), status="responded"),
            MessageRec("p1", "c1", at=iso(8), status="delivered"),
            MessageRec("p4", "c2", at=iso(5), status="responded"),
            MessageRec("p4", "c2", at=iso(3), status="replied"),
        ],
        consents=[
            ConsentRec("p4", "marketing", at=iso(50)),
            ConsentRec("p4", "reminders", at=iso(50)),
            ConsentRec("p1", "marketing", at=iso(45)),
        ],
        contacts=[
            ContactRec("p4", "lead4", captured_at=iso(60), channel="web", source="pwa"),
            ContactRec("p1", "lead1", captured_at=iso(90), channel="field", source="agent"),
        ],
    )


# ---------------------------------------------------------------------------
# torch marker plumbing (SPEC-W31 §0 invariant 5 / §3 G5)
# ---------------------------------------------------------------------------

#: Tests that assume torch/torch-geometric are ABSENT (W29 degraded-path
#: semantics). They cannot pass when the GNN overlay is installed, so they
#: skip on torch boxes; the requires_torch suite covers the same gates with
#: torch present.
TORCH_ABSENT_TESTS = frozenset(
    {
        "test_resolve_backend_gnn_falls_back_with_warning",
        "test_graphsage_backend_unavailable_raises",
        "test_gnn_backend_requested_degrades_to_heuristic",
        # asserts /healthz reports gnn_available is False (W29 heuristic image)
        "test_healthz",
        # asserts backend=gnn request degrades to heuristic in /healthz
        "test_healthz_reports_gnn_fallback_backend",
    }
)


def _torch_stack_present() -> bool:
    return (
        importlib.util.find_spec("torch") is not None
        and importlib.util.find_spec("torch_geometric") is not None
    )


def pytest_configure(config):
    config.addinivalue_line(
        "markers",
        "requires_torch: test needs torch + torch-geometric; skips cleanly "
        "when the optional GNN stack is absent (SPEC-W31 §0 invariant 5)",
    )


def pytest_collection_modifyitems(config, items):
    torch_present = _torch_stack_present()
    skip_no_torch = pytest.mark.skip(
        reason="torch/torch-geometric not installed (heuristic-only deployment)"
    )
    skip_torch_present = pytest.mark.skip(
        reason="assumes torch is absent; GNN overlay is installed here"
    )
    for item in items:
        if "requires_torch" in item.keywords and not torch_present:
            item.add_marker(skip_no_torch)
        if torch_present and item.name in TORCH_ABSENT_TESTS:
            item.add_marker(skip_torch_present)
