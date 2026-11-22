"""USSD grounding-quality metrics tests (SPEC-W112).

Every eligible USSD text-mode turn (grounding enabled, not menu mode,
non-empty parsed query) increments conversation_grounding_turns_total
exactly once with result composed|raw|compose_fallback|ungrounded|error;
turns that would have been eligible but for the grounding flag count as
"disabled". Menu turns and empty first turns are not counted. Tenant
labels are sanitized and cardinality-capped (overflow aggregation).

REDIRECTION PROOF (SPEC-W112 R2): with the record_grounding_turn call
removed from the USSD route, the raw/ungrounded/error/disabled tests
here FAIL. R4: with the tenant cap removed, the overflow test FAILS.
"""

from __future__ import annotations

import asyncio
import sys
import uuid

import pytest

sys.path.insert(0, ".")

from app import grounding_metrics, kb_compose  # noqa: E402
from app.config import Config  # noqa: E402

from test_ussd_grounding import SESSION, TENANT, _KbDapr, _app, _post  # noqa: E402

pytestmark = pytest.mark.asyncio


@pytest.fixture(autouse=True)
def _reset_metrics():
    grounding_metrics.reset_for_tests()
    yield
    grounding_metrics.reset_for_tests()


def _series(**want: str) -> float:
    """Sum of counter samples matching the given label subset."""
    total = 0.0
    for metric in grounding_metrics.grounding_turns.collect():
        for sample in metric.samples:
            if sample.name == "conversation_grounding_turns_total" and all(
                sample.labels.get(k) == v for k, v in want.items()
            ):
                total += sample.value
    return total


# ------------------------------------------------- route outcomes
def test_raw_grounded_turn_counted():
    from fastapi.testclient import TestClient

    app = _app()
    with TestClient(app) as c:
        r = _post(c, text="When do you open?")
        assert r.json()["grounded"] is True
    assert _series(tenant=str(TENANT), channel="ussd", result="raw") == 1.0


def test_ungrounded_turn_counted():
    from fastapi.testclient import TestClient

    app = _app(kb_outcome={"snippets": []})
    with TestClient(app) as c:
        r = _post(c, text="obscure question")
        assert r.json()["grounded"] is False
    assert _series(tenant=str(TENANT), channel="ussd", result="ungrounded") == 1.0


def test_error_turn_counted_and_callback_still_200():
    from fastapi.testclient import TestClient

    app = _app(kb_outcome=RuntimeError("503 sidecar down"))
    with TestClient(app) as c:
        r = _post(c, text="question")
        assert r.status_code == 200
        assert r.json()["grounded"] is False
    assert _series(tenant=str(TENANT), channel="ussd", result="error") == 1.0


def test_timeout_turn_counted_as_error():
    from fastapi.testclient import TestClient

    app = _app(kb_outcome=asyncio.TimeoutError("kb slow"))
    with TestClient(app) as c:
        r = _post(c, text="question")
        assert r.status_code == 200
    assert _series(tenant=str(TENANT), channel="ussd", result="error") == 1.0


def test_disabled_grounding_counted():
    from fastapi.testclient import TestClient

    app = _app(cfg=Config(ussd_kb_grounding=False))
    with TestClient(app) as c:
        r = _post(c, text="When do you open?")
        assert r.json()["grounded"] is False
        # KB never called, but the eligible turn is still counted
        assert app.state.dapr.invocations == []
    assert _series(tenant=str(TENANT), channel="ussd", result="disabled") == 1.0


def test_menu_and_empty_turns_not_counted():
    from fastapi.testclient import TestClient

    app = _app()
    with TestClient(app) as c:
        _post(c, text="", menu=None)  # empty first turn
        _post(c, text="1", menu=[{"key": "1", "text": "Talk to an agent"}])
    assert _series(channel="ussd") == 0.0


def test_composed_turn_counted(monkeypatch):
    from fastapi.testclient import TestClient

    async def fake_compose(query, snippets, cfg):
        return "We open 9am-5pm."

    monkeypatch.setattr(kb_compose, "compose_answer", fake_compose)
    app = _app(cfg=Config(ussd_kb_llm=True))
    with TestClient(app) as c:
        r = _post(c, text="When do you open?")
        assert r.json()["composed"] is True
    assert _series(tenant=str(TENANT), channel="ussd", result="composed") == 1.0


def test_compose_fallback_counted(monkeypatch):
    from fastapi.testclient import TestClient

    async def fake_compose(query, snippets, cfg):
        return None

    monkeypatch.setattr(kb_compose, "compose_answer", fake_compose)
    app = _app(cfg=Config(ussd_kb_llm=True))
    with TestClient(app) as c:
        r = _post(c, text="When do you open?")
        assert r.json()["grounded"] is True
        assert r.json()["composed"] is False
    assert (
        _series(tenant=str(TENANT), channel="ussd", result="compose_fallback") == 1.0
    )


def test_exactly_one_increment_per_eligible_turn():
    from fastapi.testclient import TestClient

    app = _app()
    with TestClient(app) as c:
        _post(c, text="When do you open?")
    samples = [
        s
        for m in grounding_metrics.grounding_turns.collect()
        for s in m.samples
        if s.name == "conversation_grounding_turns_total"
    ]
    assert sum(s.value for s in samples) == 1.0


# ------------------------------------------------- tenant cardinality cap
def test_tenant_label_overflow_after_cap():
    from fastapi.testclient import TestClient

    app = _app(cfg=Config(grounding_metrics_max_tenants=2))
    tenants = [uuid.uuid4() for _ in range(4)]
    with TestClient(app) as c:
        for i, tid in enumerate(tenants):
            _post(
                c,
                tenant_id=str(tid),
                session_id=f"{SESSION}-{i}",
                text="When do you open?",
            )
    assert _series(tenant=str(tenants[0]), result="raw") == 1.0
    assert _series(tenant=str(tenants[1]), result="raw") == 1.0
    # tenants beyond the cap aggregate under overflow
    assert _series(tenant="overflow", result="raw") == 2.0
    # no raw third/fourth tenant labels leaked into the series
    assert _series(tenant=str(tenants[2])) == 0.0
    assert _series(tenant=str(tenants[3])) == 0.0
    tenants_in_series = {
        s.labels["tenant"]
        for m in grounding_metrics.grounding_turns.collect()
        for s in m.samples
        if s.name == "conversation_grounding_turns_total"
    }
    assert len(tenants_in_series) == 3


def test_metric_failure_never_fails_callback(monkeypatch):
    from fastapi.testclient import TestClient

    def boom(**kwargs):
        raise RuntimeError("registry exploded")

    # record_grounding_turn is itself non-raising; even if it weren't, the
    # route must survive. Patch the module-level name the route imported.
    monkeypatch.setattr(
        "app.routes.grounding_metrics.record_grounding_turn", boom
    )
    app = _app()
    with TestClient(app) as c:
        r = _post(c, text="When do you open?")
        assert r.status_code == 200
        assert r.json()["grounded"] is True
