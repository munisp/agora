"""2026-11-21: gazetteer-coded LGA rows (field captures) appear in the CAC summary by_lga list."""
from datetime import date
from decimal import Decimal

from analytics_pipeline.cac_events import parse_funnel_event
from analytics_pipeline.cac_summary import build_summary, lga_synthetic_id


def test_parse_lga_code():
    ev = parse_funnel_event({"type": "com.opendesk.cac.FunnelEvent", "data": {
        "event_id": "e1", "tenant_id": "bb69e5fd-aa86-4d4b-9d86-4cfe5c33ae52", "entity_type": "lead", "entity_id": "l1",
        "event_name": "lead_created", "event_ts": "2026-11-21T09:00:00Z", "channel": "field",
        "idempotency_key": "lead:l1:lead_created", "lga_code": "NG-LAGOS-IKEJA", "lga_name": "Ikeja"}})
    assert ev is not None and ev.lga_code == "NG-LAGOS-IKEJA" and ev.lga_name == "Ikeja"


def test_by_lga_includes_code_rows():
    out = build_summary(tenant_id="t", date_from=date(2026, 11, 1), date_to=date(2026, 11, 21), channel_rows=[],
                        lga_rows=[], spend_by_channel={}, spend_unavailable=False, day_bounds=(None, None),
                        lga_code_rows=[{"lga_code": "NG-LAGOS-IKEJA", "lga_name": "Ikeja", "leads": 3, "conversions": 1,
                                        "revenue_ngn": Decimal("0")}])
    row = out["by_lga"][0]
    assert row["lga_name"] == "Ikeja" and row["leads"] == 3 and row["lga_id"] == lga_synthetic_id("NG-LAGOS-IKEJA")
