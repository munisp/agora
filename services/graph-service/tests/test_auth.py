"""Auth seam tests: JWT sub -> tenant; dev-mode X-Tenant-Id fallback."""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import time

from conftest import HDR_A, build_graph
from app.backend import InMemoryBackend
from app.config import Settings
from app.main import create_app
from app.store import SegmentStore
from fastapi.testclient import TestClient

from conftest import StubLLM


def _hs256_token(claims: dict, secret: str) -> str:
    def enc(obj) -> str:
        raw = json.dumps(obj, separators=(",", ":")).encode()
        return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()

    header = enc({"alg": "HS256", "typ": "JWT"})
    payload = enc(claims)
    signing = f"{header}.{payload}"
    sig = hmac.new(secret.encode(), signing.encode(), hashlib.sha256).digest()
    return f"{signing}.{base64.urlsafe_b64encode(sig).rstrip(b'=').decode()}"


def _jwt_client(tmp_path, secret="test-secret"):
    settings = Settings(
        graph_backend="memory",
        segment_store_dir=str(tmp_path / "store"),
        jwt_public_key=secret,
        jwt_algorithm="HS256",
    )
    app = create_app(
        settings,
        backend=InMemoryBackend(build_graph()),
        llm=StubLLM(),
        store=SegmentStore(str(tmp_path / "seg")),
    )
    return TestClient(app), settings


def test_dev_mode_requires_tenant_header(client):
    resp = client.get("/v1/graph/segments")
    assert resp.status_code == 401


def test_dev_mode_bearer_rejected_when_no_key_configured(client):
    token = _hs256_token({"sub": "tenant-a"}, "whatever")
    resp = client.get("/v1/graph/segments", headers={"Authorization": f"Bearer {token}"})
    assert resp.status_code == 401


def test_jwt_mode_valid_token_maps_sub_to_tenant(tmp_path):
    jwt_client, _ = _jwt_client(tmp_path)
    token = _hs256_token({"sub": "tenant-a", "exp": time.time() + 300}, "test-secret")
    resp = jwt_client.get("/v1/graph/segments", headers={"Authorization": f"Bearer {token}"})
    assert resp.status_code == 200
    assert resp.json() == {"segments": []}


def test_jwt_mode_bad_signature_rejected(tmp_path):
    jwt_client, _ = _jwt_client(tmp_path)
    token = _hs256_token({"sub": "tenant-a"}, "wrong-secret")
    resp = jwt_client.get("/v1/graph/segments", headers={"Authorization": f"Bearer {token}"})
    assert resp.status_code == 401


def test_jwt_mode_expired_rejected(tmp_path):
    jwt_client, _ = _jwt_client(tmp_path)
    token = _hs256_token({"sub": "tenant-a", "exp": time.time() - 10}, "test-secret")
    resp = jwt_client.get("/v1/graph/segments", headers={"Authorization": f"Bearer {token}"})
    assert resp.status_code == 401


def test_jwt_mode_missing_sub_rejected(tmp_path):
    jwt_client, _ = _jwt_client(tmp_path)
    token = _hs256_token({"exp": time.time() + 300}, "test-secret")
    resp = jwt_client.get("/v1/graph/segments", headers={"Authorization": f"Bearer {token}"})
    assert resp.status_code == 401


def test_jwt_mode_header_fallback_disabled(tmp_path):
    # When a public key IS configured the X-Tenant-Id dev fallback is off.
    jwt_client, _ = _jwt_client(tmp_path)
    resp = jwt_client.get("/v1/graph/segments", headers=HDR_A)
    assert resp.status_code == 401


def test_healthz_metrics_unauthenticated(client):
    assert client.get("/healthz").status_code == 200
    assert client.get("/metrics").status_code == 200


# ---------------------------------------------------------- SPEC-W43 Y-07
def _jwt_client_iap(tmp_path, secret="test-secret", issuer="", audience=""):
    settings = Settings(
        graph_backend="memory",
        segment_store_dir=str(tmp_path / "store"),
        jwt_public_key=secret,
        jwt_algorithm="HS256",
        jwt_issuer=issuer,
        jwt_audience=audience,
    )
    app = create_app(
        settings,
        backend=InMemoryBackend(build_graph()),
        llm=StubLLM(),
        store=SegmentStore(str(tmp_path / "seg")),
    )
    return TestClient(app)


def test_jwt_without_exp_rejected_401(tmp_path):
    """exp is REQUIRED — an expiry-less token never authenticates."""
    jwt_client = _jwt_client_iap(tmp_path)
    token = _hs256_token({"sub": "tenant-a"}, "test-secret")  # no exp claim
    resp = jwt_client.get("/v1/graph/segments",
                          headers={"Authorization": f"Bearer {token}"})
    assert resp.status_code == 401
    assert "exp" in resp.json()["detail"]


def test_jwt_malformed_exp_rejected(tmp_path):
    jwt_client = _jwt_client_iap(tmp_path)
    token = _hs256_token({"sub": "tenant-a", "exp": "not-a-number"},
                         "test-secret")
    resp = jwt_client.get("/v1/graph/segments",
                          headers={"Authorization": f"Bearer {token}"})
    assert resp.status_code == 401


def test_jwt_issuer_validated_when_configured(tmp_path):
    client_ok = _jwt_client_iap(tmp_path / "a", issuer="opendesk-idp")
    good = _hs256_token({"sub": "tenant-a", "exp": time.time() + 300,
                         "iss": "opendesk-idp"}, "test-secret")
    assert client_ok.get("/v1/graph/segments",
                         headers={"Authorization": f"Bearer {good}"}
                         ).status_code == 200
    bad = _hs256_token({"sub": "tenant-a", "exp": time.time() + 300,
                        "iss": "someone-else"}, "test-secret")
    assert client_ok.get("/v1/graph/segments",
                         headers={"Authorization": f"Bearer {bad}"}
                         ).status_code == 401
    # no iss claim at all -> mismatch when issuer is configured
    none_iss = _hs256_token({"sub": "tenant-a", "exp": time.time() + 300},
                            "test-secret")
    assert client_ok.get("/v1/graph/segments",
                         headers={"Authorization": f"Bearer {none_iss}"}
                         ).status_code == 401


def test_jwt_audience_validated_when_configured(tmp_path):
    client_ok = _jwt_client_iap(tmp_path / "b", audience="graph-service")
    good_str = _hs256_token({"sub": "t", "exp": time.time() + 300,
                             "aud": "graph-service"}, "test-secret")
    assert client_ok.get("/v1/graph/segments",
                         headers={"Authorization": f"Bearer {good_str}"}
                         ).status_code == 200
    good_list = _hs256_token({"sub": "t", "exp": time.time() + 300,
                              "aud": ["other", "graph-service"]}, "test-secret")
    assert client_ok.get("/v1/graph/segments",
                         headers={"Authorization": f"Bearer {good_list}"}
                         ).status_code == 200
    bad = _hs256_token({"sub": "t", "exp": time.time() + 300,
                        "aud": "billing-engine"}, "test-secret")
    assert client_ok.get("/v1/graph/segments",
                         headers={"Authorization": f"Bearer {bad}"}
                         ).status_code == 401
    missing = _hs256_token({"sub": "t", "exp": time.time() + 300},
                           "test-secret")
    assert client_ok.get("/v1/graph/segments",
                         headers={"Authorization": f"Bearer {missing}"}
                         ).status_code == 401


def test_jwt_iss_aud_not_enforced_when_unset(tmp_path):
    jwt_client = _jwt_client_iap(tmp_path / "c")  # issuer/audience unset
    token = _hs256_token({"sub": "tenant-a", "exp": time.time() + 300,
                          "iss": "anything", "aud": "whatever"}, "test-secret")
    assert jwt_client.get("/v1/graph/segments",
                          headers={"Authorization": f"Bearer {token}"}
                          ).status_code == 200
