"""SPEC-W67 — cross-tenant document-fraud registry (Coder A core battery).

Covers: DDL parity (BOTH copies, byte-identical block) + offline schema
allowlist; HMAC keying (raw sha256 never in registry state); live path
first-upload silence vs second-tenant pinned match payload; earlier tenant
gets NOTHING; idempotency (re-extract, backfill re-run); concurrent
same-hash race => one membership set + one alert; dry_run; keyset
pagination; disabled-when-unset (extract unaffected, backfill 503);
extract response byte-identity match vs no-match; TenantDelete own-
membership semantics (parent drops at zero); subject erasure untouched;
metrics; the app.ocr_registry GUC grep gate; and DB-gated (honest-skip)
information_schema allowlist + FORCE-RLS zero-rows proof.

Runs against the in-memory fakes (conftest _RegistryScope replicates the
registry SQL semantics in lockstep, incl. the PK-row-lock serialization).
"""

from __future__ import annotations

import asyncio
import hashlib
import hmac
import os
import re
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

from app import registry
from app.config import settings
from app.events import TYPE_CROSS_TENANT_MATCH
from app.store import DDL

from conftest import INTERNAL_TOKEN, TENANT_A, TENANT_B, make_png

REG_SECRET = "w67-test-registry-secret"
EXTRACT = "/v1/ocr/extract"
REG_BACKFILL = "/v1/internal/ocr/backfill-document-registry"
AUTH = {"X-Internal-Token": INTERNAL_TOKEN}
T0 = datetime(2026, 1, 5, 12, 0, 0, tzinfo=timezone.utc)

INIT_SQL = (
    Path(__file__).resolve().parents[3] / "infra/postgres/init-scripts/31-ocr-schema.sql"
).read_text()
REPO = Path(__file__).resolve().parents[3]

REGISTRY_BLOCK_MARKER = "-- SPEC-W67 (G1/G4): cross-tenant document-fraud registry."


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------
def _upload(client, tenant, payload, kind="receipt", subject_phone=None):
    data = {"kind": kind, "tenant": tenant}
    if subject_phone is not None:
        data["subject_phone"] = subject_phone
    resp = client.post(
        EXTRACT,
        files={"file": ("doc.png", payload, "image/png")},
        data=data,
        headers=AUTH,
    )
    assert resp.status_code == 200, resp.text
    return resp


def _sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def _key_for(data: bytes) -> str:
    return registry.registry_key(_sha(data), REG_SECRET)


def _match_events(deps):
    return [
        p
        for _pb, topic, p in deps.dapr.published
        if topic == settings.ocr_events_topic
        and isinstance(p, dict)
        and p.get("type") == TYPE_CROSS_TENANT_MATCH
    ]


def _seed(deps, tenant, *, sha256, created_at, kind="receipt", doc_id=None):
    """Insert one extracted ocr_documents row directly (store-level fixture,
    test_backfill.py idiom)."""
    doc_id = doc_id or uuid.uuid4()
    deps.db.rows[str(doc_id)] = {
        "id": doc_id,
        "tenant_id": tenant,
        "kind": kind,
        "storage_bucket": "documents",
        "storage_key": f"{tenant}/{doc_id}.png",
        "mime": "image/png",
        "bytes": 128,
        "sha256": sha256,
        "phash": None,
        "page_count": 1,
        "model_version": "minicpm-ocr-v1",
        "extracted_text": None,
        "extracted_fields": None,
        "status": "extracted",
        "error": None,
        "expires_at": None,
        "subject_phone": None,
        "created_at": created_at,
        "updated_at": created_at,
    }
    return doc_id


def _tenant_deleted_event(tenant):
    return {
        "specversion": "1.0",
        "id": str(uuid.uuid4()),
        "source": "identity-service",
        "type": "com.opendesk.identity.TenantDeleted",
        "subject": "tenant-slug",
        "tenantid": tenant,
        "data": {
            "tenant_slug": "tenant-slug",
            "tenant_id": tenant,
            "deleted_at": datetime.now(timezone.utc).isoformat(),
            "actor": "test",
        },
    }


def _table_columns(sql: str, table: str) -> list[str]:
    m = re.search(rf"CREATE TABLE IF NOT EXISTS {table} \((.*?)\);", sql, re.S)
    assert m, f"{table} DDL not found"
    cols = []
    for line in m.group(1).splitlines():
        line = line.strip().rstrip(",")
        if not line or line.split()[0] in ("PRIMARY", "CONSTRAINT", "FOREIGN", "CHECK"):
            continue
        cols.append(line.split()[0])
    return cols


@pytest.fixture
def reg_client(client, deps, monkeypatch):
    """The standard fake-deps app with the registry ENABLED (test secret)."""
    monkeypatch.setattr(settings, "ocr_registry_hmac_secret", REG_SECRET)
    return client


# ---------------------------------------------------------------------------
# DDL parity + schema allowlist (offline; test_store_ddl.py is untouched —
# these are the W67 parity assertions)
# ---------------------------------------------------------------------------
def test_registry_ddl_block_byte_identical_both_copies():
    """DDL PARITY: the entire SPEC-W67 block (statements + comments) is
    byte-identical in the runtime bootstrap (store.DDL) and the init script."""
    assert REGISTRY_BLOCK_MARKER in DDL and REGISTRY_BLOCK_MARKER in INIT_SQL
    block_ddl = DDL[DDL.index(REGISTRY_BLOCK_MARKER):]
    block_sql = INIT_SQL[INIT_SQL.index(REGISTRY_BLOCK_MARKER):]
    assert block_ddl.strip() == block_sql.strip()


def test_registry_schema_allowlist_both_copies():
    """EXACTLY the pinned columns, no extras (no document_id, storage_key,
    phash, kind, fields, text — G2 minimization)."""
    for name, sql in (("store.DDL", DDL), ("31-ocr-schema.sql", INIT_SQL)):
        assert _table_columns(sql, "ocr_doc_registry") == [
            "registry_key",
            "first_seen",
            "last_seen",
            "tenant_count",
            "doc_count",
        ], name
        assert _table_columns(sql, "ocr_doc_registry_tenants") == [
            "registry_key",
            "tenant_id",
            "first_seen",
            "alerted",
        ], name
        # FORCE RLS + single GUC-only policy on BOTH tables
        for table in ("ocr_doc_registry", "ocr_doc_registry_tenants"):
            assert f"ALTER TABLE {table} ENABLE ROW LEVEL SECURITY;" in sql, name
            assert f"ALTER TABLE {table} FORCE ROW LEVEL SECURITY;" in sql, name
            policy = re.search(
                rf"CREATE POLICY registry_internal_access ON {table}\s+USING \((.*?)\);",
                sql,
                re.S,
            )
            assert policy, f"{name}:{table} policy missing"
            assert (
                policy.group(1).strip()
                == "current_setting('app.ocr_registry', true) = 'on'"
            ), name
        # FK + cascade pinned
        assert (
            "REFERENCES ocr_doc_registry ON DELETE CASCADE" in sql
        ), name
    # G2 grep gate: no phash anywhere in the registry DDL block
    block = DDL[DDL.index(REGISTRY_BLOCK_MARKER):]
    assert "phash" not in block


def test_registry_guc_grep_gate():
    """'app.ocr_registry' appears ONLY in the registry module (runtime
    set_config), the DDL policy copies, and tests — nowhere else."""
    app_dir = REPO / "services/ocr-service/app"
    hits = sorted(
        p.name for p in app_dir.glob("*.py") if "app.ocr_registry" in p.read_text()
    )
    assert hits == ["registry.py", "store.py"]  # store.py: the DDL policy only
    for p in app_dir.glob("*.py"):
        if "set_config('app.ocr_registry'" in p.read_text():
            assert p.name == "registry.py"
    allowed = {
        "services/ocr-service/app/registry.py",
        "services/ocr-service/app/store.py",
        "infra/postgres/init-scripts/31-ocr-schema.sql",
    }
    offenders = []
    for pattern in ("services/**/*.py", "services/**/*.go", "infra/**/*.sql"):
        for p in REPO.glob(pattern):
            if "/tests/" in p.as_posix():
                continue  # test batteries may reference the GUC
            if "app.ocr_registry" in p.read_text(errors="ignore"):
                rel = p.relative_to(REPO).as_posix()
                if rel not in allowed:
                    offenders.append(rel)
    assert offenders == []


# ---------------------------------------------------------------------------
# HMAC keying (G3)
# ---------------------------------------------------------------------------
def test_registry_key_is_hmac_never_raw_sha256():
    digest = "ab" * 32
    key = registry.registry_key(digest, REG_SECRET)
    assert key == hmac.new(REG_SECRET.encode(), digest.encode(), hashlib.sha256).hexdigest()
    assert key != digest
    assert registry.registry_key(digest, "other-secret") != key
    with pytest.raises(registry.RegistryDisabled):
        registry.registry_key(digest, "")


@pytest.mark.asyncio
async def test_register_document_requires_secret(deps):
    with pytest.raises(registry.RegistryDisabled):
        await registry.register_document(
            deps.db, secret="", tenant_id=TENANT_A, sha256="ab" * 32, first_seen=T0
        )


# ---------------------------------------------------------------------------
# live path semantics (G5)
# ---------------------------------------------------------------------------
def test_first_upload_registers_silently(reg_client, deps):
    payload = make_png(seed=42)
    resp = _upload(reg_client, TENANT_A, payload)
    doc_id = resp.json()["document_id"]
    assert _match_events(deps) == []
    key = _key_for(payload)
    parent = deps.db.registry_keys[key]
    assert parent["tenant_count"] == 1
    assert parent["doc_count"] == 1
    # first_seen is the DOCUMENT row's created_at, never wall-clock
    assert parent["first_seen"] == deps.db.rows[doc_id]["created_at"]
    member = deps.db.registry_members[(key, TENANT_A)]
    assert member["alerted"] is False
    assert member["first_seen"] == deps.db.rows[doc_id]["created_at"]
    # raw sha256 NEVER written to the registry store
    assert _sha(payload) not in str(deps.db.registry_keys)
    assert _sha(payload) not in str(deps.db.registry_members)


def test_second_tenant_upload_emits_exactly_one_pinned_match(reg_client, deps):
    payload = make_png(seed=43)
    _upload(reg_client, TENANT_A, payload)
    resp_b = _upload(reg_client, TENANT_B, payload)

    events = _match_events(deps)
    assert len(events) == 1
    evt = events[0]
    # CloudEvents tenantid = the LATER tenant; subject = its OWN document_id
    # (document-event precedent, same as DocumentExtracted live/backfill)
    assert evt["tenantid"] == TENANT_B
    assert evt["subject"] == resp_b.json()["document_id"]
    assert evt["type"] == "com.opendesk.ocr.CrossTenantDocumentMatch"
    # data = EXACTLY {document_id (own), sha256 (own), kind, tenant_count}
    assert set(evt["data"]) == {"document_id", "sha256", "kind", "tenant_count"}
    assert evt["data"] == {
        "document_id": resp_b.json()["document_id"],
        "sha256": _sha(payload),
        "kind": "receipt",
        "tenant_count": 2,
    }
    # the earlier tenant gets NOTHING — no event addressed to it anywhere
    assert all(
        p.get("tenantid") != TENANT_A
        for _pb, _t, p in deps.dapr.published
        if isinstance(p, dict) and p.get("type") == TYPE_CROSS_TENANT_MATCH
    )
    assert TENANT_A not in str(evt["data"])
    # alerted flag: at-most-one per (tenant, hash)
    key = _key_for(payload)
    assert deps.db.registry_members[(key, TENANT_B)]["alerted"] is True
    assert deps.db.registry_members[(key, TENANT_A)]["alerted"] is False
    assert deps.db.registry_keys[key]["tenant_count"] == 2
    assert deps.db.registry_keys[key]["doc_count"] == 2


def test_third_tenant_alerts_only_itself(reg_client, deps):
    payload = make_png(seed=44)
    _upload(reg_client, TENANT_A, payload)
    _upload(reg_client, TENANT_B, payload)
    tenant_c = "33333333-4444-5555-6666-777777777777"
    _upload(reg_client, tenant_c, payload)
    events = _match_events(deps)
    assert [e["tenantid"] for e in events] == [TENANT_B, tenant_c]
    assert events[1]["data"]["tenant_count"] == 3
    assert deps.db.registry_keys[_key_for(payload)]["tenant_count"] == 3


def test_reextract_same_bytes_is_idempotent(reg_client, deps):
    """Re-extract of the same bytes (new document row) re-upserts the SAME
    membership: no new event, stable tenant_count/memberships (doc_count is
    a registration counter and increments by design)."""
    payload = make_png(seed=45)
    _upload(reg_client, TENANT_A, payload)
    _upload(reg_client, TENANT_B, payload)
    _upload(reg_client, TENANT_B, payload)  # same bytes again at B
    events = _match_events(deps)
    assert len(events) == 1  # still exactly one alert
    key = _key_for(payload)
    assert deps.db.registry_keys[key]["tenant_count"] == 2
    assert len(deps.db.registry_members) == 2


def test_extract_response_byte_identical_match_vs_no_match(reg_client, deps):
    """G5 timing/disclosure gate: the extract response body+status is
    invariant whether or not a cross-tenant match fired."""
    payload = make_png(seed=46)
    r_a = _upload(reg_client, TENANT_A, payload)  # first use — no match
    r_b = _upload(reg_client, TENANT_B, payload)  # second tenant — match fires
    assert len(_match_events(deps)) == 1  # a match DID fire for B...
    assert r_a.status_code == r_b.status_code == 200
    # ...but the only body difference is the inherently-unique document id
    doc_a = r_a.json()["document_id"].encode()
    doc_b = r_b.json()["document_id"].encode()
    assert doc_a != doc_b
    assert r_a.content.replace(doc_a, b"<DOC>") == r_b.content.replace(doc_b, b"<DOC>")


def test_registry_disabled_when_secret_unset(client, deps, monkeypatch):
    """G1: OCR_REGISTRY_HMAC_SECRET unset => registry DISABLED; extract path
    unaffected, no writes, no events, backfill refuses (503)."""
    monkeypatch.setattr(settings, "ocr_registry_hmac_secret", "")
    payload = make_png(seed=47)
    r_a = _upload(client, TENANT_A, payload)
    r_b = _upload(client, TENANT_B, payload)
    assert r_a.status_code == r_b.status_code == 200
    assert deps.db.registry_keys == {}
    assert deps.db.registry_members == {}
    assert _match_events(deps) == []
    resp = client.post(REG_BACKFILL, json={}, headers=AUTH)
    assert resp.status_code == 503


def test_registry_upsert_failure_never_breaks_extract(reg_client, deps, monkeypatch):
    """Best-effort hook: a registry failure is logged, never surfaced."""
    async def boom(*a, **k):
        raise RuntimeError("registry db down")

    monkeypatch.setattr(registry, "register_document", boom)
    resp = _upload(reg_client, TENANT_A, make_png(seed=48))
    assert resp.status_code == 200
    assert resp.json()["status"] == "extracted"


# ---------------------------------------------------------------------------
# concurrency: same-hash race => exactly one membership set + one alert
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_concurrent_same_hash_race_single_membership_set(deps, monkeypatch):
    monkeypatch.setattr(settings, "ocr_registry_hmac_secret", REG_SECRET)
    digest = hashlib.sha256(b"race-doc").hexdigest()
    now = datetime.now(timezone.utc)
    results = await asyncio.gather(
        registry.register_document(
            deps.db, secret=REG_SECRET, tenant_id=TENANT_A, sha256=digest, first_seen=now
        ),
        registry.register_document(
            deps.db, secret=REG_SECRET, tenant_id=TENANT_B, sha256=digest, first_seen=now
        ),
        # a redelivery racing in too — ON CONFLICT DO NOTHING absorbs it
        registry.register_document(
            deps.db, secret=REG_SECRET, tenant_id=TENANT_A, sha256=digest, first_seen=now
        ),
    )
    assert sum(1 for r in results if r["match"]) == 1  # exactly ONE alert
    assert sum(1 for r in results if r["membership_added"]) == 2  # A + B once each
    assert sum(1 for r in results if r["created"]) == 1  # one parent row
    key = registry.registry_key(digest, REG_SECRET)
    assert deps.db.registry_keys[key]["tenant_count"] == 2
    assert len(deps.db.registry_members) == 2


# ---------------------------------------------------------------------------
# backfill endpoint (W66 idiom)
# ---------------------------------------------------------------------------
def _run_registry_backfill(client, body=None):
    body = dict(body or {})
    pages = []
    while True:
        resp = client.post(REG_BACKFILL, json=body, headers=AUTH)
        assert resp.status_code == 200, resp.text
        page = resp.json()
        pages.append(page)
        if page["next_cursor"] is None:
            return pages
        assert len(pages) < 50, "pagination did not terminate"
        body["cursor"] = page["next_cursor"]


def test_backfill_internauth_fail_closed(client, deps, monkeypatch):
    monkeypatch.setattr(settings, "ocr_registry_hmac_secret", REG_SECRET)
    _seed(deps, TENANT_A, sha256="aa" * 32, created_at=T0)
    monkeypatch.setattr(settings, "internal_token", "")
    resp = client.post(REG_BACKFILL, json={}, headers={"X-Internal-Token": "x"})
    assert resp.status_code == 503
    monkeypatch.setattr(settings, "internal_token", INTERNAL_TOKEN)
    assert client.post(REG_BACKFILL, json={}).status_code == 401  # missing
    assert (
        client.post(REG_BACKFILL, json={}, headers={"X-Internal-Token": "wrong"}).status_code
        == 401
    )
    assert deps.db.registry_keys == {}
    assert _match_events(deps) == []


def test_backfill_registers_history_and_alerts_later_tenant(reg_client, deps):
    shared = "bb" * 32
    _seed(deps, TENANT_A, sha256=shared, created_at=T0)
    _seed(deps, TENANT_B, sha256=shared, created_at=T0 + timedelta(hours=1))
    _seed(deps, TENANT_A, sha256="cc" * 32, created_at=T0 + timedelta(hours=2))

    (page,) = _run_registry_backfill(reg_client)
    assert page == {
        "upserted": 3,
        "memberships_added": 3,
        "matches": 1,
        "scanned": 3,
        "skipped": 0,
        "next_cursor": None,
    }
    events = _match_events(deps)
    assert len(events) == 1
    # first-use-wins by (created_at, id): A is earlier, B is the later tenant
    assert events[0]["tenantid"] == TENANT_B
    assert set(events[0]["data"]) == {"document_id", "sha256", "kind", "tenant_count"}
    assert events[0]["data"]["sha256"] == shared
    assert events[0]["data"]["tenant_count"] == 2
    # first_seen comes from the document row's created_at (true chronology)
    key = registry.registry_key(shared, REG_SECRET)
    assert deps.db.registry_keys[key]["first_seen"] == T0
    assert deps.db.registry_keys[key]["tenant_count"] == 2
    solo_key = registry.registry_key("cc" * 32, REG_SECRET)
    assert deps.db.registry_members[(solo_key, TENANT_A)]["alerted"] is False


def test_backfill_rerun_is_idempotent(reg_client, deps):
    shared = "dd" * 32
    _seed(deps, TENANT_A, sha256=shared, created_at=T0)
    _seed(deps, TENANT_B, sha256=shared, created_at=T0 + timedelta(hours=1))
    (first,) = _run_registry_backfill(reg_client)
    assert first["matches"] == 1 and first["memberships_added"] == 2
    (second,) = _run_registry_backfill(reg_client)
    assert second == {
        "upserted": 2,
        "memberships_added": 0,
        "matches": 0,
        "scanned": 2,
        "skipped": 0,
        "next_cursor": None,
    }
    assert len(_match_events(deps)) == 1  # still exactly one alert total
    assert len(deps.db.registry_members) == 2


def test_backfill_dry_run_writes_and_emits_nothing(reg_client, deps):
    _seed(deps, TENANT_A, sha256="ee" * 32, created_at=T0)
    _seed(deps, TENANT_B, sha256="ee" * 32, created_at=T0 + timedelta(hours=1))
    (page,) = _run_registry_backfill(reg_client, {"dry_run": True})
    assert page["scanned"] == 2
    assert page["skipped"] == 2
    assert page["upserted"] == 0
    assert page["matches"] == 0
    assert deps.db.registry_keys == {}
    assert deps.db.registry_members == {}
    assert _match_events(deps) == []


def test_backfill_keyset_pagination_no_dup_no_gap(reg_client, deps):
    shas = [f"{i:02x}" * 32 for i in range(5)]
    for i, sha in enumerate(shas):
        _seed(deps, TENANT_A, sha256=sha, created_at=T0 + timedelta(hours=i))
    pages = _run_registry_backfill(reg_client, {"limit": 2})
    assert [p["scanned"] for p in pages] == [2, 2, 1]
    assert sum(p["upserted"] for p in pages) == 5
    assert pages[-1]["next_cursor"] is None
    assert all(p["next_cursor"] is not None for p in pages[:-1])
    assert len(deps.db.registry_keys) == 5  # no dup, no gap


def test_backfill_tenant_filter(reg_client, deps):
    shared = "ff" * 32
    _seed(deps, TENANT_A, sha256=shared, created_at=T0)
    _seed(deps, TENANT_B, sha256=shared, created_at=T0 + timedelta(hours=1))
    (page,) = _run_registry_backfill(reg_client, {"tenant_id": TENANT_A})
    assert page["upserted"] == 1 and page["matches"] == 0
    key = registry.registry_key(shared, REG_SECRET)
    assert set(t for _k, t in deps.db.registry_members) == {TENANT_A}
    assert deps.db.registry_keys[key]["tenant_count"] == 1


def test_live_path_and_backfill_share_the_same_upsert_fn(reg_client, deps, monkeypatch):
    """Pinned: the backfill calls the SAME registry upsert fn as the live
    extract path (registry.register_document)."""
    calls = []
    orig = registry.register_document

    async def spy(db, **kw):
        calls.append(kw)
        return await orig(db, **kw)

    monkeypatch.setattr(registry, "register_document", spy)
    payload = make_png(seed=49)
    _upload(reg_client, TENANT_A, payload)
    assert len(calls) == 1  # live path
    _seed(deps, TENANT_B, sha256=_sha(payload), created_at=T0)
    (page,) = _run_registry_backfill(reg_client)
    # the backfill scans BOTH the live-uploaded A row and the seeded B row,
    # each through the same fn (A's re-upsert is an idempotent no-op)
    assert page["upserted"] == 2 and page["matches"] == 1
    assert len(calls) == 3
    assert calls[0]["first_seen"] is not None and calls[1]["first_seen"] == T0


# ---------------------------------------------------------------------------
# erasure / TenantDelete (G7)
# ---------------------------------------------------------------------------
def test_tenant_delete_removes_only_own_memberships(reg_client, deps):
    payload = make_png(seed=50)
    _upload(reg_client, TENANT_A, payload)
    _upload(reg_client, TENANT_B, payload)
    key = _key_for(payload)
    assert len(_match_events(deps)) == 1

    resp = reg_client.post(
        "/v1/ocr/events/tenant-deleted", json=_tenant_deleted_event(TENANT_B)
    )
    assert resp.status_code == 200, resp.text
    # B's membership gone; A's protection (membership + parent row) intact
    assert (key, TENANT_B) not in deps.db.registry_members
    assert (key, TENANT_A) in deps.db.registry_members
    assert deps.db.registry_keys[key]["tenant_count"] == 1

    # deleting the last member drops the parent fingerprint row
    resp = reg_client.post(
        "/v1/ocr/events/tenant-deleted", json=_tenant_deleted_event(TENANT_A)
    )
    assert resp.status_code == 200, resp.text
    assert key not in deps.db.registry_keys
    assert deps.db.registry_members == {}


def test_subject_erasure_does_not_touch_registry(reg_client, deps):
    """G7: subject erasure (mark_erased tombstone) leaves the registry
    untouched — the registry holds no subject linkage."""
    payload = make_png(seed=51)
    phone = "+2348012345678"
    doc_a = _upload(
        reg_client, TENANT_A, payload, kind="kyc_nin_slip", subject_phone=phone
    ).json()["document_id"]
    _upload(reg_client, TENANT_B, payload, kind="kyc_nin_slip")
    key = _key_for(payload)
    before_keys = {k: dict(v) for k, v in deps.db.registry_keys.items()}
    before_members = {m: dict(v) for m, v in deps.db.registry_members.items()}

    resp = reg_client.post(
        "/v1/ocr/events/consent-erasure",
        json={
            "type": "com.opendesk.consent.ErasureRequested",
            "data": {"tenant_id": TENANT_A, "data_subject_id": phone},
        },
    )
    assert resp.status_code == 200, resp.text
    assert resp.json()["erased"] == 1
    assert deps.db.rows[doc_a]["status"] == "erased"  # tombstone...
    assert deps.db.rows[doc_a]["sha256"] == _sha(payload)  # ...retains hashes
    assert deps.db.registry_keys == before_keys  # registry UNTOUCHED
    assert deps.db.registry_members == before_members
    assert key in deps.db.registry_keys


# ---------------------------------------------------------------------------
# RLS posture (FakeDB-level) + metrics + startup posture
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_registry_unreachable_from_tenant_scopes(deps):
    """FakeDB-level RLS mirror: tenant/sweep scopes expose NO statement
    verbs at all — the registry is reachable only via registry_conn (the
    GUC scope), exactly like FORCE RLS without the app.ocr_registry escape
    (zero rows without the GUC; the real-PG proof is the DB-gated test)."""
    async with deps.db.tenant_conn(TENANT_A) as scope:
        for verb in ("execute", "fetch", "fetchrow", "fetchval"):
            assert not hasattr(scope, verb), verb
    async with deps.db.sweep_conn() as scope:
        for verb in ("execute", "fetch", "fetchrow", "fetchval"):
            assert not hasattr(scope, verb), verb
    async with deps.db.registry_conn() as scope:
        assert all(hasattr(scope, v) for v in ("execute", "fetch", "fetchrow", "fetchval"))


def test_registry_metrics_counters(reg_client, deps):
    k0 = registry.REGISTRY_KEYS._value.get()
    m0 = registry.REGISTRY_MATCHES._value.get()
    payload = make_png(seed=52)
    _upload(reg_client, TENANT_A, payload)
    _upload(reg_client, TENANT_B, payload)
    _upload(reg_client, TENANT_B, payload)  # re-extract: no new key/match
    assert registry.REGISTRY_KEYS._value.get() == k0 + 1
    assert registry.REGISTRY_MATCHES._value.get() == m0 + 1


def test_startup_posture_logged(capfd):
    registry.log_startup_posture("")
    out = capfd.readouterr().out
    assert "ocr_registry.disabled" in out
    assert "OCR_REGISTRY_HMAC_SECRET" in out
    registry.log_startup_posture("some-secret")
    assert "ocr_registry.enabled" in capfd.readouterr().out


# ---------------------------------------------------------------------------
# DB-gated: real Postgres schema allowlist + FORCE RLS proof (honest skip)
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
@pytest.mark.skipif(
    not os.environ.get("OCR_TEST_DATABASE_URL"),
    reason="OCR_TEST_DATABASE_URL unset — no test Postgres available",
)
async def test_registry_schema_allowlist_and_rls_db():
    from app.store import Database

    db = Database(os.environ["OCR_TEST_DATABASE_URL"])
    await db.connect()
    try:
        await db.ensure_ocr_documents_table()
        async with db._pool.acquire() as raw:
            cols = await raw.fetch(
                "SELECT table_name, column_name FROM information_schema.columns"
                " WHERE table_name IN ('ocr_doc_registry', 'ocr_doc_registry_tenants')"
            )
            by_table: dict[str, list[str]] = {}
            for r in cols:
                by_table.setdefault(r["table_name"], []).append(r["column_name"])
            assert sorted(by_table["ocr_doc_registry"]) == sorted(
                ["registry_key", "first_seen", "last_seen", "tenant_count", "doc_count"]
            )
            assert sorted(by_table["ocr_doc_registry_tenants"]) == sorted(
                ["registry_key", "tenant_id", "first_seen", "alerted"]
            )
            forced = await raw.fetch(
                "SELECT relname FROM pg_class"
                " WHERE relname IN ('ocr_doc_registry', 'ocr_doc_registry_tenants')"
                " AND relforcerowsecurity AND relrowsecurity"
            )
            assert sorted(r["relname"] for r in forced) == [
                "ocr_doc_registry",
                "ocr_doc_registry_tenants",
            ]
            pols = await raw.fetch(
                "SELECT c.relname, p.polname, pg_get_expr(p.polqual, p.polrelid)"
                " FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid"
                " WHERE c.relname IN ('ocr_doc_registry', 'ocr_doc_registry_tenants')"
            )
            assert len(pols) == 2  # single policy per table
            for p in pols:
                assert p["polname"] == "registry_internal_access"
                assert "app.ocr_registry" in p[2]
            superuser = await raw.fetchval(
                "SELECT rolsuper FROM pg_roles WHERE rolname = current_user"
            )

        sha = "99" * 32
        res = await registry.register_document(
            db, secret=REG_SECRET, tenant_id=TENANT_A, sha256=sha, first_seen=T0
        )
        assert res["created"] and res["membership_added"] and not res["match"]
        key = res["registry_key"]
        assert key != sha  # raw sha256 never stored
        async with db._pool.acquire() as raw:
            n = await raw.fetchval(
                "SELECT COUNT(*) FROM ocr_doc_registry WHERE registry_key = $1", key
            )
            if not superuser:
                # FORCE RLS without the GUC: ZERO rows, even for the owner
                assert n == 0
        removed = await registry.remove_tenant_memberships(db, TENANT_A)
        assert removed == {"memberships_removed": 1, "keys_dropped": 1}
        async with db._pool.acquire() as raw:
            if superuser:
                assert (
                    await raw.fetchval(
                        "SELECT COUNT(*) FROM ocr_doc_registry WHERE registry_key = $1",
                        key,
                    )
                    == 0
                )
    finally:
        await db.aclose()
