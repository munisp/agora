"""Shared fixtures: clean + tripped in-memory graph builders.

Timestamps are ISO-8601 UTC (what graph-sync writes). NOW is fixed so
lookback windows are deterministic.
"""

from __future__ import annotations

import sys
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from fraud_engine.config import Settings  # noqa: E402
from fraud_engine.events import InMemoryPublisher  # noqa: E402

from fakes import FakeGraphClient, PropertyGraph  # noqa: E402

NOW = datetime(2026, 8, 5, 12, 0, 0, tzinfo=UTC)
TENANT = "tenant-alpha"
OTHER_TENANT = "tenant-beta"


def ts(minutes_ago: float = 0, *, base: datetime = NOW) -> str:
    return (base - timedelta(minutes=minutes_ago)).isoformat()


@pytest.fixture()
def settings() -> Settings:
    return Settings()


@pytest.fixture()
def graph() -> PropertyGraph:
    return PropertyGraph()


@pytest.fixture()
def client(graph) -> FakeGraphClient:
    return FakeGraphClient(graph)


@pytest.fixture()
def publisher() -> InMemoryPublisher:
    return InMemoryPublisher()


def make_cycle(g: PropertyGraph, tenant: str, ids: list[str]) -> None:
    keys = {pid: g.add_person(tenant, pid) for pid in ids}
    for a, b in zip(ids, ids[1:] + ids[:1]):
        g.add_edge(keys[a], "REFERRED", keys[b], at=ts(120), program="referral-1")


def make_referral_chain(g: PropertyGraph, tenant: str, ids: list[str]) -> None:
    keys = {pid: g.add_person(tenant, pid) for pid in ids}
    for a, b in zip(ids, ids[1:]):
        g.add_edge(keys[a], "REFERRED", keys[b], at=ts(120), program="referral-1")


def add_booking_for(g: PropertyGraph, tenant: str, person_id: str, status: str = "confirmed") -> None:
    pkey = g.person_key(tenant, person_id)
    bkey = g.add_booking(tenant, f"bk-{person_id}", status=status, created_at=ts(200))
    g.add_edge(pkey, "BOOKED", bkey, at=ts(200))


def add_capture(
    g: PropertyGraph,
    tenant: str,
    person_id: str,
    lead_id: str,
    agent: str,
    captured_at: str,
    *,
    embedding: list[float] | None = None,
    lga: str | None = "Ikeja",
    lat: float | None = None,
    lon: float | None = None,
) -> None:
    pkey = g.add_person(tenant, person_id, name_embedding=embedding, name=person_id)
    ckey = g.add_contact(tenant, lead_id, captured_by=agent, captured_at=captured_at)
    g.add_edge(pkey, "HAS_CONTACT", ckey)
    if lga is not None or lat is not None:
        lkey = g.add_location(tenant, lga=lga, lat=lat, lon=lon)
        g.add_edge(ckey, "CAPTURED_AT", lkey)


def burst_captures(
    g: PropertyGraph,
    tenant: str,
    agent: str,
    count: int,
    start_minutes_ago: float,
    spacing_seconds: float = 30.0,
    lead_prefix: str = "lead",
) -> None:
    base = NOW - timedelta(minutes=start_minutes_ago)
    for i in range(count):
        at = (base + timedelta(seconds=i * spacing_seconds)).isoformat()
        ckey = g.add_contact(tenant, f"{lead_prefix}-{agent}-{i}", captured_by=agent, captured_at=at)
