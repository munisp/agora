"""SPEC-W58 GATES R2/R3: TTL sweeper + consent-erasure / TenantDeleted
Dapr consumers (A5/A6). Runs against the in-memory fakes — no network."""

from __future__ import annotations

import json
import uuid
from datetime import datetime, timedelta, timezone

import pytest

from app import erasure, sweeper
from conftest import INTERNAL_TOKEN, TENANT_A, TENANT_B, make_png

EXTRACT = "/v1/ocr/extract"


def _post(client, kind, tenant=TENANT_A):
    resp = client.post(
        EXTRACT,
        files={"file": ("doc.png", b"\x89PNG\r\n\x1a\n" + bytes(64), "image/png")},
        data={"kind": kind, "tenant": tenant},
        headers={"X-Internal-Token": INTERNAL_TOKEN},
    )
    assert resp.status_code == 200, resp.text
    return resp.json()["document_id"]


# ---------------------------------------------------------------------------
# GATE R3: TTL sweeper
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_sweeper_deletes_expired_kyc_rows_and_objects(deps):
    now = datetime.now(timezone.utc)

    async def seed(kind, tenant, expires_at, status="extracted"):
        doc_id = uuid.uuid4()
        key = f"{tenant}/{doc_id}.png"
        deps.documents.objects[("documents", key)] = b"bytes"
        async with deps.db.tenant_conn(tenant) as conn:
            await deps.db.insert_document(
                conn,
                {
                    "id": doc_id,
                    "tenant_id": tenant,
                    "kind": kind,
                    "storage_bucket": "documents",
                    "storage_key": key,
                    "mime": "image/png",
                    "bytes": 5,
                    "sha256": "ab" * 32,
                    "phash": "00" * 8,
                    "expires_at": expires_at,
                },
            )
        row = deps.db.rows[str(doc_id)]
        row["status"] = status
        return doc_id, key

    expired_id, expired_key = await seed(
        "kyc_nin_slip", TENANT_A, now - timedelta(hours=1)
    )
    expired_b_id, expired_b_key = await seed(
        "kyc_passport", TENANT_B, now - timedelta(minutes=5)
    )
    fresh_id, fresh_key = await seed("kyc_nin_slip", TENANT_A, now + timedelta(hours=71))
    receipt_id, receipt_key = await seed("receipt", TENANT_A, None)
    erased_id, erased_key = await seed(
        "kyc_voters_card", TENANT_A, now - timedelta(hours=2), status="erased"
    )

    deleted = await sweeper.sweep_expired_once(deps.db, deps.objects)
    assert deleted == 2
    # expired rows + objects gone (both tenants — sweeper is cross-tenant)
    assert str(expired_id) not in deps.db.rows
    assert str(expired_b_id) not in deps.db.rows
    assert ("documents", expired_key) not in deps.documents.objects
    assert ("documents", expired_b_key) not in deps.documents.objects
    # unexpired kyc row, receipt row (no TTL), and the erased audit stub kept
    assert str(fresh_id) in deps.db.rows
    assert ("documents", fresh_key) in deps.documents.objects
    assert str(receipt_id) in deps.db.rows
    assert ("documents", receipt_key) in deps.documents.objects
    assert str(erased_id) in deps.db.rows
    assert deps.db.rows[str(erased_id)]["status"] == "erased"
    # idempotent second pass: nothing left to delete
    assert await sweeper.sweep_expired_once(deps.db, deps.objects) == 0


# ---------------------------------------------------------------------------
# GATE R2: consent ErasureRequested consumer
# ---------------------------------------------------------------------------
def _erasure_event(subject, tenant=TENANT_A):
    return {
        "specversion": "1.0",
        "id": str(uuid.uuid4()),
        "source": "identity-service",
        "type": "com.opendesk.consent.ErasureRequested",
        "subject": subject,
        "tenantid": tenant,
        "data": {
            "tenant_id": tenant,
            "data_subject_id": subject,
            "purpose": "kyc_document",
            "erased_records": 1,
            "erasure_ts": datetime.now(timezone.utc).isoformat(),
            "synthetic": False,
        },
    }


def test_consent_erasure_blanks_kyc_row_and_deletes_object(client, deps):
    doc_id = _post(client, "kyc_nin_slip")  # fixture nin = 12345678901
    row = deps.db.rows[doc_id]
    key = row["storage_key"]
    assert ("documents", key) in deps.documents.objects
    sha_before, phash_before = row["sha256"], row["phash"]

    resp = client.post(
        "/v1/ocr/events/consent-erasure", json=_erasure_event("12345678901")
    )
    assert resp.status_code == 200, resp.text
    assert resp.json()["status"] == "SUCCESS"
    assert resp.json()["erased"] == 1

    row = deps.db.rows[doc_id]
    assert row["status"] == "erased"
    assert row["extracted_fields"] is None
    assert row["extracted_text"] is None
    assert row["sha256"] == sha_before and row["phash"] == phash_before  # audit
    assert ("documents", key) not in deps.documents.objects
    # redelivery converges (idempotent: already-erased row is not matched)
    resp2 = client.post(
        "/v1/ocr/events/consent-erasure", json=_erasure_event("12345678901")
    )
    assert resp2.json()["erased"] == 0


def test_consent_erasure_is_tenant_and_kind_scoped(client, deps):
    # same fixture nin in both tenants + a receipt (never erased by this path)
    a_id = _post(client, "kyc_nin_slip", tenant=TENANT_A)
    b_id = _post(client, "kyc_nin_slip", tenant=TENANT_B)
    r_id = _post(client, "receipt", tenant=TENANT_A)

    resp = client.post(
        "/v1/ocr/events/consent-erasure", json=_erasure_event("12345678901", TENANT_A)
    )
    assert resp.json()["erased"] == 1
    assert deps.db.rows[a_id]["status"] == "erased"
    assert deps.db.rows[b_id]["status"] == "extracted"  # other tenant untouched
    assert deps.db.rows[r_id]["status"] == "extracted"  # non-kyc untouched


def test_consent_erasure_unknown_subject_is_noop(client, deps):
    _post(client, "kyc_nin_slip")
    resp = client.post(
        "/v1/ocr/events/consent-erasure", json=_erasure_event("+2348000000000")
    )
    assert resp.status_code == 200
    assert resp.json()["erased"] == 0


def test_consent_erasure_malformed_dropped(client):
    # no CloudEvent type -> DROP (not ours to process); no exception, no 5xx
    resp = client.post("/v1/ocr/events/consent-erasure", json={"data": {}})
    assert resp.status_code == 200
    assert resp.json()["status"] == "DROP"
    resp = client.post(
        "/v1/ocr/events/consent-erasure",
        json={"type": "com.opendesk.consent.SomethingElse", "data": {}},
    )
    assert resp.json()["status"] == "DROP"
    # correct type but missing subject/tenant -> SUCCESS no-op (0 erased)
    resp = client.post(
        "/v1/ocr/events/consent-erasure",
        json={"type": "com.opendesk.consent.ErasureRequested", "data": {}},
    )
    assert resp.status_code == 200
    assert resp.json()["erased"] == 0


# ---------------------------------------------------------------------------
# GATE R2: TenantDeleted consumer (hard-delete ALL kinds)
# ---------------------------------------------------------------------------
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


def test_tenant_deleted_hard_deletes_all_rows_and_objects(client, deps):
    a1 = _post(client, "kyc_nin_slip", tenant=TENANT_A)
    a2 = _post(client, "receipt", tenant=TENANT_A)  # ALL kinds per P3
    b1 = _post(client, "kyc_passport", tenant=TENANT_B)
    keys_a = [deps.db.rows[d]["storage_key"] for d in (a1, a2)]

    resp = client.post(
        "/v1/ocr/events/tenant-deleted", json=_tenant_deleted_event(TENANT_A)
    )
    assert resp.status_code == 200, resp.text
    assert resp.json()["rows"] == 2
    assert a1 not in deps.db.rows and a2 not in deps.db.rows
    for key in keys_a:
        assert ("documents", key) not in deps.documents.objects
    # tenant B untouched
    assert b1 in deps.db.rows
    # idempotent redelivery
    resp2 = client.post(
        "/v1/ocr/events/tenant-deleted", json=_tenant_deleted_event(TENANT_A)
    )
    assert resp2.json()["rows"] == 0


def test_tenant_deleted_foreign_type_dropped(client):
    resp = client.post(
        "/v1/ocr/events/tenant-deleted",
        json={"type": "com.opendesk.identity.TenantCreated", "data": {}},
    )
    assert resp.json()["status"] == "DROP"


def test_dapr_subscribe_route(client):
    resp = client.get("/dapr/subscribe")
    assert resp.status_code == 200
    subs = resp.json()
    routes = {s["topic"]: s["route"] for s in subs}
    assert routes["opendesk.consent.erasure.v1"] == "/v1/ocr/events/consent-erasure"
    assert routes["opendesk.identity.events"] == "/v1/ocr/events/tenant-deleted"
    assert all(s["pubsubname"] == "pubsub-kafka" for s in subs)


# ---------------------------------------------------------------------------
# direct handler unit shape (no HTTP)
# ---------------------------------------------------------------------------
@pytest.mark.asyncio
async def test_handle_erasure_event_direct(deps):
    doc_id = uuid.uuid4()
    async with deps.db.tenant_conn(TENANT_A) as conn:
        await deps.db.insert_document(
            conn,
            {
                "id": doc_id,
                "tenant_id": TENANT_A,
                "kind": "kyc_drivers_license",
                "storage_bucket": "documents",
                "storage_key": f"{TENANT_A}/{doc_id}.png",
                "mime": "image/png",
                "bytes": 5,
                "sha256": "cd" * 32,
                "phash": None,
            },
        )
    deps.db.rows[str(doc_id)]["extracted_fields"] = {"license_no": "FKJ-1"}
    result = await erasure.handle_erasure_event(
        deps.db, deps.objects, _erasure_event("FKJ-1")
    )
    assert result == {"matched": 1, "erased": 1}
    assert deps.db.rows[str(doc_id)]["status"] == "erased"


def test_event_payloads_carry_no_pii_constants():
    # belt-and-braces: the event type constants are the pinned contract
    from app import events

    assert events.TYPE_CONSENT_ERASURE == "com.opendesk.consent.ErasureRequested"
    assert events.TOPIC_CONSENT_ERASURE == "opendesk.consent.erasure.v1"
    assert events.TYPE_TENANT_DELETED == "com.opendesk.identity.TenantDeleted"
    assert events.TOPIC_IDENTITY_EVENTS == "opendesk.identity.events"
    # redaction unit: kyc kind => {} regardless of the fields handed in
    evt = events.document_extracted_event(
        tenant_id=TENANT_A,
        document_id=uuid.uuid4(),
        kind="kyc_passport",
        status="extracted",
        page_count=1,
        sha256="ab" * 32,
        phash=None,
        model_version="m",
        storage_bucket="documents",
        storage_key="k",
        extracted_fields={"full_name": "PII", "passport_no": "A1"},
        mime="image/png",
    )
    assert evt["data"]["extracted_fields"] == {}
    assert "PII" not in json.dumps(evt)
    # SPEC-W64: mime is not PII — it rides the event for kyc kinds too.
    assert evt["data"]["mime"] == "image/png"
