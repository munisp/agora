"""W46-F P7 regression tests (performance work order, no API change):

  * JWT public-key PEM parsed ONCE at startup (app.state.jwt_verification_key)
    and reused by RS256 verification — signature semantics unchanged;
  * best-effort scorer warm at boot: tenant scopes listed by the filesystem
    registry are resolved during startup (cache populated before the first
    request); failures degrade to rules-only (I1 preserved).

Runs WITHOUT the torch overlay (LearnedScorer.load → None is the exact
warm-path degradation) and without a live model-registry service.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import time

import pytest
from fastapi.testclient import TestClient

from credit_bureau.config import Settings
from credit_bureau.main import create_app, decode_and_verify

pytest.importorskip("cryptography", reason="cryptography not installed")


def _b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def _rs256_keypair():
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric import rsa

    private = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    pem = private.public_key().public_bytes(
        serialization.Encoding.PEM,
        serialization.PublicFormat.SubjectPublicKeyInfo,
    ).decode()
    return private, pem


def _sign_rs256(private, claims: dict) -> str:
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import padding

    header = {"alg": "RS256", "typ": "JWT"}
    signing_input = (
        _b64url(json.dumps(header).encode()) + "." + _b64url(json.dumps(claims).encode())
    )
    signature = private.sign(
        signing_input.encode(), padding.PKCS1v15(), hashes.SHA256()
    )
    return signing_input + "." + _b64url(signature)


def test_startup_parses_pem_once_and_verifies() -> None:
    private, pem = _rs256_keypair()
    settings = Settings(jwt_public_key=pem, jwt_algorithm="RS256")
    app = create_app(settings)
    # parsed at startup, not per request
    assert app.state.jwt_verification_key is not None

    client = TestClient(app)
    token = _sign_rs256(private, {"sub": "tenant-a", "exp": time.time() + 60})
    res = client.post(
        "/v1/credit/score",
        json={"signals": {"tenure_days": 120, "completed_bookings": 5, "repaid_loans": 1}},
        headers={"Authorization": f"Bearer {token}"},
    )
    assert res.status_code == 200, res.text
    assert res.json()["tenant_id"] == "tenant-a"

    # wrong signature still fails closed with the cached key
    other_private, _ = _rs256_keypair()
    bad = _sign_rs256(other_private, {"sub": "tenant-a", "exp": time.time() + 60})
    res = client.post(
        "/v1/credit/score",
        json={"signals": {}},
        headers={"Authorization": f"Bearer {bad}"},
    )
    assert res.status_code == 401


def test_decode_and_verify_accepts_parsed_key() -> None:
    private, pem = _rs256_keypair()
    from cryptography.hazmat.primitives import serialization

    parsed = serialization.load_pem_public_key(pem.encode())
    token = _sign_rs256(private, {"sub": "t", "exp": time.time() + 60})
    assert decode_and_verify(token, pem, "RS256", parsed)["sub"] == "t"
    # HS256 path ignores the parsed key (no parse needed)
    hs = (
        _b64url(json.dumps({"alg": "HS256"}).encode())
        + "."
        + _b64url(json.dumps({"sub": "h"}).encode())
    )
    sig = hmac.new(b"k", hs.encode(), hashlib.sha256).digest()
    assert decode_and_verify(hs + "." + _b64url(sig), "k", "HS256")["sub"] == "h"


def test_bad_pem_fails_closed_not_boot() -> None:
    # An unparseable PEM must not crash startup; requests 401 (legacy
    # per-request parse failure behavior preserved).
    settings = Settings(jwt_public_key="not-a-pem", jwt_algorithm="RS256")
    app = create_app(settings)
    assert app.state.jwt_verification_key is None
    client = TestClient(app)
    res = client.post(
        "/v1/credit/score",
        json={"signals": {}},
        headers={"Authorization": "Bearer a.b.c"},
    )
    assert res.status_code == 401


def test_scorer_warm_at_boot_best_effort(tmp_path) -> None:
    # Registry lists two tenant scopes (one with no artifact inside) — the
    # startup hook resolves both into the cache (None without the torch
    # overlay = the I1 degradation) BEFORE any request arrives.
    (tmp_path / "tenant-a").mkdir()
    (tmp_path / "global").mkdir()
    settings = Settings(ml_registry_dir=str(tmp_path))
    app = create_app(settings)
    with TestClient(app):  # startup event runs on enter
        cache = app.state.scorer_cache
        assert "tenant-a" in cache
        assert "global" in cache
    # endpoint still serves rules-only after the warm pass
    client = TestClient(app)
    res = client.post(
        "/v1/credit/score",
        json={"signals": {"tenure_days": 120, "completed_bookings": 5, "repaid_loans": 1}},
        headers={"X-Tenant-Id": "tenant-a"},
    )
    assert res.status_code == 200, res.text
    assert res.json()["model_version"] == "heuristic-v1"


def test_scorer_warm_disabled_without_registry() -> None:
    app = create_app(Settings(ml_registry_dir=""))
    with TestClient(app):
        assert app.state.scorer_cache == {}
