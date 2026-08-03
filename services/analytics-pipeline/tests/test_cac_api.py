"""GET /v1/cac/summary endpoint contract tests (SPEC-W13 §5): response
shape, X-Tenant-Slug resolution, validation errors, disabled-module 503 —
offline via FastAPI TestClient with fakes (no Postgres/Dapr/Kafka)."""

from __future__ import annotations

from datetime import date
from decimal import Decimal

from fastapi.testclient import TestClient

from analytics_pipeline.cac_summary import SpendUnavailable
from analytics_pipeline.config import load_settings
from analytics_pipeline.server import CacDeps, create_app
from analytics_pipeline.tenants import TenantResolver

T1 = "11111111-1111-1111-1111-111111111111"
CAMP_A = "33333333-3333-3333-3333-333333333333"


class FakeBronzeConsumer:
    running = True
    last_error = None

    async def lag_report(self):
        return {}


class FakeStore:
    async def fetch_channel_rollup(self, tenant_id, date_from, date_to):
        assert tenant_id == T1  # slug resolved to uuid before hitting the store
        return [{
            "channel": "whatsapp",
            "leads": 100,
            "conversions": 10,
            "revenue_ngn": Decimal("310000"),
        }]

    async def fetch_lga_rollup(self, tenant_id, date_from, date_to):
        return [{
            "lga_id": 42,
            "leads": 100,
            "conversions": 10,
            "revenue_ngn": Decimal("310000"),
        }]

    async def list_campaign_channels(self, tenant_id):
        return [{"campaign_id": CAMP_A, "channel": "whatsapp"}]

    async def rollup_day_bounds(self, tenant_id):
        return (date(2026, 1, 1), date(2026, 1, 31))


class FakeSpend:
    async def spend_sum(self, campaign_id, date_from, date_to):
        return Decimal("50000")


class FailingSpend:
    async def spend_sum(self, campaign_id, date_from, date_to):
        raise SpendUnavailable("404")


class FakeDapr:
    """identity-service tenant resolution only (spend uses FakeSpend)."""

    def __init__(self, tenants):
        self._tenants = tenants

    async def invoke(self, app_id, method, **kwargs):
        slug = method.rsplit("/", 1)[-1]
        if slug not in self._tenants:
            from analytics_pipeline.dapr_client import DaprError

            raise DaprError(f"invoke identity/{method}: 404", status_code=404)
        return self._tenants[slug]


def _client(spend=None, tenants=None, with_cac=True):
    settings = load_settings()
    cac = None
    if with_cac:
        dapr = FakeDapr(tenants or {"acme-salon": {"id": T1, "name": "Acme"}})
        cac = CacDeps(
            store=FakeStore(),
            spend=spend or FakeSpend(),
            tenants=TenantResolver(dapr, "identity", ttl_seconds=60),
        )
    app = create_app(FakeBronzeConsumer(), {"ready": True}, settings, cac)
    return TestClient(app)


def test_summary_contract_shape_via_tenant_slug_header():
    client = _client()
    resp = client.get(
        "/v1/cac/summary?from=2026-01-01&to=2026-01-31",
        headers={"X-Tenant-Slug": "acme-salon"},
    )
    assert resp.status_code == 200
    body = resp.json()
    # contract §5 fields (+ltv_ngn per cross-agent coordination)
    assert body["tenant"] == T1
    assert body["from"] == "2026-01-01" and body["to"] == "2026-01-31"
    assert body["by_channel"] == [{
        "channel": "whatsapp",
        "spend_ngn": 50000.0,
        "leads": 100,
        "conversions": 10,
        "cac_ngn": 5000.0,
    }]
    assert body["by_lga"] == [{
        "lga_id": 42, "leads": 100, "conversions": 10,
        "spend_ngn": None, "cac_ngn": None, "geom": None,
    }]
    assert body["blended_cac_ngn"] == 5000.0
    assert body["ltv_ngn"] == 31000.0
    assert body["payback_days_estimate"] == 5.0  # 5000 / (31000/31)
    assert body["data_quality"] == "ok"


def test_summary_via_tenant_query_param():
    client = _client()
    resp = client.get(f"/v1/cac/summary?tenant={T1}")
    assert resp.status_code == 200
    assert resp.json()["tenant"] == T1


def test_summary_spend_unavailable_still_200():
    client = _client(spend=FailingSpend())
    resp = client.get("/v1/cac/summary", headers={"X-Tenant-Slug": "acme-salon"})
    assert resp.status_code == 200
    body = resp.json()
    assert body["data_quality"] == "spend_unavailable"
    assert body["by_channel"][0]["spend_ngn"] == 0.0


def test_summary_unknown_tenant_slug_404():
    client = _client()
    resp = client.get("/v1/cac/summary", headers={"X-Tenant-Slug": "ghost"})
    assert resp.status_code == 404


def test_summary_requires_tenant():
    client = _client()
    resp = client.get("/v1/cac/summary")
    assert resp.status_code == 400


def test_summary_validates_date_range():
    client = _client()
    resp = client.get(
        "/v1/cac/summary?from=2026-02-01&to=2026-01-01&tenant=" + T1
    )
    assert resp.status_code == 400
    resp = client.get("/v1/cac/summary?from=not-a-date&tenant=" + T1)
    assert resp.status_code == 400


def test_summary_503_when_cac_disabled():
    client = _client(with_cac=False)
    resp = client.get("/v1/cac/summary?tenant=" + T1)
    assert resp.status_code == 503
