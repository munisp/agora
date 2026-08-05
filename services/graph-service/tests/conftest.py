"""Shared fixtures: seeded in-memory graph (two tenants), stub LLM, TestClient.

Graph fixture (SPEC-W28 §3) — tenant-a:
  pa1 Ada   : marketing consent (valid), contact->Alimosho, old booking
              (2025-01-10 -> offering o1), messaged 40d ago, REFERRED->pa2
  pa2 Bola  : NO consent, old booking, contact->Alimosho
  pa3 Chidi : marketing consent REVOKED
  pa4 Dara  : marketing consent (valid) but quarantine=true
  pa5 Efe   : marketing consent (valid), recent booking (5d ago)
  pa6 Femi  : marketing consent (valid), old booking (2025-02-01), messaged 2d ago,
              NO Contact (audience member with lead_id=null)
  pa7 Goke  : consent purpose "service" only
tenant-b:
  pb1       : marketing consent (valid), contact->Alimosho, old booking

=> segment {purpose: marketing} matches pa1,pa5,pa6 (3) in tenant-a.
=> segment {marketing, lga=Alimosho, last_booking_before=2025-06-01,
   not_messaged_since_days=30} matches ONLY pa1.
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest
from fastapi.testclient import TestClient

from app.ask import AskUnavailable
from app.backend import InMemoryBackend, InMemoryGraph
from app.config import Settings
from app.main import create_app
from app.store import SegmentStore

TENANT_A = "tenant-a"
TENANT_B = "tenant-b"
HDR_A = {"X-Tenant-Id": TENANT_A}
HDR_B = {"X-Tenant-Id": TENANT_B}


def iso(dt: datetime) -> str:
    return dt.isoformat()


def build_graph() -> InMemoryGraph:
    now = datetime.now(timezone.utc)
    g = InMemoryGraph()

    def person(pid, tenant, name, quarantine=False):
        return g.add_node(
            f"{tenant}:{pid}",
            {"Person"},
            person_id=pid,
            tenant_id=tenant,
            name=name,
            phone_hash=f"hash-{pid}",
            channels=["whatsapp"],
            consent_summary="",
            quarantine=quarantine,
            updated_at=iso(now),
        )

    def consent(cid, tenant, purpose, revoked=None):
        return g.add_node(
            f"{tenant}:{cid}",
            {"Consent"},
            consent_id=cid,
            tenant_id=tenant,
            purpose=purpose,
            granted_at=iso(now - timedelta(days=90)),
            revoked_at=revoked,
        )

    def grant(p, c, purpose):
        g.add_edge(p.node_id, c.node_id, "CONSENTED", purpose=purpose, at=iso(now - timedelta(days=90)))

    def contact(lead_id, tenant, p, loc=None):
        ct = g.add_node(
            f"{tenant}:{lead_id}",
            {"Contact"},
            lead_id=lead_id,
            tenant_id=tenant,
            channel_of_first_touch="field_pwa",
            source="field",
            captured_at=iso(now - timedelta(days=120)),
        )
        g.add_edge(p.node_id, ct.node_id, "HAS_CONTACT")
        if loc is not None:
            g.add_edge(ct.node_id, loc.node_id, "CAPTURED_AT")
        return ct

    def location(lid, tenant, lga):
        return g.add_node(f"{tenant}:{lid}", {"Location"}, tenant_id=tenant, lga=lga, ward="w1")

    def booking(bid, tenant, p, created_at, offering=None):
        b = g.add_node(
            f"{tenant}:{bid}",
            {"Booking"},
            booking_id=bid,
            tenant_id=tenant,
            status="completed",
            created_at=iso(created_at),
            showed=True,
        )
        g.add_edge(p.node_id, b.node_id, "BOOKED", at=iso(created_at))
        if offering is not None:
            g.add_edge(b.node_id, offering.node_id, "FOR")
        return b

    def message(p, camp, at, status="sent"):
        g.add_edge(
            p.node_id, camp.node_id, "MESSAGED",
            campaign_id=camp.props["campaign_id"], at=iso(at), status=status,
        )

    # ---- tenant A -----------------------------------------------------------
    a = TENANT_A
    pa1 = person("pa1", a, "Ada")
    pa2 = person("pa2", a, "Bola")
    pa3 = person("pa3", a, "Chidi")
    pa4 = person("pa4", a, "Dara", quarantine=True)
    pa5 = person("pa5", a, "Efe")
    pa6 = person("pa6", a, "Femi")
    pa7 = person("pa7", a, "Goke")

    grant(pa1, consent("c1", a, "marketing"), "marketing")
    grant(pa3, consent("c3", a, "marketing", revoked=iso(now - timedelta(days=10))), "marketing")
    grant(pa4, consent("c4", a, "marketing"), "marketing")
    grant(pa5, consent("c5", a, "marketing"), "marketing")
    grant(pa6, consent("c6", a, "marketing"), "marketing")
    grant(pa7, consent("c7", a, "service"), "service")

    loc_alimosho = location("loc1", a, "Alimosho")
    loc_ikeja = location("loc2", a, "Ikeja")
    contact("lead1", a, pa1, loc_alimosho)
    # Second, MORE RECENT contact for pa1: audience lead_id resolution must
    # pick the most recent Contact's lead_id ("lead1b").
    ct1b = contact("lead1b", a, pa1, loc_alimosho)
    ct1b.props["captured_at"] = iso(now - timedelta(days=10))
    contact("lead2", a, pa2, loc_alimosho)
    contact("lead5", a, pa5, loc_ikeja)
    # pa6 intentionally has NO Contact -> audience member lead_id is null.

    o1 = g.add_node(f"{a}:o1", {"Offering"}, offering_id="o1", tenant_id=a, name="Haircut")
    booking("b1", a, pa1, datetime(2025, 1, 10, tzinfo=timezone.utc), o1)
    booking("b2", a, pa2, datetime(2025, 1, 15, tzinfo=timezone.utc), o1)
    booking("b5", a, pa5, now - timedelta(days=5), o1)
    booking("b6", a, pa6, datetime(2025, 2, 1, tzinfo=timezone.utc), o1)

    camp1 = g.add_node(f"{a}:camp1", {"Campaign"}, campaign_id="camp1", tenant_id=a, kind="outreach")
    camp2 = g.add_node(f"{a}:camp2", {"Campaign"}, campaign_id="camp2", tenant_id=a, kind="promo")
    message(pa1, camp1, now - timedelta(days=40))
    message(pa6, camp2, now - timedelta(days=2))

    g.add_edge(pa1.node_id, pa2.node_id, "REFERRED", at=iso(now - timedelta(days=30)), program="ref-2026")

    g.add_node(f"{a}:tenant", {"Tenant"}, tenant_id=a, slug="alpha")

    # ---- tenant B -----------------------------------------------------------
    b = TENANT_B
    pb1 = person("pb1", b, "B-person")
    grant(pb1, consent("cb1", b, "marketing"), "marketing")
    contact("leadb1", b, pb1, location("locb1", b, "Alimosho"))
    booking("bb1", b, pb1, datetime(2025, 3, 1, tzinfo=timezone.utc))
    g.add_node(f"{b}:tenant", {"Tenant"}, tenant_id=b, slug="beta")
    return g


class StubLLM:
    """Scriptable ask LLM: returns queued responses; AskUnavailable simulates
    Ollama being down."""

    def __init__(self, responses: list[object] | None = None) -> None:
        self.responses = list(responses or [])
        self.calls: list[list[dict[str, str]]] = []

    async def complete(self, messages: list[dict[str, str]]) -> str:
        self.calls.append(messages)
        if not self.responses:
            return '{"template": "consent_counts", "params": {}}'
        item = self.responses.pop(0)
        if isinstance(item, Exception):
            raise item
        return str(item)


@pytest.fixture()
def settings(tmp_path):
    return Settings(
        graph_backend="memory",
        segment_store_dir=str(tmp_path / "store"),
        jwt_public_key="",
    )


@pytest.fixture()
def make_client(settings):
    def _make(llm: StubLLM | None = None, graph: InMemoryGraph | None = None, st=None):
        app = create_app(
            st or settings,
            backend=InMemoryBackend(graph or build_graph()),
            llm=llm or StubLLM(),
            store=SegmentStore((st or settings).segment_store_dir + "-seg"),
        )
        return TestClient(app)

    return _make


@pytest.fixture()
def client(make_client):
    return make_client()


MARKETING_SEGMENT = {"name": "Marketing consented", "purpose": "marketing", "filter": {}}

LAPSED_SEGMENT = {
    "name": "Lapsed Alimosho",
    "purpose": "marketing",
    "filter": {
        "lga": "Alimosho",
        "last_booking_before": "2025-06-01",
        "not_messaged_since_days": 30,
    },
}
