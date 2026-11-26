"""SPEC-W115 C4/C5/C6: caller document intake — fail-closed config, upload
proxy, share-link token round-trip, session correlation, verdict summary.
"""

from __future__ import annotations

import time
import uuid
from types import SimpleNamespace

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app.documents import (
    LinkTokenError,
    mint_document_link,
    verdict_summary,
    verify_document_link,
    wire_document_routes,
)
from app.session_state import SessionState, SessionStore

TENANT = "11111111-2222-3333-4444-555555555555"
PNG = (
    b"\x89PNG\r\n\x1a\n"
    b"\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00"
    b"\x90wS\xde"
)


def _settings(**over):
    base = dict(
        ocr_service_url="http://ocr:7020",
        ocr_internal_token="tok",
        ocr_timeout_s=5.0,
        document_link_secret="link-secret",
        document_link_ttl_s=900,
        max_document_bytes=1024 * 1024,
    )
    base.update(over)
    return SimpleNamespace(**base)


class FakeOcr:
    def __init__(self):
        self.calls = []

    async def extract(self, **kw):
        self.calls.append(kw)
        return {
            "document_id": str(uuid.uuid4()),
            "status": "extracted",
            "validation_verdict": "validated",
            "validation_reasons": [],
        }


async def _resolver(site_slug: str) -> str:
    return TENANT


def _app(settings, ocr=None, sessions=None):
    app = FastAPI()
    wire_document_routes(
        app,
        settings=settings,
        sessions=sessions if sessions is not None else SessionStore(),
        ocr_client=ocr if ocr is not None else FakeOcr(),
        tenant_resolver=_resolver,
    )
    return app


# --- fail-closed config (C4) ------------------------------------------------
def test_unconfigured_fails_closed():
    client = TestClient(_app(_settings(ocr_service_url="", ocr_internal_token="")))
    resp = client.post(
        "/voice/documents/upload",
        files={"file": ("a.png", PNG, "image/png")},
        data={"kind": "pay_stub", "site_slug": "acme"},
    )
    assert resp.status_code == 503
    # link minting also fails closed
    store = SessionStore()
    session = store.get_or_create(None, site_slug="acme")
    app = _app(_settings(document_link_secret=""), sessions=store)
    resp = TestClient(app).post(
        f"/voice/sessions/{session.conversation_id}/document-link",
        json={"session_secret": session.session_secret},
    )
    assert resp.status_code == 503


# --- direct upload (web chat) ----------------------------------------------
def test_upload_happy_path_forwards_to_ocr():
    ocr = FakeOcr()
    client = TestClient(_app(_settings(), ocr=ocr))
    resp = client.post(
        "/voice/documents/upload",
        files={"file": ("stub.png", PNG, "image/png")},
        data={"kind": "pay_stub", "site_slug": "acme"},
    )
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["validation_verdict"] == "validated"
    assert "passed all consistency checks" in body["summary"]
    call = ocr.calls[0]
    assert call["tenant_id"] == TENANT
    assert call["kind"] == "pay_stub"
    assert call["source_channel"] == "web"


def test_upload_accepts_all_non_kyc_kinds():
    # SPEC-W116 §4: pay stub was the example — every non-KYC kind is
    # caller-shareable now.
    ocr = FakeOcr()
    client = TestClient(_app(_settings(), ocr=ocr))
    for kind in ("pay_stub", "receipt", "document"):
        resp = client.post(
            "/voice/documents/upload",
            files={"file": ("doc.png", PNG, "image/png")},
            data={"kind": kind, "site_slug": "acme"},
        )
        assert resp.status_code == 200, (kind, resp.text)
    assert [c["kind"] for c in ocr.calls] == ["pay_stub", "receipt", "document"]


def test_upload_rejects_oversize_and_bad_mime_and_kind():
    client = TestClient(_app(_settings(max_document_bytes=8)))
    big = PNG + b"\x00" * 64
    resp = client.post(
        "/voice/documents/upload",
        files={"file": ("big.png", big, "image/png")},
        data={"kind": "pay_stub", "site_slug": "acme"},
    )
    assert resp.status_code == 413
    client = TestClient(_app(_settings()))
    resp = client.post(
        "/voice/documents/upload",
        files={"file": ("a.gif", b"GIF89a" + b"\x00" * 32, "image/gif")},
        data={"kind": "pay_stub", "site_slug": "acme"},
    )
    assert resp.status_code == 415
    resp = client.post(
        "/voice/documents/upload",
        files={"file": ("a.png", PNG, "image/png")},
        data={"kind": "kyc_passport", "site_slug": "acme"},
    )
    assert resp.status_code == 400


def test_upload_attaches_to_session_when_session_id_given():
    store = SessionStore()
    session = store.get_or_create(None, site_slug="acme")
    client = TestClient(_app(_settings(), sessions=store))
    resp = client.post(
        "/voice/documents/upload",
        files={"file": ("stub.png", PNG, "image/png")},
        data={"kind": "pay_stub", "site_slug": "acme",
              "session_id": session.conversation_id},
    )
    assert resp.status_code == 200
    docs = session.shared_documents
    assert len(docs) == 1
    assert docs[0]["channel"] == "web"
    assert docs[0]["kind"] == "pay_stub"
    assert docs[0]["validation_verdict"] == "validated"


# --- share links (voice mid-call) ------------------------------------------
def test_mint_then_claim_roundtrip():
    store = SessionStore()
    session = store.get_or_create(None, site_slug="acme")
    ocr = FakeOcr()
    client = TestClient(_app(_settings(), ocr=ocr, sessions=store))
    mint = client.post(
        f"/voice/sessions/{session.conversation_id}/document-link",
        json={"session_secret": session.session_secret},
    )
    assert mint.status_code == 200, mint.text
    url = mint.json()["upload_url"]
    claim = client.post(
        url,
        files={"file": ("stub.png", PNG, "image/png")},
        data={"kind": "pay_stub"},
    )
    assert claim.status_code == 200, claim.text
    assert ocr.calls[0]["source_channel"] == "voice"
    assert ocr.calls[0]["tenant_id"] == TENANT
    docs = session.shared_documents
    assert len(docs) == 1 and docs[0]["channel"] == "voice"


def test_mint_requires_session_secret_and_known_session():
    store = SessionStore()
    session = store.get_or_create(None, site_slug="acme")
    client = TestClient(_app(_settings(), sessions=store))
    resp = client.post(
        f"/voice/sessions/{session.conversation_id}/document-link",
        json={"session_secret": "wrong"},
    )
    assert resp.status_code == 401
    resp = client.post(
        f"/voice/sessions/{uuid.uuid4()}/document-link",
        json={"session_secret": "whatever"},
    )
    assert resp.status_code == 404


def test_claim_rejects_expired_and_tampered_tokens():
    token, _ = mint_document_link(
        secret="s", session_id="x", tenant_id=TENANT, ttl_s=60, now=1000.0
    )
    with pytest.raises(LinkTokenError):
        verify_document_link(secret="s", token=token, now=1000.0 + 61)
    raw, sig = token.split(".", 1)
    tampered = raw + "." + ("0" if sig[0] != "0" else "1") + sig[1:]
    with pytest.raises(LinkTokenError):
        verify_document_link(secret="s", token=tampered, now=1000.0)
    with pytest.raises(LinkTokenError):
        verify_document_link(secret="other", token=token, now=1000.0)
    # happy verify
    payload = verify_document_link(secret="s", token=token, now=1000.0)
    assert payload["sid"] == "x" and payload["t"] == TENANT


def test_claim_unknown_session_404():
    store = SessionStore()
    settings = _settings()
    client = TestClient(_app(settings, sessions=store))
    token, _ = mint_document_link(
        secret=settings.document_link_secret,
        session_id=str(uuid.uuid4()),
        tenant_id=TENANT,
        ttl_s=900,
    )
    resp = client.post(
        f"/voice/documents/claim/{token}",
        files={"file": ("stub.png", PNG, "image/png")},
        data={"kind": "pay_stub"},
    )
    assert resp.status_code == 404


def test_claim_bad_token_401():
    client = TestClient(_app(_settings()))
    resp = client.post(
        "/voice/documents/claim/garbage.token",
        files={"file": ("stub.png", PNG, "image/png")},
        data={"kind": "pay_stub"},
    )
    assert resp.status_code == 401


# --- session document listing ----------------------------------------------
def test_list_session_documents_secret_guarded():
    store = SessionStore()
    session = store.get_or_create(None, site_slug="acme")
    session.shared_documents.append({"document_id": "d1", "kind": "pay_stub"})
    client = TestClient(_app(_settings(), sessions=store))
    resp = client.get(f"/voice/sessions/{session.conversation_id}/documents")
    assert resp.status_code == 401
    resp = client.get(
        f"/voice/sessions/{session.conversation_id}/documents",
        headers={"X-Session-Secret": session.session_secret},
    )
    assert resp.status_code == 200
    assert resp.json()["documents"][0]["document_id"] == "d1"


# --- SessionState round-trip (C6) ------------------------------------------
def test_session_state_shared_documents_roundtrip():
    session = SessionState(conversation_id="c1", site_slug="acme")
    session.shared_documents.append(
        {"document_id": "d1", "kind": "pay_stub", "validation_verdict": "validated"}
    )
    restored = SessionState.from_dict(session.to_dict())
    assert restored is not None
    assert restored.shared_documents[0]["document_id"] == "d1"


def test_session_state_from_dict_tolerates_absent_and_junk():
    session = SessionState(conversation_id="c1", site_slug="acme")
    envelope = session.to_dict()
    del envelope["shared_documents"]  # pre-W115 envelope
    restored = SessionState.from_dict(envelope)
    assert restored is not None and restored.shared_documents == []
    envelope["shared_documents"] = ["junk", {"document_id": "d2"}, 42]
    restored = SessionState.from_dict(envelope)
    assert [d["document_id"] for d in restored.shared_documents] == ["d2"]


# --- verdict summary (pure) -------------------------------------------------
def test_verdict_summary_sentences():
    assert "passed all consistency checks" in verdict_summary("pay_stub", "validated", [])
    rejected = verdict_summary("pay_stub", "rejected", ["net_exceeds_gross"])
    assert "could not accept" in rejected and "net pay" in rejected
    review = verdict_summary("document", "needs_review", ["no_validator_for_kind"])
    assert "team member will review" in review
    missing = verdict_summary("pay_stub", "rejected", ["missing_required_field:employer_name"])
    assert "employer name is missing" in missing


# --- async intake (2026-11-26): accept at once, read in the background --------
class SlowFailOcr:
    async def extract(self, **kw):
        raise RuntimeError("model timed out")


def _poll(client, job_id, tries=50):
    for _ in range(tries):
        r = client.get(f"/voice/documents/jobs/{job_id}")
        assert r.status_code == 200, r.text
        if r.json()["status"] != "processing":
            return r.json()
        time.sleep(0.02)
    raise AssertionError("job never finished")


def test_async_upload_accepts_then_reports_the_verdict():
    store = SessionStore()
    session = store.get_or_create(None, site_slug="acme")
    ocr = FakeOcr()
    with TestClient(_app(_settings(), ocr=ocr, sessions=store)) as client:
        resp = client.post(
            "/voice/documents/upload",
            files={"file": ("stub.png", PNG, "image/png")},
            data={"kind": "pay_stub", "site_slug": "acme", "respond": "async",
                  "session_id": session.conversation_id},
        )
        assert resp.status_code == 202, resp.text
        body = resp.json()
        assert body["status"] == "processing" and body["job_id"] and "checking your document" in body["summary"]
        done = _poll(client, body["job_id"])
    assert done["status"] == "done" and done["validation_verdict"] == "validated"
    assert "passed all consistency checks" in done["summary"]
    assert ocr.calls[0]["source_channel"] == "web"
    docs = session.shared_documents
    assert len(docs) == 1 and docs[0]["job_id"] == body["job_id"]
    assert docs[0]["status"] == "done" and docs[0]["validation_verdict"] == "validated"


def test_async_upload_failure_is_reported_not_raised():
    with TestClient(_app(_settings(), ocr=SlowFailOcr())) as client:
        resp = client.post(
            "/voice/documents/upload",
            files={"file": ("stub.png", PNG, "image/png")},
            data={"kind": "pay_stub", "site_slug": "acme", "respond": "async"},
        )
        assert resp.status_code == 202
        done = _poll(client, resp.json()["job_id"])
    assert done["status"] == "failed" and "couldn't read" in done["summary"]


def test_async_claim_link_and_unknown_job():
    store = SessionStore()
    session = store.get_or_create(None, site_slug="acme")
    ocr = FakeOcr()
    with TestClient(_app(_settings(), ocr=ocr, sessions=store)) as client:
        url = client.post(f"/voice/sessions/{session.conversation_id}/document-link",
                          json={"session_secret": session.session_secret}).json()["upload_url"]
        resp = client.post(url, files={"file": ("stub.png", PNG, "image/png")},
                           data={"kind": "pay_stub", "respond": "async"})
        assert resp.status_code == 202, resp.text
        done = _poll(client, resp.json()["job_id"])
        assert client.get("/voice/documents/jobs/" + "0" * 32).status_code == 404
    assert done["status"] == "done" and ocr.calls[0]["source_channel"] == "voice"
    assert session.shared_documents[0]["channel"] == "voice"
