"""2026-11-20: the business behind a request comes from the gateway's X-Tenant-Slugs (resolved to its tenant uuid via
identity-service), not the JWT sub (the user) - graph-sync writes every node under the business's tenant uuid."""
import asyncio

from app import auth
from app.config import Settings


def _settings():
    import dataclasses
    return dataclasses.replace(Settings(), identity_url="http://identity:7001")


def test_slug_resolves_to_business_id(monkeypatch):
    auth._TENANT_ID_CACHE.clear()
    seen = {}

    def fake(url, slug, authorization, slugs):
        seen.update(url=url, slug=slug, authz=authorization)
        return "bb69e5fd-aa86-4d4b-9d86-4cfe5c33ae52"

    monkeypatch.setattr(auth, "_lookup_tenant_id", fake)
    tid = asyncio.run(auth.tenant_id_from_slugs(_settings(), "Bearer t", "leadway-pensure,acme"))
    assert tid == "bb69e5fd-aa86-4d4b-9d86-4cfe5c33ae52"
    assert seen == {"url": "http://identity:7001", "slug": "leadway-pensure", "authz": "Bearer t"}
    # cached: a second request does not ask identity again
    monkeypatch.setattr(auth, "_lookup_tenant_id", lambda *a: (_ for _ in ()).throw(AssertionError("not cached")))
    assert asyncio.run(auth.tenant_id_from_slugs(_settings(), "Bearer t", "leadway-pensure")) == tid


def test_path_style_slug_is_trimmed(monkeypatch):
    auth._TENANT_ID_CACHE.clear()
    monkeypatch.setattr(auth, "_lookup_tenant_id", lambda url, slug, a, s: f"id-{slug}")
    assert asyncio.run(auth.tenant_id_from_slugs(_settings(), "Bearer t", "/tenants/harbour")) == "id-harbour"


def test_lookup_failure_falls_back(monkeypatch):
    auth._TENANT_ID_CACHE.clear()

    def boom(*a):
        raise OSError("identity down")

    monkeypatch.setattr(auth, "_lookup_tenant_id", boom)
    assert asyncio.run(auth.tenant_id_from_slugs(_settings(), "Bearer t", "leadway-pensure")) is None


def test_no_header_means_old_behaviour():
    assert asyncio.run(auth.tenant_id_from_slugs(_settings(), "Bearer t", None)) is None
    assert asyncio.run(auth.tenant_id_from_slugs(_settings(), None, "leadway-pensure")) is None
