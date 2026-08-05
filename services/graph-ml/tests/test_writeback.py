"""Write-back tests: chunking (500), X-Internal-Token auth header, per-tenant
payloads, cross-tenant refusal (SPEC-W29 §3 WS-A / §4 gates 1+3)."""

from __future__ import annotations

import httpx
import pytest

from graph_ml.writeback import (
    DEFAULT_CHUNK_SIZE,
    RECOMMENDATIONS_PATH,
    SCORES_PATH,
    HttpWritebackClient,
    chunked,
)


def make_client(handler, chunk_size=DEFAULT_CHUNK_SIZE):
    transport = httpx.MockTransport(handler)
    http = httpx.Client(base_url="http://graph-service.test", transport=transport)
    return HttpWritebackClient(
        base_url="http://graph-service.test",
        internal_token="s3cret",
        chunk_size=chunk_size,
        client=http,
    )


def test_chunked_slices():
    items = [{"i": i} for i in range(1001)]
    chunks = list(chunked(items, 500))
    assert [len(c) for c in chunks] == [500, 500, 1]
    assert list(chunked([], 500)) == []
    with pytest.raises(ValueError):
        list(chunked(items, 0))


def test_post_scores_chunked_with_token_header():
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(200, json={"written": 1})

    client = make_client(handler)
    scores = [
        {"tenant_id": "t1", "person_id": f"p{i}", "propensity_churn": 0.5}
        for i in range(1200)
    ]
    written = client.post_scores("t1", scores)

    assert written == 1200
    assert len(calls) == 3  # 500 + 500 + 200
    for call in calls:
        assert call.headers["X-Internal-Token"] == "s3cret"
        assert call.url.path == SCORES_PATH
        body = __import__("json").loads(call.content)
        assert body["tenant_id"] == "t1"
        assert len(body["scores"]) <= 500


def test_post_recommendations_path_and_body():
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(200, json={"written": 1})

    client = make_client(handler)
    recs = [
        {
            "tenant_id": "t1",
            "person_id": "p1",
            "offering_id": "o3",
            "score": 0.5,
            "rank": 1,
            "reason": "booked_cleaning_2x",
            "model_version": "heuristic-v1",
            "scored_at": "2026-08-05T12:00:00+00:00",
        }
    ]
    assert client.post_recommendations("t1", recs) == 1
    assert calls[0].url.path == RECOMMENDATIONS_PATH
    body = __import__("json").loads(calls[0].content)
    assert body["recommendations"][0]["reason"] == "booked_cleaning_2x"


def test_cross_tenant_writeback_refused():
    def handler(request: httpx.Request) -> httpx.Response:  # pragma: no cover
        return httpx.Response(200)

    client = make_client(handler)
    foreign = [{"tenant_id": "t2", "person_id": "pX"}]
    with pytest.raises(ValueError, match="cross-tenant"):
        client.post_scores("t1", foreign)


def test_internal_token_required():
    with pytest.raises(ValueError, match="INTERNAL_TOKEN"):
        HttpWritebackClient(base_url="http://x", internal_token="")


def test_http_error_propagates():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(401, json={"error": "bad token"})

    client = make_client(handler)
    with pytest.raises(httpx.HTTPStatusError):
        client.post_scores("t1", [{"tenant_id": "t1", "person_id": "p1"}])
