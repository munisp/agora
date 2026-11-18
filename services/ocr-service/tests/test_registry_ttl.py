"""SPEC-W76 — registry TTL-membership reconciler (WS-A).

Offline (FakeDB) battery for reconcile_registry_ttl / registry_reconcile_loop:
(a) aged membership expired, fresh kept; (b) parent dropped when the last
membership leaves, tenant_count correct; (c) shared key: expiring the aged
membership keeps the parent + other membership, recount correct; (d) drift
repair (hand-corrupted tenant_count fixed, doc_count NEVER rewritten);
(e) zero-schema assumption (R5: no expires column referenced);
(f) R3: reconcile works with NO HMAC secret and rides registry_conn ONLY;
(g) loop: one tick with interval>0 runs reconcile exactly once,
cancel-clean; (h) R4 re-alert semantics; (i) idempotence.

conftest._RegistryScope replicates SQL_EXPIRE_MEMBERSHIPS / SQL_DRIFT_SWEEP
in lockstep, dispatched on the exact constants.
"""

from __future__ import annotations

import asyncio
from contextlib import asynccontextmanager
from datetime import datetime, timedelta, timezone

import pytest

# Top-level names new in SPEC-W76 — against BASE registry.py this import
# fails (ImportError), which IS the A6 redirection proof.
from app import registry
from app.config import settings
from app.registry import (
    REGISTRY_TTL_DRIFT_REPAIRED,
    REGISTRY_TTL_KEYS_DROPPED,
    REGISTRY_TTL_MEMBERSHIPS_EXPIRED,
    SQL_DRIFT_SWEEP,
    SQL_EXPIRE_MEMBERSHIPS,
    reconcile_registry_ttl,
    registry_reconcile_loop,
)

from conftest import TENANT_A, TENANT_B

REG_SECRET = "w76-test-registry-secret"
NOW = datetime(2026, 7, 1, 12, 0, 0, tzinfo=timezone.utc)
OLD = NOW - timedelta(days=200)  # aged vs the default 180d TTL
FRESH = NOW - timedelta(days=10)
TTL = 180


async def _reg(db, tenant, sha, ts, secret=REG_SECRET):
    return await registry.register_document(
        db, secret=secret, tenant_id=tenant, sha256=sha, first_seen=ts
    )


def _key(sha: str) -> str:
    return registry.registry_key(sha, REG_SECRET)


# ---------------------------------------------------------------------------
# (a) aged membership expired, fresh kept (+ W67-pattern metric counters)
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_a_aged_membership_expired_fresh_kept(deps):
    sha_old, sha_fresh = "aa" * 32, "bb" * 32
    await _reg(deps.db, TENANT_A, sha_old, OLD)
    await _reg(deps.db, TENANT_A, sha_fresh, FRESH)

    e0 = REGISTRY_TTL_MEMBERSHIPS_EXPIRED._value.get()
    d0 = REGISTRY_TTL_KEYS_DROPPED._value.get()
    r0 = REGISTRY_TTL_DRIFT_REPAIRED._value.get()
    stats = await reconcile_registry_ttl(deps.db, now=NOW, ttl_days=TTL)

    assert stats == {
        "memberships_expired": 1,
        "keys_dropped": 1,
        "keys_drift_repaired": 0,
    }
    key_old, key_fresh = _key(sha_old), _key(sha_fresh)
    assert (key_old, TENANT_A) not in deps.db.registry_members
    assert key_old not in deps.db.registry_keys  # sole member -> parent dropped
    assert (key_fresh, TENANT_A) in deps.db.registry_members
    assert deps.db.registry_keys[key_fresh]["tenant_count"] == 1
    assert REGISTRY_TTL_MEMBERSHIPS_EXPIRED._value.get() == e0 + 1
    assert REGISTRY_TTL_KEYS_DROPPED._value.get() == d0 + 1
    assert REGISTRY_TTL_DRIFT_REPAIRED._value.get() == r0


# ---------------------------------------------------------------------------
# (b) parent dropped when the last membership leaves; tenant_count correct
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_b_parent_dropped_when_last_membership_leaves(deps):
    sha = "cc" * 32
    await _reg(deps.db, TENANT_A, sha, OLD)
    await _reg(deps.db, TENANT_B, sha, OLD + timedelta(hours=1))
    key = _key(sha)
    assert deps.db.registry_keys[key]["tenant_count"] == 2

    stats = await reconcile_registry_ttl(deps.db, now=NOW, ttl_days=TTL)
    assert stats == {
        "memberships_expired": 2,
        "keys_dropped": 1,
        "keys_drift_repaired": 0,
    }
    assert key not in deps.db.registry_keys  # parent dropped at zero
    assert deps.db.registry_members == {}


# ---------------------------------------------------------------------------
# (c) shared key: expiring the aged membership keeps parent + other tenant
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_c_shared_key_keeps_parent_and_fresh_membership(deps):
    sha = "dd" * 32
    await _reg(deps.db, TENANT_A, sha, OLD)     # aged out
    await _reg(deps.db, TENANT_B, sha, FRESH)   # retained
    key = _key(sha)
    doc_count_before = deps.db.registry_keys[key]["doc_count"]

    stats = await reconcile_registry_ttl(deps.db, now=NOW, ttl_days=TTL)
    assert stats == {
        "memberships_expired": 1,
        "keys_dropped": 0,
        "keys_drift_repaired": 0,
    }
    assert (key, TENANT_A) not in deps.db.registry_members
    assert (key, TENANT_B) in deps.db.registry_members
    parent = deps.db.registry_keys[key]
    assert parent["tenant_count"] == 1  # recount, never trust
    assert parent["doc_count"] == doc_count_before  # cumulative, untouched


# ---------------------------------------------------------------------------
# (d) R6 drift repair: corrupted tenant_count re-counted; doc_count NEVER
# rewritten; empty parent rows dropped by the drift sweep
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_d_drift_repair_fixes_tenant_count_never_doc_count(deps):
    sha1, sha2 = "ee" * 32, "ff" * 32
    await _reg(deps.db, TENANT_A, sha1, FRESH)
    await _reg(deps.db, TENANT_B, sha1, FRESH + timedelta(hours=1))
    await _reg(deps.db, TENANT_A, sha2, FRESH)
    key1, key2 = _key(sha1), _key(sha2)
    doc_count_before = deps.db.registry_keys[key1]["doc_count"]

    # hand-corrupt drift (R6b) + an orphan parent (R6a, e.g. a crashed
    # TenantDelete that removed memberships but never dropped the parent)
    deps.db.registry_keys[key1]["tenant_count"] = 99
    del deps.db.registry_members[(key2, TENANT_A)]

    stats = await reconcile_registry_ttl(deps.db, now=NOW, ttl_days=TTL)
    assert stats == {
        "memberships_expired": 0,
        "keys_dropped": 1,
        "keys_drift_repaired": 1,
    }
    assert deps.db.registry_keys[key1]["tenant_count"] == 2  # re-counted
    assert deps.db.registry_keys[key1]["doc_count"] == doc_count_before
    assert key2 not in deps.db.registry_keys  # orphan parent dropped


# ---------------------------------------------------------------------------
# (e) R5 zero-schema: no expires column anywhere in the reconciler SQL; the
# membership age basis is the pre-existing immutable first_seen
# ---------------------------------------------------------------------------
def test_e_zero_schema_no_expires_column():
    for sql in (SQL_EXPIRE_MEMBERSHIPS, SQL_DRIFT_SWEEP):
        assert "expires" not in sql.lower()
    assert "first_seen" in SQL_EXPIRE_MEMBERSHIPS


# ---------------------------------------------------------------------------
# (f) R3: reconcile works with NO HMAC secret and rides registry_conn ONLY
# (one transaction — one registry_conn scope; tenant/sweep scopes unused)
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_f_reconcile_needs_no_hmac_and_uses_registry_conn_only(deps, monkeypatch):
    monkeypatch.setattr(settings, "ocr_registry_hmac_secret", "")  # disabled
    sha = "11" * 32
    await _reg(deps.db, TENANT_A, sha, OLD)
    await _reg(deps.db, TENANT_B, "22" * 32, FRESH)

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

    stats = await reconcile_registry_ttl(deps.db, now=NOW, ttl_days=TTL)
    assert stats["memberships_expired"] == 1
    # ONE registry_conn scope (one transaction); NO other access path
    assert used == {"registry": 1, "tenant": 0, "sweep": 0}


# ---------------------------------------------------------------------------
# (g) loop: one tick with interval>0 runs reconcile exactly once (patched
# cadence/clock), cancel-clean
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_g_loop_one_tick_runs_reconcile_once_cancel_clean(deps, monkeypatch):
    monkeypatch.setattr(settings, "registry_reconcile_minutes", 1)
    monkeypatch.setattr(settings, "registry_ttl_days", TTL)

    calls = []
    real_reconcile = registry.reconcile_registry_ttl

    async def spy_reconcile(db, *, now, ttl_days):
        calls.append(ttl_days)
        return await real_reconcile(db, now=now, ttl_days=ttl_days)

    monkeypatch.setattr(registry, "reconcile_registry_ttl", spy_reconcile)

    real_sleep = asyncio.sleep
    delays = []

    async def fake_sleep(delay):
        if delay > 0:
            delays.append(delay)
            if len(delays) >= 2:
                await asyncio.Event().wait()  # park until cancelled
            return
        await real_sleep(0)  # preserve yield points

    monkeypatch.setattr(asyncio, "sleep", fake_sleep)

    task = asyncio.create_task(registry.registry_reconcile_loop(deps.db, settings))
    for _ in range(50):  # let the loop run one full tick
        await real_sleep(0)
    assert calls == [TTL]  # exactly ONE reconcile run
    assert delays == [60, 60]  # interval derived from the knob (1 min), then parked
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert task.cancelled()
    assert calls == [TTL]  # cancel-clean: no extra run after cancel


# ---------------------------------------------------------------------------
# (h) R4 re-alert semantics: an expired membership that re-uploads is a NEW
# membership (alerted=false) and MAY alert again (stale protection lapsed)
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_h_re_register_after_expiry_is_fresh_membership(deps):
    # solo key: expiry drops the parent; re-register creates a FRESH
    # membership with alerted=false
    sha_solo = "33" * 32
    await _reg(deps.db, TENANT_A, sha_solo, OLD)
    await reconcile_registry_ttl(deps.db, now=NOW, ttl_days=TTL)
    key_solo = _key(sha_solo)
    assert key_solo not in deps.db.registry_keys

    res = await _reg(deps.db, TENANT_A, sha_solo, FRESH)
    assert res["created"] and res["membership_added"] and not res["match"]
    member = deps.db.registry_members[(key_solo, TENANT_A)]
    assert member["alerted"] is False
    assert member["first_seen"] == FRESH

    # shared key: A aged out, B retained; A's re-upload is a NEW membership
    # and MAY alert again (B is still protected)
    sha_shared = "44" * 32
    await _reg(deps.db, TENANT_A, sha_shared, OLD)
    await _reg(deps.db, TENANT_B, sha_shared, FRESH)
    key_shared = _key(sha_shared)
    stats = await reconcile_registry_ttl(deps.db, now=NOW, ttl_days=TTL)
    assert stats["memberships_expired"] == 1
    assert (key_shared, TENANT_A) not in deps.db.registry_members

    res = await _reg(deps.db, TENANT_A, sha_shared, NOW)
    assert res["membership_added"] and res["match"]  # re-alert MAY fire
    assert res["other_tenant_ids"] == [TENANT_B]
    assert deps.db.registry_members[(key_shared, TENANT_A)]["alerted"] is True
    assert deps.db.registry_keys[key_shared]["tenant_count"] == 2


# ---------------------------------------------------------------------------
# (i) idempotence: a second run with nothing aged is all-zero
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_i_second_run_with_nothing_aged_is_all_zero(deps):
    await _reg(deps.db, TENANT_A, "55" * 32, OLD)
    first = await reconcile_registry_ttl(deps.db, now=NOW, ttl_days=TTL)
    assert first["memberships_expired"] == 1
    second = await reconcile_registry_ttl(deps.db, now=NOW, ttl_days=TTL)
    assert second == {
        "memberships_expired": 0,
        "keys_dropped": 0,
        "keys_drift_repaired": 0,
    }
