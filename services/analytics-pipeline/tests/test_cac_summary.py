"""CAC summary math + spend-join resilience tests (SPEC-W13 contract §5):
per-channel CAC, blended CAC, ltv_ngn, payback_days_estimate, and the
never-fail spend lookup (404/unreachable -> spend 0 + spend_unavailable)."""

from __future__ import annotations

import asyncio
from datetime import date
from decimal import Decimal

from analytics_pipeline.cac_summary import (
    BookingSpendClient,
    SpendUnavailable,
    _parse_spend,
    build_summary,
    fetch_summary,
)
from analytics_pipeline.config import load_settings

T1 = "11111111-1111-1111-1111-111111111111"
CAMP_A = "33333333-3333-3333-3333-333333333333"
CAMP_B = "44444444-4444-4444-4444-444444444444"

FROM = date(2026, 1, 1)
TO = date(2026, 1, 31)  # 31 days


def _channel(channel, leads, conversions, revenue):
    return {
        "channel": channel,
        "leads": leads,
        "conversions": conversions,
        "revenue_ngn": Decimal(str(revenue)),
    }


def _lga(lga_id, leads, conversions, revenue=0):
    return {
        "lga_id": lga_id,
        "leads": leads,
        "conversions": conversions,
        "revenue_ngn": Decimal(str(revenue)),
    }


def test_parse_spend_shapes():
    assert _parse_spend({"spend_ngn": 1200.5}) == Decimal("1200.5")
    assert _parse_spend({"total_spend_ngn": "300"}) == Decimal("300")
    assert _parse_spend({"amount_ngn": 7}) == Decimal("7")
    assert _parse_spend(42) == Decimal("42")
    assert _parse_spend(None) == Decimal("0")
    assert _parse_spend({"unexpected": 1}) == Decimal("0")
    assert _parse_spend("garbage") == Decimal("0")


def test_build_summary_contract_math():
    summary = build_summary(
        tenant_id=T1,
        date_from=FROM,
        date_to=TO,
        channel_rows=[
            _channel("whatsapp", leads=100, conversions=10, revenue=310000),
            _channel("web", leads=50, conversions=5, revenue=62000),
        ],
        lga_rows=[_lga(42, 60, 6), _lga(7, 40, 4)],
        spend_by_channel={"whatsapp": Decimal("50000"), "web": Decimal("20000")},
        spend_unavailable=False,
        day_bounds=(FROM, TO),
    )
    wa, web = summary["by_channel"]
    assert wa == {
        "channel": "whatsapp",
        "spend_ngn": 50000.0,
        "leads": 100,
        "conversions": 10,
        "cac_ngn": 5000.0,
    }
    assert web["cac_ngn"] == 4000.0
    # blended = 70000 / 15
    assert summary["blended_cac_ngn"] == 4666.67
    # ltv = 372000 / 15 = 24800
    assert summary["ltv_ngn"] == 24800.0
    # payback = blended / (ltv / 31d) = 4666.67 / 800 = 5.8
    assert summary["payback_days_estimate"] == 5.8
    assert summary["data_quality"] == "ok"
    assert summary["totals"] == {
        "spend_ngn": 70000.0,
        "leads": 150,
        "conversions": 15,
        "revenue_ngn": 372000.0,
    }
    # by_lga: no spend allocation, geom null (no boundary endpoint this wave)
    assert summary["by_lga"] == [
        {"lga_id": 42, "leads": 60, "conversions": 6,
         "spend_ngn": None, "cac_ngn": None, "geom": None},
        {"lga_id": 7, "leads": 40, "conversions": 4,
         "spend_ngn": None, "cac_ngn": None, "geom": None},
    ]


def test_build_summary_null_safe_when_no_conversions_or_amounts():
    summary = build_summary(
        tenant_id=T1,
        date_from=FROM,
        date_to=TO,
        channel_rows=[_channel("sms", leads=10, conversions=0, revenue=0)],
        lga_rows=[],
        spend_by_channel={"sms": Decimal("5000")},
        spend_unavailable=False,
        day_bounds=(None, None),
    )
    assert summary["by_channel"][0]["cac_ngn"] is None
    assert summary["blended_cac_ngn"] is None
    assert summary["ltv_ngn"] is None
    assert summary["payback_days_estimate"] is None


def test_build_summary_open_range_uses_data_span_for_payback():
    summary = build_summary(
        tenant_id=T1,
        date_from=None,
        date_to=None,
        channel_rows=[_channel("web", leads=10, conversions=2, revenue=20000)],
        lga_rows=[],
        spend_by_channel={"web": Decimal("10000")},
        spend_unavailable=False,
        day_bounds=(date(2026, 1, 1), date(2026, 1, 10)),  # 10 days
    )
    assert summary["from"] is None and summary["to"] is None
    # blended=5000, ltv=10000, daily=1000 -> 5.0 days
    assert summary["payback_days_estimate"] == 5.0


def test_build_summary_spend_only_channel_surfaces():
    summary = build_summary(
        tenant_id=T1,
        date_from=FROM,
        date_to=TO,
        channel_rows=[],
        lga_rows=[],
        spend_by_channel={"promo": Decimal("8000")},
        spend_unavailable=False,
        day_bounds=(None, None),
    )
    assert summary["by_channel"] == [
        {"channel": "promo", "spend_ngn": 8000.0, "leads": 0,
         "conversions": 0, "cac_ngn": None}
    ]
    assert summary["totals"]["spend_ngn"] == 8000.0


def test_build_summary_marks_spend_unavailable():
    summary = build_summary(
        tenant_id=T1,
        date_from=FROM,
        date_to=TO,
        channel_rows=[_channel("web", 5, 1, 1000)],
        lga_rows=[],
        spend_by_channel={},
        spend_unavailable=True,
        day_bounds=(None, None),
    )
    assert summary["data_quality"] == "spend_unavailable"


# ---------------------------------------------------------------------------
# fetch_summary orchestration (fake store + fake spend fetcher)
# ---------------------------------------------------------------------------


class FakeStore:
    def __init__(self, campaigns):
        self._campaigns = campaigns

    async def fetch_channel_rollup(self, tenant_id, date_from, date_to):
        return [_channel("whatsapp", 100, 10, 310000)]

    async def fetch_lga_rollup(self, tenant_id, date_from, date_to):
        return [_lga(42, 100, 10)]

    async def list_campaign_channels(self, tenant_id):
        return self._campaigns

    async def rollup_day_bounds(self, tenant_id):
        return (FROM, TO)


class FakeSpend:
    def __init__(self, amounts, fail=()):
        self._amounts = amounts
        self._fail = set(fail)
        self.calls = []

    async def spend_sum(self, campaign_id, date_from, date_to):
        self.calls.append((campaign_id, date_from, date_to))
        if campaign_id in self._fail:
            raise SpendUnavailable(f"spend-sum {campaign_id}: 404")
        return Decimal(str(self._amounts[campaign_id]))


def test_fetch_summary_joins_spend_per_campaign_channel():
    store = FakeStore([
        {"campaign_id": CAMP_A, "channel": "whatsapp"},
        {"campaign_id": CAMP_B, "channel": "whatsapp"},
    ])
    spend = FakeSpend({CAMP_A: 30000, CAMP_B: 20000})
    summary = asyncio.run(
        fetch_summary(load_settings(), store, spend, T1, FROM, TO)
    )
    # two campaigns on the same channel accumulate
    assert summary["by_channel"][0]["spend_ngn"] == 50000.0
    assert summary["data_quality"] == "ok"
    assert spend.calls == [(CAMP_A, FROM, TO), (CAMP_B, FROM, TO)]


def test_fetch_summary_survives_spend_lookup_failure():
    store = FakeStore([
        {"campaign_id": CAMP_A, "channel": "whatsapp"},
        {"campaign_id": CAMP_B, "channel": "whatsapp"},
    ])
    spend = FakeSpend({CAMP_A: 30000}, fail={CAMP_B})
    summary = asyncio.run(
        fetch_summary(load_settings(), store, spend, T1, FROM, TO)
    )
    # summary still succeeds: failed campaign counts as 0, quality flagged
    assert summary["by_channel"][0]["spend_ngn"] == 30000.0
    assert summary["data_quality"] == "spend_unavailable"
    assert summary["blended_cac_ngn"] == 3000.0


# ---------------------------------------------------------------------------
# BookingSpendClient (Dapr invoke contract)
# ---------------------------------------------------------------------------


class FakeDapr:
    def __init__(self, payload=None, error=None):
        self._payload = payload
        self._error = error
        self.invocations = []

    async def invoke(self, app_id, method, *, json_body=None, params=None,
                     headers=None, http_method=None):
        self.invocations.append((app_id, method, params, http_method))
        if self._error is not None:
            raise self._error
        return self._payload


def test_booking_spend_client_invokes_internal_endpoint():
    dapr = FakeDapr(payload={"campaign_id": CAMP_A, "spend_ngn": 1234.56})
    client = BookingSpendClient(dapr, "booking")
    amount = asyncio.run(client.spend_sum(CAMP_A, FROM, TO))
    assert amount == Decimal("1234.56")
    app_id, method, params, verb = dapr.invocations[0]
    assert app_id == "booking"
    assert method == f"internal/campaigns/{CAMP_A}/spend-sum"
    assert params == {"from": "2026-01-01", "to": "2026-01-31"}
    assert verb == "GET"


def test_booking_spend_client_404_becomes_spend_unavailable():
    dapr = FakeDapr(error=RuntimeError("invoke booking/...: 404 not found"))
    client = BookingSpendClient(dapr, "booking")
    try:
        asyncio.run(client.spend_sum(CAMP_A, FROM, None))
    except SpendUnavailable:
        pass
    else:
        raise AssertionError("expected SpendUnavailable")
