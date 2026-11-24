"""Admin route tests (SPEC-W118 §4): K19 token guard, live credential
validation, webhook auto-registration, stable tokens, masking, delete."""

from __future__ import annotations

import uuid

import pytest
from fastapi.testclient import TestClient

pytestmark = pytest.mark.asyncio

from conftest import (  # noqa: E402
    BRIDGE_TOKEN,
    TENANT_A,
    FakeChatwoot,
    FakeConvClient,
    FakeStore,
)

from app.config import Settings  # noqa: E402
from app.main import create_app  # noqa: E402

HDR = {"X-Internal-Token": BRIDGE_TOKEN}


def _app(cw, store=None, *, token=BRIDGE_TOKEN):
    cfg = Settings(sync_enabled=False, chatwoot_bridge_internal_token=token)
    return create_app(
        cfg,
        store=store or FakeStore(),
        chatwoot_client=cw,
        conv_client=FakeConvClient(),
    )


def _body(**overrides):
    b = {
        "tenant_id": TENANT_A,
        "account_id": 7,
        "inbox_id": 12,
        "api_access_token": "cw-live-token",
    }
    b.update(overrides)
    return b


def test_guard_503_when_server_token_unset():
    with TestClient(_app(FakeChatwoot(), token="")) as c:
        r = c.post("/internal/tenants/register", json=_body(), headers=HDR)
        assert r.status_code == 503


def test_guard_401_on_wrong_or_missing_token():
    with TestClient(_app(FakeChatwoot())) as c:
        assert c.post("/internal/tenants/register",
                      json=_body()).status_code == 401
        assert c.post("/internal/tenants/register", json=_body(),
                      headers={"X-Internal-Token": "nope"}).status_code == 401


def test_register_happy_path_auto_registers_webhook():
    cw = FakeChatwoot()
    with TestClient(_app(cw)) as c:
        r = c.post("/internal/tenants/register", json=_body(), headers=HDR)
        assert r.status_code == 200
        out = r.json()
        assert out["tenant_id"] == TENANT_A
        assert out["account_id"] == 7
        assert out["inbox_id"] == 12
        assert out["webhook_registered"] is True
        assert out["api_access_token"] == "…oken"  # masked
        assert TENANT_A in out["webhook_url"]
    # webhook created in chatwoot pointing back at the bridge
    assert len(cw.webhooks) == 1
    hook = cw.webhooks[0]
    assert hook["subscriptions"] == ["message_type"]
    assert hook["url"] == out["webhook_url"]
    assert "/v1/chatwoot/webhook/" in hook["url"]


def test_register_rejects_unusable_credential():
    cw = FakeChatwoot(inbox_valid=False)
    store = FakeStore()
    with TestClient(_app(cw, store)) as c:
        r = c.post("/internal/tenants/register", json=_body(), headers=HDR)
        assert r.status_code == 400
    assert store.tenants == {}  # nothing stored on failed validation


def test_reregister_keeps_stable_webhook_token_and_reuses_hook():
    cw = FakeChatwoot()
    store = FakeStore()
    with TestClient(_app(cw, store)) as c:
        r1 = c.post("/internal/tenants/register", json=_body(), headers=HDR)
        r2 = c.post("/internal/tenants/register",
                    json=_body(api_access_token="rotated-token-1234"),
                    headers=HDR)
        assert r1.json()["webhook_url"] == r2.json()["webhook_url"]
    assert len(cw.webhooks) == 1  # reused, not duplicated
    assert store.tenants[TENANT_A]["api_access_token"] == "rotated-token-1234"


def test_webhook_autoreg_failure_still_stores_mapping():
    cw = FakeChatwoot()
    cw.fail_next = True  # validate_inbox would fail -> move failure later
    # first call is validate_inbox; make that succeed and webhooks fail
    cw.fail_next = False
    real_create = cw.create_webhook

    async def boom(*a, **k):
        from app.client import ChatwootError
        raise ChatwootError(500, "webhook endpoint down")

    cw.create_webhook = boom
    store = FakeStore()
    with TestClient(_app(cw, store)) as c:
        r = c.post("/internal/tenants/register", json=_body(), headers=HDR)
        assert r.status_code == 200
        assert r.json()["webhook_registered"] is False
    assert TENANT_A in store.tenants


def test_get_masks_and_delete_removes_everything():
    cw = FakeChatwoot()
    store = FakeStore()
    with TestClient(_app(cw, store)) as c:
        c.post("/internal/tenants/register", json=_body(), headers=HDR)
        r = c.get(f"/internal/tenants/{TENANT_A}", headers=HDR)
        assert r.status_code == 200
        assert r.json()["api_access_token"] == "…oken"
        assert "cw-live-token" not in str(r.json())
        r = c.delete(f"/internal/tenants/{TENANT_A}", headers=HDR)
        assert r.status_code == 200
        assert c.get(f"/internal/tenants/{TENANT_A}",
                     headers=HDR).status_code == 404
        assert store.tenants == {}


def test_get_unknown_tenant_404():
    with TestClient(_app(FakeChatwoot())) as c:
        assert c.get(f"/internal/tenants/{uuid.uuid4()}",
                     headers=HDR).status_code == 404
