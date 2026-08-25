"""SPEC-W43 C1 gateway tenant binding — 403/401 matrix for
conversation-service routes (_require_tenant + POST /v1/conversations
body binding).

X-Tenant-Slugs (comma-separated slugs/uuids from the validated JWT,
injected by APISIX) is the ONLY source of tenant context. Explicit
?tenant=/X-Tenant-ID/body tenant_id selectors are honored ONLY when they
exactly match a header entry (else 403). Without the header there is no
authenticated tenant context (401) unless the standalone-dev escape
OPENDESK_TRUST_DIRECT_TENANT=1 is on (never set in compose).
"""

from __future__ import annotations

import contextlib
import sys
import uuid
from datetime import UTC, datetime

from fastapi import FastAPI
from fastapi.testclient import TestClient

sys.path.insert(0, ".")

from app.config import Config  # noqa: E402
from app.logging import get_logger  # noqa: E402
from app.routes import router  # noqa: E402

TENANT_A = uuid.uuid4()
TENANT_B = uuid.uuid4()
SLUG_A = "acme"


class _FakeDB:
    def __init__(self):
        self.convs = {}

    async def create_conversation(self, tenant_id, site_slug, channel, contact_phone=None):
        cid = uuid.uuid4()
        rec = dict(id=cid, tenant_id=tenant_id, site_slug=site_slug, channel=channel,
                   contact_phone=contact_phone,
                   started_at=datetime.now(UTC), ended_at=None)
        self.convs[cid] = rec
        return rec

    async def list_conversations(self, tenant_id, limit, offset, contact=None):
        return [c for c in self.convs.values() if c["tenant_id"] == tenant_id]


class _FakeResolver:
    """TenantResolver stand-in: slug -> id map."""

    def __init__(self, mapping):
        self._mapping = mapping

    async def by_slug(self, slug):
        from app.tenants import TenantInfo, TenantNotFoundError
        if slug not in self._mapping:
            raise TenantNotFoundError(slug)
        return TenantInfo(id=str(self._mapping[slug]), slug=slug)


def _app(trust_direct: bool = False):
    @contextlib.asynccontextmanager
    async def lifespan(app):
        app.state.cfg = Config(trust_direct_tenant=trust_direct)
        app.state.db = _FakeDB()
        app.state.tenant_resolver = _FakeResolver({SLUG_A: TENANT_A})
        app.state.log = get_logger("tenant-binding-test")
        yield

    app = FastAPI(lifespan=lifespan)
    app.include_router(router)
    return app


def _client(trust_direct: bool = False):
    return TestClient(_app(trust_direct))


# ------------------------------------------------------- gateway header path
def test_header_single_uuid_selects_tenant():
    with _client() as c:
        r = c.get("/v1/conversations", headers={"X-Tenant-Slugs": str(TENANT_A)})
        assert r.status_code == 200, r.text


def test_header_single_slug_resolved_via_identity():
    with _client() as c:
        r = c.get("/v1/conversations", headers={"X-Tenant-Slugs": SLUG_A})
        assert r.status_code == 200, r.text


def test_explicit_query_param_matching_header_is_honored():
    with _client() as c:
        r = c.get(f"/v1/conversations?tenant={TENANT_A}",
                  headers={"X-Tenant-Slugs": f"{TENANT_B},{TENANT_A}"})
        assert r.status_code == 200, r.text


def test_explicit_query_param_not_in_header_is_403():
    with _client() as c:
        r = c.get(f"/v1/conversations?tenant={TENANT_B}",
                  headers={"X-Tenant-Slugs": str(TENANT_A)})
        assert r.status_code == 403, r.text


def test_explicit_slug_not_in_header_is_403():
    with _client() as c:
        r = c.get(f"/v1/conversations?tenant={SLUG_A}",
                  headers={"X-Tenant-Slugs": str(TENANT_B)})
        assert r.status_code == 403, r.text


def test_x_tenant_id_header_must_match_slugs_header():
    with _client() as c:
        r = c.get("/v1/conversations",
                  headers={"X-Tenant-Slugs": str(TENANT_A),
                           "X-Tenant-ID": str(TENANT_B)})
        assert r.status_code == 403, r.text
        r = c.get("/v1/conversations",
                  headers={"X-Tenant-Slugs": f"{TENANT_A},{TENANT_B}",
                           "X-Tenant-ID": str(TENANT_B)})
        assert r.status_code == 200, r.text


def test_multi_tenant_principal_without_selector_is_400():
    with _client() as c:
        r = c.get("/v1/conversations",
                  headers={"X-Tenant-Slugs": f"{TENANT_A},{TENANT_B}"})
        assert r.status_code == 400, r.text


def test_unknown_slug_in_header_is_404():
    with _client() as c:
        r = c.get("/v1/conversations", headers={"X-Tenant-Slugs": "no-such-org"})
        assert r.status_code == 404, r.text


# ------------------------------------------------------ body tenant binding
def test_create_conversation_body_tenant_must_match_header():
    with _client() as c:
        # match -> 201
        r = c.post("/v1/conversations",
                   json={"tenant_id": str(TENANT_A), "site_slug": "acme",
                         "channel": "voice"},
                   headers={"X-Tenant-Slugs": str(TENANT_A)})
        assert r.status_code == 201, r.text
        # mismatch -> 403 (cross-tenant write attempt)
        r = c.post("/v1/conversations",
                   json={"tenant_id": str(TENANT_B), "site_slug": "evil",
                         "channel": "voice"},
                   headers={"X-Tenant-Slugs": str(TENANT_A)})
        assert r.status_code == 403, r.text


# ----------------------------------------------------------- no header path
def test_no_gateway_header_is_401_by_default():
    with _client() as c:
        # even with a legacy ?tenant= param — no unauthenticated selection
        r = c.get(f"/v1/conversations?tenant={TENANT_A}")
        assert r.status_code == 401, r.text
        r = c.post("/v1/conversations",
                   json={"tenant_id": str(TENANT_A), "site_slug": "acme",
                         "channel": "voice"})
        assert r.status_code == 401, r.text


def test_dev_escape_restores_legacy_direct_selection():
    with _client(trust_direct=True) as c:
        r = c.get(f"/v1/conversations?tenant={TENANT_A}")
        assert r.status_code == 200, r.text
        r = c.get(f"/v1/conversations?tenant={SLUG_A}")
        assert r.status_code == 200, r.text  # slug still resolved via identity
        r = c.post("/v1/conversations",
                   json={"tenant_id": str(TENANT_A), "site_slug": "acme",
                         "channel": "voice"})
        assert r.status_code == 201, r.text
        # but without any selector it still fails closed
        r = c.get("/v1/conversations")
        assert r.status_code == 401, r.text
