"""2026-11-21: LGA polygons from geo-service reach the CAC summary's by_lga rows (fail-open)."""
import asyncio
from datetime import date
from types import SimpleNamespace

import httpx

from analytics_pipeline.cac_summary import build_summary, fetch_lga_geoms

IKEJA = {"type": "MultiPolygon", "coordinates": [[[[3.3, 6.6], [3.4, 6.6], [3.4, 6.7], [3.3, 6.6]]]]}
SETTINGS = SimpleNamespace(geo_base_url="http://geo:7015")


def _run(coro):
    return asyncio.run(coro)


def test_build_summary_attaches_geom_by_code():
    out = build_summary(tenant_id="t", date_from=date(2026, 11, 1), date_to=date(2026, 11, 21), channel_rows=[],
                        lga_rows=[], spend_by_channel={}, spend_unavailable=False, day_bounds=(None, None),
                        lga_code_rows=[{"lga_code": "NG-LAGOS-IKEJA", "lga_name": "Ikeja", "leads": 2, "conversions": 0},
                                       {"lga_code": "NG-LAGOS-SURULERE", "lga_name": "Surulere", "leads": 1, "conversions": 0}],
                        lga_geoms={"NG-LAGOS-IKEJA": IKEJA})
    rows = {r["lga_code"]: r for r in out["by_lga"]}
    assert rows["NG-LAGOS-IKEJA"]["geom"] == IKEJA
    assert rows["NG-LAGOS-SURULERE"]["geom"] is None


def test_fetch_lga_geoms_parses_feature_collection():
    seen = {}

    def handler(req: httpx.Request) -> httpx.Response:
        seen["url"] = str(req.url)
        return httpx.Response(200, json={"type": "FeatureCollection", "features": [
            {"type": "Feature", "properties": {"code": "NG-LAGOS-IKEJA"}, "geometry": IKEJA}]})

    async def go():
        async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as c:
            return await fetch_lga_geoms(SETTINGS, ["NG-LAGOS-IKEJA", "NG-LAGOS-IKEJA", ""], client=c)

    assert _run(go()) == {"NG-LAGOS-IKEJA": IKEJA}
    assert "/v1/geo/boundaries" in seen["url"] and "kind=lga" in seen["url"]


def test_fetch_lga_geoms_fails_open():
    async def go():
        async with httpx.AsyncClient(transport=httpx.MockTransport(lambda r: httpx.Response(503))) as c:
            return await fetch_lga_geoms(SETTINGS, ["NG-LAGOS-IKEJA"], client=c)

    assert _run(go()) == {}


def test_fetch_lga_geoms_skips_without_codes_or_url():
    assert _run(fetch_lga_geoms(SETTINGS, [])) == {}
    assert _run(fetch_lga_geoms(SimpleNamespace(geo_base_url=""), ["NG-LAGOS-IKEJA"])) == {}
