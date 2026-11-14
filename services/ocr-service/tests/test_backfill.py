"""SPEC-W66 WS1-A: POST /v1/internal/ocr/backfill-document-events.

Covers: internauth fail-closed (503/401), keyset pagination (no dup/no
gap), deterministic uuid5 ids with a RE-RUN proof (second pass emits zero
NEW ids), historical event time, dry_run emits nothing, erased/failed
excluded, receipt whitelist + {} for kyc/other kinds, tenant filter,
since/until bounds, and request validation (limit/cursor/timestamps).
"""

from __future__ import annotations

import uuid
from datetime import datetime, timedelta, timezone

from app.config import settings
from app.events import (
    TYPE_DOCUMENT_EXTRACTED,
    backfill_event_id,
    document_backfill_event,
)

from conftest import INTERNAL_TOKEN, TENANT_A, TENANT_B

ROUTE = "/v1/internal/ocr/backfill-document-events"
AUTH = {"X-Internal-Token": INTERNAL_TOKEN}

T0 = datetime(2026, 1, 5, 12, 0, 0, tzinfo=timezone.utc)


def seed(
    deps,
    tenant=TENANT_A,
    *,
    kind="receipt",
    status="extracted",
    created_at=T0,
    fields=None,
    sha256=None,
    phash=None,
    page_count=2,
    model_version="minicpm-ocr-v1",
    doc_id=None,
):
    """Insert one ocr_documents row directly (store-level fixture)."""
    doc_id = doc_id or uuid.uuid4()
    deps.db.rows[str(doc_id)] = {
        "id": doc_id,
        "tenant_id": tenant,
        "kind": kind,
        "storage_bucket": "documents",
        "storage_key": f"{tenant}/{doc_id}.png",
        "mime": "image/png",
        "bytes": 128,
        "sha256": sha256 or (uuid.uuid4().hex * 2),
        "phash": phash,
        "page_count": page_count,
        "model_version": model_version,
        "extracted_text": None,
        "extracted_fields": fields,
        "status": status,
        "error": None,
        "expires_at": None,
        "subject_phone": None,
        "created_at": created_at,
        "updated_at": created_at,
    }
    return doc_id


def backfill(client, body=None, headers=AUTH):
    return client.post(ROUTE, json=body or {}, headers=headers)


def run_all_pages(client, body=None):
    """Drive the keyset loop to completion; return (pages, all events)."""
    body = dict(body or {})
    pages = []
    while True:
        resp = backfill(client, body)
        assert resp.status_code == 200, resp.text
        page = resp.json()
        pages.append(page)
        if page["next_cursor"] is None:
            return pages
        assert len(pages) < 50, "pagination did not terminate"
        body["cursor"] = page["next_cursor"]


def doc_events(deps):
    return [
        payload
        for _pub, topic, payload in deps.dapr.published
        if topic == settings.ocr_events_topic
    ]


# ---------------------------------------------------------------------------
# auth (erase-endpoint idiom: fail-closed)
# ---------------------------------------------------------------------------
def test_backfill_503_when_token_unconfigured(client, deps, monkeypatch):
    seed(deps)
    monkeypatch.setattr(settings, "internal_token", "")
    resp = backfill(client, headers={"X-Internal-Token": "anything"})
    assert resp.status_code == 503
    assert deps.dapr.published == []


def test_backfill_401_wrong_token(client, deps):
    seed(deps)
    resp = backfill(client, headers={"X-Internal-Token": "wrong"})
    assert resp.status_code == 401
    assert deps.dapr.published == []


def test_backfill_401_missing_token(client, deps):
    seed(deps)
    resp = client.post(ROUTE, json={})
    assert resp.status_code == 401
    assert deps.dapr.published == []


# ---------------------------------------------------------------------------
# emission shape: deterministic ids, historical time, hash-only payload
# ---------------------------------------------------------------------------
def test_backfill_emits_deterministic_ids_and_historical_time(client, deps):
    created = T0 - timedelta(days=40)
    doc_id = seed(deps, created_at=created)
    resp = backfill(client)
    assert resp.status_code == 200
    body = resp.json()
    assert body["emitted"] == 1 and body["scanned"] == 1 and body["skipped"] == 0
    assert body["next_cursor"] is None
    (event,) = doc_events(deps)
    assert event["type"] == TYPE_DOCUMENT_EXTRACTED
    # Deterministic id: uuid5(URL namespace, "ocr-backfill:{document_id}").
    assert event["id"] == str(uuid.uuid5(uuid.NAMESPACE_URL, f"ocr-backfill:{doc_id}"))
    assert event["id"] == str(backfill_event_id(doc_id))
    # Historical event time = document created_at, NOT the backfill time.
    assert event["time"] == created.isoformat()
    assert event["subject"] == str(doc_id)
    assert event["tenantid"] == TENANT_A
    # Hash-only pinned set: no storage pointers, no text, no mime.
    data = event["data"]
    assert data["document_id"] == str(doc_id)
    assert data["tenant_id"] == TENANT_A
    assert data["status"] == "extracted"
    for forbidden in ("storage_bucket", "storage_key", "extracted_text", "mime"):
        assert forbidden not in data


def test_backfill_rerun_emits_zero_new_event_ids(client, deps):
    """G4 re-run proof: a second full pass over the same window re-emits
    the SAME deterministic ids — zero NEW events downstream."""
    ids = [seed(deps, created_at=T0 + timedelta(hours=i)) for i in range(5)]
    pages1 = run_all_pages(client, {"limit": 2})
    first_ids = {e["id"] for e in doc_events(deps)}
    assert first_ids == {str(backfill_event_id(i)) for i in ids}
    assert sum(p["emitted"] for p in pages1) == 5
    # Second run over the identical window.
    pages2 = run_all_pages(client, {"limit": 2})
    second_ids = {e["id"] for e in doc_events(deps)}
    assert second_ids == first_ids, "re-run must not mint new event ids"


def test_backfill_keyset_pagination_no_dup_no_gap(client, deps):
    wanted = [
        seed(deps, created_at=T0 + timedelta(hours=i), doc_id=uuid.UUID(int=i + 1))
        for i in range(7)
    ]
    pages = run_all_pages(client, {"limit": 3})
    assert [p["scanned"] for p in pages] == [3, 3, 1]
    assert all(p["emitted"] == p["scanned"] and p["skipped"] == 0 for p in pages)
    assert pages[-1]["next_cursor"] is None
    seen = [e["data"]["document_id"] for e in doc_events(deps)]
    assert len(seen) == len(set(seen)) == 7, "no duplicates, no gaps"
    assert set(seen) == {str(i) for i in wanted}
    # Global (created_at, id) ordering held across pages.
    assert seen == sorted(seen, key=lambda s: wanted.index(uuid.UUID(s)))


def test_backfill_dry_run_emits_nothing(client, deps):
    for i in range(3):
        seed(deps, created_at=T0 + timedelta(hours=i))
    resp = backfill(client, {"dry_run": True})
    assert resp.status_code == 200
    body = resp.json()
    assert body["emitted"] == 0
    assert body["scanned"] == 3
    assert body["skipped"] == 3  # counted, never published
    assert deps.dapr.published == [], "dry_run must publish NOTHING"
    # And a dry_run still paginates honestly.
    pages = run_all_pages(client, {"dry_run": True, "limit": 2})
    assert [p["scanned"] for p in pages] == [2, 1]
    assert deps.dapr.published == []


def test_backfill_excludes_erased_failed_uploaded(client, deps):
    keep = seed(deps, status="extracted")
    seed(deps, status="failed", created_at=T0 + timedelta(hours=1))
    seed(deps, status="erased", created_at=T0 + timedelta(hours=2))
    seed(deps, status="uploaded", created_at=T0 + timedelta(hours=3))
    resp = backfill(client)
    assert resp.status_code == 200
    assert resp.json()["emitted"] == 1
    (event,) = doc_events(deps)
    assert event["data"]["document_id"] == str(keep)


def test_backfill_receipt_whitelist_and_kyc_redaction(client, deps):
    seed(
        deps,
        kind="receipt",
        fields={
            "amount_minor": 12500,
            "currency": "NGN",
            "receipt_date": "2026-01-02",
            "vendor": "ACME Stores",  # NOT whitelisted — must be dropped
            "customer_phone": "+2348012345678",  # PII — must be dropped
        },
    )
    seed(
        deps,
        kind="kyc_nin_slip",
        created_at=T0 + timedelta(hours=1),
        fields={"full_name": "Ada Lovelace", "nin": "12345678901"},
    )
    seed(
        deps,
        kind="document",
        created_at=T0 + timedelta(hours=2),
        fields={"title": "contract"},
    )
    resp = backfill(client)
    assert resp.status_code == 200
    assert resp.json()["emitted"] == 3
    by_kind = {e["data"]["kind"]: e["data"]["extracted_fields"] for e in doc_events(deps)}
    # EXACTLY the three whitelisted scalars for receipts.
    assert by_kind["receipt"] == {
        "amount_minor": 12500,
        "currency": "NGN",
        "receipt_date": "2026-01-02",
    }
    # {} for every other kind (stricter than the live path).
    assert by_kind["kyc_nin_slip"] == {}
    assert by_kind["document"] == {}


def test_backfill_tenant_filter(client, deps):
    a = seed(deps, TENANT_A)
    seed(deps, TENANT_B, created_at=T0 + timedelta(hours=1))
    resp = backfill(client, {"tenant_id": TENANT_A})
    assert resp.status_code == 200
    assert resp.json()["emitted"] == 1
    (event,) = doc_events(deps)
    assert event["tenantid"] == TENANT_A
    assert event["data"]["document_id"] == str(a)


def test_backfill_since_until_bounds(client, deps):
    seed(deps, created_at=T0)  # before since
    mid = seed(deps, created_at=T0 + timedelta(hours=12))
    seed(deps, created_at=T0 + timedelta(hours=48))  # after until
    resp = backfill(
        client,
        {
            "since": (T0 + timedelta(hours=1)).isoformat(),
            "until": (T0 + timedelta(hours=24)).isoformat(),
        },
    )
    assert resp.status_code == 200
    assert resp.json()["emitted"] == 1
    (event,) = doc_events(deps)
    assert event["data"]["document_id"] == str(mid)
    # Bounds are inclusive.
    resp = backfill(client, {"since": T0.isoformat(), "until": T0.isoformat()})
    assert resp.json()["emitted"] == 1


def test_backfill_cross_tenant_merge_ordering(client, deps):
    """Rows interleaved across tenants still paginate in global
    (created_at, id) order with no dup/no gap."""
    ids = []
    for i in range(6):
        tenant = TENANT_A if i % 2 == 0 else TENANT_B
        ids.append(
            seed(deps, tenant, created_at=T0 + timedelta(hours=i), doc_id=uuid.UUID(int=i + 1))
        )
    pages = run_all_pages(client, {"limit": 2})
    assert [p["scanned"] for p in pages] == [2, 2, 2]
    seen = [e["data"]["document_id"] for e in doc_events(deps)]
    assert seen == [str(i) for i in ids], "global created_at order across tenants"


# ---------------------------------------------------------------------------
# request validation
# ---------------------------------------------------------------------------
def test_backfill_limit_clamped_and_validated(client, deps):
    for i in range(3):
        seed(deps, created_at=T0 + timedelta(hours=i))
    resp = backfill(client, {"limit": 99999})  # clamped to 5000, not an error
    assert resp.status_code == 200
    assert resp.json()["emitted"] == 3
    assert backfill(client, {"limit": 0}).status_code == 400
    assert backfill(client, {"limit": -5}).status_code == 400
    assert backfill(client, {"limit": True}).status_code == 400
    assert backfill(client, {"limit": "10"}).status_code == 400


def test_backfill_bad_cursor_400(client, deps):
    seed(deps)
    assert backfill(client, {"cursor": "not-a-cursor"}).status_code == 400
    assert backfill(client, {"cursor": "2026-01-01T00:00:00|nope"}).status_code == 400
    assert backfill(client, {"cursor": 123}).status_code == 400


def test_backfill_bad_window_400(client, deps):
    seed(deps)
    assert backfill(client, {"since": "yesterday"}).status_code == 400
    assert backfill(client, {"since": "2026-01-01 12:00:00"}).status_code == 400  # naive
    assert backfill(client, {"until": 123}).status_code == 400
    resp = backfill(
        client,
        {"since": T0.isoformat(), "until": (T0 - timedelta(hours=1)).isoformat()},
    )
    assert resp.status_code == 400
    assert backfill(client, {"tenant_id": "not-a-uuid"}).status_code == 400


def test_backfill_empty_body_and_empty_store(client, deps):
    resp = client.post(ROUTE, headers=AUTH)  # no JSON body at all
    assert resp.status_code == 200
    assert resp.json() == {"emitted": 0, "scanned": 0, "skipped": 0, "next_cursor": None}


# ---------------------------------------------------------------------------
# builder unit tests (no HTTP)
# ---------------------------------------------------------------------------
def test_document_backfill_event_parses_jsonb_text():
    doc_id = uuid.uuid4()
    event = document_backfill_event(
        tenant_id=TENANT_A,
        document_id=doc_id,
        kind="receipt",
        status="extracted",
        page_count=1,
        sha256="ab" * 32,
        phash=None,
        model_version="v1",
        created_at=T0,
        # asyncpg returns jsonb as text when no codec is registered.
        extracted_fields='{"amount_minor": 500, "vendor": "DROP-ME"}',
    )
    assert event["data"]["extracted_fields"] == {"amount_minor": 500}
    assert event["time"] == T0.isoformat()


def test_document_backfill_event_bad_json_and_missing_keys():
    event = document_backfill_event(
        tenant_id=TENANT_A,
        document_id=uuid.uuid4(),
        kind="receipt",
        status="extracted",
        page_count=1,
        sha256="ab" * 32,
        phash=None,
        model_version="v1",
        created_at=T0,
        extracted_fields="{not json",
    )
    assert event["data"]["extracted_fields"] == {}
    event2 = document_backfill_event(
        tenant_id=TENANT_A,
        document_id=uuid.uuid4(),
        kind="receipt",
        status="extracted",
        page_count=1,
        sha256="ab" * 32,
        phash=None,
        model_version="v1",
        created_at=T0,
        extracted_fields={"currency": "NGN"},  # partial whitelist present
    )
    assert event2["data"]["extracted_fields"] == {"currency": "NGN"}


def test_cloud_event_legacy_callers_byte_identical():
    """Pre-W66 callers (no event_time/event_id) keep random ids + now time."""
    from app.events import cloud_event

    legacy = cloud_event("t", subject="s", tenant_id=TENANT_A, data={})
    assert uuid.UUID(legacy["id"]).version == 4
    pinned = cloud_event(
        "t",
        subject="s",
        tenant_id=TENANT_A,
        data={},
        event_id=backfill_event_id("doc-1"),
        event_time=T0,
    )
    assert uuid.UUID(pinned["id"]).version == 5
    assert pinned["time"] == T0.isoformat()
