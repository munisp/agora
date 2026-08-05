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
