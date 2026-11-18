"""SPEC-W77 WS-A — platform-admin registry stats (count-not-identity, R1).

Covers: (a) response shape is EXACTLY the R1 key set (set equality — a
future field cannot slip in silently); (b) aggregate counts correct over
a hand-built fixture (2 keys, 3 memberships, 1 multi-tenant, 1 alerted);
(c) zero-disclosure — recursive proof that NO string value in the
response equals (or even contains) any fixture tenant_id / registry_key;
(d) R4 disabled posture — HMAC unset => 200 {enabled:false, zeros,
ttl/reconcile echo}, tables untouched (registry_conn spy never entered);
(e) internauth — EXACTLY the backfill endpoint's block: unset
OCR_INTERNAL_TOKEN => 503, missing token => 401, wrong token => 401
(SPEC A4(e) "match what the backfill endpoint does; document" — the
backfill endpoint returns 401 for a wrong token, and so does this one);
(f) GUC gate preserved — the enabled read rides registry_conn EXACTLY
once, never a tenant/sweep scope.

Redirection proof: the top-level imports below name symbols that do NOT
exist in base app/registry.py — against base this module fails at
collection (ImportError), and test_route_registered asserts the route
itself exists (404 on base even if the imports were satisfied).

conftest._RegistryScope replicates SQL_STATS_* in lockstep, dispatched
on the exact constants.
"""

from __future__ import annotations

import asyncio
from contextlib import asynccontextmanager
from datetime import datetime, timedelta, timezone

import pytest

# Top-level names new in SPEC-W77 — against BASE registry.py this import
# fails (ImportError), which IS the redirection proof.
from app import registry
from app.config import settings
from app.registry import (
    SQL_STATS_ALERTED_MEMBERSHIPS,
    SQL_STATS_KEYS_TOTAL,
    SQL_STATS_MEMBERSHIPS_TOTAL,
    SQL_STATS_MULTI_TENANT_KEYS,
    registry_stats,
)

from conftest import INTERNAL_TOKEN, TENANT_A, TENANT_B

REG_SECRET = "w77-test-registry-secret"
STATS = "/v1/internal/ocr/registry-stats"
AUTH = {"X-Internal-Token": INTERNAL_TOKEN}
T0 = datetime(2026, 1, 5, 12, 0, 0, tzinfo=timezone.utc)

R1_KEYS = {
    "enabled",
    "keys_total",
    "memberships_total",
    "multi_tenant_keys",
    "alerted_memberships",
    "ttl_days",
    "reconcile_minutes",
}


@pytest.fixture
def reg_client(client, deps, monkeypatch):
    """The standard fake-deps app with the registry ENABLED (test secret)."""
    monkeypatch.setattr(settings, "ocr_registry_hmac_secret", REG_SECRET)
    return client


async def _reg(db, tenant, sha, ts, secret=REG_SECRET):
    return await registry.register_document(
        db, secret=secret, tenant_id=tenant, sha256=sha, first_seen=ts
    )


async def _build_fixture_async(db):
    """Hand-built fixture: 2 keys, 3 memberships, 1 multi-tenant key,
    1 alerted membership.

    key_shared: TENANT_A (T0) + TENANT_B (T0+1h) -> cross-tenant match,
                B's membership alerted (G5 later-tenant rule).
    key_solo:   TENANT_A (T0) -> single membership, never alerted.
    """
    sha_shared, sha_solo = "aa" * 32, "bb" * 32
    await _reg(db, TENANT_A, sha_shared, T0)
    await _reg(db, TENANT_B, sha_shared, T0 + timedelta(hours=1))
    await _reg(db, TENANT_A, sha_solo, T0)
    return {
        "shared": registry.registry_key(sha_shared, REG_SECRET),
        "solo": registry.registry_key(sha_solo, REG_SECRET),
    }


def _build_fixture(deps):
    """Sync wrapper for the route-level (TestClient) tests."""
    return asyncio.run(_build_fixture_async(deps.db))


def _walk_strings(node):
    """Yield EVERY string in a JSON-shaped structure (keys AND values,
    recursively) — the zero-disclosure scan surface."""
    if isinstance(node, dict):
        for k, v in node.items():
            yield from _walk_strings(k)
            yield from _walk_strings(v)
    elif isinstance(node, (list, tuple)):
        for v in node:
            yield from _walk_strings(v)
    elif isinstance(node, str):
        yield node


# ---------------------------------------------------------------------------
# (a) response shape is EXACTLY the R1 key set — set equality, both postures
# ---------------------------------------------------------------------------
def test_a_response_shape_is_exactly_the_r1_key_set(reg_client, deps):
    resp = reg_client.get(STATS, headers=AUTH)
    assert resp.status_code == 200, resp.text
    assert set(resp.json()) == R1_KEYS  # no more, no less — ever


def test_a_disabled_response_shape_is_also_exactly_the_r1_key_set(
    client, deps, monkeypatch
):
    monkeypatch.setattr(settings, "ocr_registry_hmac_secret", "")
    resp = client.get(STATS, headers=AUTH)
    assert resp.status_code == 200, resp.text
    assert set(resp.json()) == R1_KEYS


# ---------------------------------------------------------------------------
# (b) aggregate counts correct over the hand-built fixture
# ---------------------------------------------------------------------------
def test_b_counts_correct_over_fixture(reg_client, deps):
    keys = _build_fixture(deps)
    assert len(deps.db.registry_keys) == 2
    assert len(deps.db.registry_members) == 3
    assert deps.db.registry_members[(keys["shared"], TENANT_B)]["alerted"] is True
    assert deps.db.registry_members[(keys["shared"], TENANT_A)]["alerted"] is False
    assert deps.db.registry_members[(keys["solo"], TENANT_A)]["alerted"] is False

    resp = reg_client.get(STATS, headers=AUTH)
    assert resp.status_code == 200, resp.text
    assert resp.json() == {
        "enabled": True,
        "keys_total": 2,
        "memberships_total": 3,
        "multi_tenant_keys": 1,
        "alerted_memberships": 1,
        "ttl_days": int(settings.registry_ttl_days),
        "reconcile_minutes": int(settings.registry_reconcile_minutes),
    }


@pytest.mark.asyncio
async def test_b_registry_stats_fn_matches_over_fixture(deps):
    """The aggregate fn itself (the route is a thin internauth wrapper)."""
    await _build_fixture_async(deps.db)
    stats = await registry_stats(
        deps.db, secret_set=True, ttl_days=180, reconcile_minutes=60
    )
    assert stats == {
        "enabled": True,
        "keys_total": 2,
        "memberships_total": 3,
        "multi_tenant_keys": 1,
        "alerted_memberships": 1,
        "ttl_days": 180,
        "reconcile_minutes": 60,
    }


# ---------------------------------------------------------------------------
# (c) zero-disclosure: recursively, NO string value in the response equals
# (or contains) any fixture tenant_id or registry_key
# ---------------------------------------------------------------------------
def test_c_zero_disclosure_no_identity_leaks(reg_client, deps):
    keys = _build_fixture(deps)
    forbidden = {TENANT_A, TENANT_B, keys["shared"], keys["solo"]}

    resp = reg_client.get(STATS, headers=AUTH)
    assert resp.status_code == 200, resp.text
    body = resp.json()
    # key-level: no field may even be NAMED like an identity carrier
    for name in ("tenant_id", "registry_key", "tenants", "keys", "hashes"):
        assert name not in body
    # recursive value scan: no string ANYWHERE equals a fixture identity
    for s in _walk_strings(body):
        assert s not in forbidden
    # and the raw response bytes contain none of them at all
    for token in forbidden:
        assert token not in resp.text
    # count-not-identity: every VALUE is an int/bool — the only strings
    # in the payload are the R1 field names themselves
    assert all(
        isinstance(v, (int, bool)) and not isinstance(v, str)
        for v in body.values()
    )
    assert all(s in R1_KEYS for s in _walk_strings(body))


def test_c_stats_sql_selects_counts_only():
    """The aggregate SQL constants are COUNT(*) aggregates — they name NO
    identity column in the SELECT list (zero schema change: the WHERE
    clauses ride the existing tenant_count / alerted columns only)."""
    for sql in (
        SQL_STATS_KEYS_TOTAL,
        SQL_STATS_MEMBERSHIPS_TOTAL,
        SQL_STATS_MULTI_TENANT_KEYS,
        SQL_STATS_ALERTED_MEMBERSHIPS,
    ):
        head = sql.strip().splitlines()[0].strip().upper()
        assert head in (
            "SELECT COUNT(*) FROM OCR_DOC_REGISTRY",
            "SELECT COUNT(*) FROM OCR_DOC_REGISTRY_TENANTS",
        )
        select_list = sql.upper().split("WHERE")[0]
        assert "REGISTRY_KEY" not in select_list
        assert "TENANT_ID" not in select_list
        # zero schema change: no column beyond the W67 allowlist
        assert "EXPIRES" not in sql.upper()
    assert "tenant_count >= 2" in SQL_STATS_MULTI_TENANT_KEYS
    assert "alerted" in SQL_STATS_ALERTED_MEMBERSHIPS


# ---------------------------------------------------------------------------
# (d) R4 disabled posture: HMAC unset => 200 enabled:false + zeros +
# ttl/reconcile echo; the tables are NEVER touched (registry_conn spy)
# ---------------------------------------------------------------------------
def test_d_disabled_posture_200_zeros_tables_untouched(client, deps, monkeypatch):
    monkeypatch.setattr(settings, "ocr_registry_hmac_secret", "")
    # Pre-existing registry state (e.g. predating a rotation/unset) must
    # NOT leak: counts are honest zeros, and the state is untouched.
    deps.db.registry_keys["k"] = {
        "registry_key": "k",
        "first_seen": T0,
        "last_seen": T0,
        "tenant_count": 1,
        "doc_count": 1,
    }
    deps.db.registry_members[("k", TENANT_A)] = {
        "registry_key": "k",
        "tenant_id": TENANT_A,
        "first_seen": T0,
        "alerted": False,
    }
    before_keys = {k: dict(v) for k, v in deps.db.registry_keys.items()}
    before_members = {m: dict(v) for m, v in deps.db.registry_members.items()}

    entered = {"registry_conn": 0}
    orig = deps.db.registry_conn

    @asynccontextmanager
    async def spy_registry_conn():
        entered["registry_conn"] += 1
        async with orig() as scope:
            yield scope

    monkeypatch.setattr(deps.db, "registry_conn", spy_registry_conn)

    resp = client.get(STATS, headers=AUTH)
    assert resp.status_code == 200, resp.text  # posture page, never an error
    assert resp.json() == {
        "enabled": False,
        "keys_total": 0,
        "memberships_total": 0,
        "multi_tenant_keys": 0,
        "alerted_memberships": 0,
        "ttl_days": int(settings.registry_ttl_days),
        "reconcile_minutes": int(settings.registry_reconcile_minutes),
    }
    assert entered["registry_conn"] == 0  # tables untouched (cheap, honest)
    assert deps.db.registry_keys == before_keys
    assert deps.db.registry_members == before_members


# ---------------------------------------------------------------------------
# (e) internauth — EXACTLY the backfill endpoint's block: 503 when
# OCR_INTERNAL_TOKEN unset; 401 for a missing OR wrong token (the
# backfill's documented semantics; a wrong token is never distinguished
# from a missing one — no oracle)
# ---------------------------------------------------------------------------
def test_e_internauth_fail_closed(reg_client, deps, monkeypatch):
    # unset OCR_INTERNAL_TOKEN => 503 (fail closed), token header or not
    monkeypatch.setattr(settings, "internal_token", "")
    assert reg_client.get(STATS, headers=AUTH).status_code == 503
    assert reg_client.get(STATS).status_code == 503

    monkeypatch.setattr(settings, "internal_token", INTERNAL_TOKEN)
    # missing token header => 401 (same as the backfill endpoint)
    assert reg_client.get(STATS).status_code == 401
    # wrong token => 401 (same as the backfill endpoint — no oracle)
    assert (
        reg_client.get(STATS, headers={"X-Internal-Token": "wrong"}).status_code
        == 401
    )
    # correct token => 200
    assert reg_client.get(STATS, headers=AUTH).status_code == 200
    # auth failures touch NOTHING
    assert deps.db.registry_keys == {}
    assert deps.db.registry_members == {}


# ---------------------------------------------------------------------------
# (f) GUC gate preserved: the enabled read rides registry_conn EXACTLY
# once — the ONLY GUC access path; tenant/sweep scopes are never used
# ---------------------------------------------------------------------------
def test_f_enabled_read_uses_registry_conn_only(reg_client, deps, monkeypatch):
    _build_fixture(deps)
    used = {"registry": 0, "tenant": 0, "sweep": 0}
    orig_registry, orig_tenant, orig_sweep = (
        deps.db.registry_conn,
        deps.db.tenant_conn,
        deps.db.sweep_conn,
    )

    @asynccontextmanager
    async def spy_registry():
        used["registry"] += 1
        async with orig_registry() as scope:
            yield scope

    @asynccontextmanager
    async def spy_tenant(tenant_id):
        used["tenant"] += 1
        async with orig_tenant(tenant_id) as scope:
            yield scope

    @asynccontextmanager
    async def spy_sweep():
        used["sweep"] += 1
        async with orig_sweep() as scope:
            yield scope

    monkeypatch.setattr(deps.db, "registry_conn", spy_registry)
    monkeypatch.setattr(deps.db, "tenant_conn", spy_tenant)
    monkeypatch.setattr(deps.db, "sweep_conn", spy_sweep)

    resp = reg_client.get(STATS, headers=AUTH)
    assert resp.status_code == 200, resp.text
    assert resp.json()["keys_total"] == 2
    assert used == {"registry": 1, "tenant": 0, "sweep": 0}


# ---------------------------------------------------------------------------
# redirection: the top-level imports of SQL_STATS_* / registry_stats fail
# against BASE (ImportError at collection). This test additionally pins
# that the route is registered on the app (404 on base even if the
# imports were somehow satisfied).
# ---------------------------------------------------------------------------
def test_route_registered(reg_client):
    resp = reg_client.get(STATS, headers=AUTH)
    assert resp.status_code == 200, resp.text
