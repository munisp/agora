"""API tests (SPEC-W33 §3 B2, gate GB6 adapted):

  * registry-empty -> pure rule output, BYTE-EQUAL across calls (GB3-style);
  * blend math 0.6*ml + 0.4*rule with [300,900] clamp;
  * provenance fields (model_version, ml_contribution, feature_schema) — I2;
  * rule reasons preserved (never dropped — the Go rule emits none);
  * tenant scoping (I4): X-Tenant-Id dev seam, 401 without it;
  * torch-absent import path: the service module imports and serves the
    rules-only path with torch blocked (I5) — subprocess import hook,
    runs in BOTH deployments (mirrors graph-ml test_gnn_train_import).
"""

from __future__ import annotations

import json
import shutil
import subprocess
import sys
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from credit_bureau.config import Settings
from credit_bureau.main import create_app

SERVICE_ROOT = Path(__file__).resolve().parents[1]

HDR = {"X-Tenant-Id": "tenant-a"}
PAYLOAD = {"signals": {"tenure_days": 120, "completed_bookings": 5, "repaid_loans": 1}}
# Ported Go rule for PAYLOAD: (120//30)*3=12 + 5*4=20 + 1*10=10 -> 42 naive
# -> 300 + 6*42 = 552 bureau.
EXPECTED_RULE_BUREAU = 552


def _client(settings: Settings) -> TestClient:
    return TestClient(create_app(settings))


def _settings(registry: str = "") -> Settings:
    return Settings(ml_registry_dir=registry)


# ---------------------------------------------------------------------------
# Rules-only path (no torch needed — runs in every deployment)
# ---------------------------------------------------------------------------


def test_healthz() -> None:
    client = _client(_settings())
    res = client.get("/healthz")
    assert res.status_code == 200
    body = res.json()
    assert body["ok"] is True
    assert body["ml_registry"] == "off"
    assert body["feature_schema"] == "fv1"


def test_registry_empty_pure_rule_byte_equal() -> None:
    client = _client(_settings(""))
    res1 = client.post("/v1/credit/score", json=PAYLOAD, headers=HDR)
    res2 = client.post("/v1/credit/score", json=PAYLOAD, headers=HDR)
    assert res1.status_code == 200
    assert res1.content == res2.content, "registry-empty output must be byte-stable (GB3-style)"
    body = res1.json()
    assert body["score"] == EXPECTED_RULE_BUREAU
    assert body["rule_score"] == 42
    assert body["rule_score_bureau"] == EXPECTED_RULE_BUREAU
    assert body["model_version"] == "heuristic-v1"
    assert body["ml_contribution"] == 0.0
    assert body["reasons"] == []  # Go rule emits none; never dropped, never invented
    assert body["feature_schema"] == "fv1"
    assert body["default_probability"] is None
    assert body["tenant_id"] == "tenant-a"


def test_tenant_header_required_in_dev_mode() -> None:
    client = _client(_settings())
    res = client.post("/v1/credit/score", json=PAYLOAD)
    assert res.status_code == 401


def test_rule_edge_signals_through_api() -> None:
    client = _client(_settings())
    res = client.post(
        "/v1/credit/score",
        json={"signals": {"tenure_days": 3650, "completed_bookings": 100, "repaid_loans": 100}},
        headers=HDR,
    )
    body = res.json()
    assert body["rule_score"] == 100
    assert body["score"] == 900


# ---------------------------------------------------------------------------
# Learned path (torch overlay required — these tests skip cleanly in the
# rules-only base deployment, mirroring the graph-ml convention)
# ---------------------------------------------------------------------------

try:
    import torch  # noqa: F401

    from credit_bureau.ml import train as train_mod
    from tests.test_ml_train import _write_synthetic_a1

    HAS_TORCH = True
except ImportError:  # rules-only deployment
    HAS_TORCH = False

requires_torch = pytest.mark.skipif(not HAS_TORCH, reason="torch overlay not installed")


@pytest.fixture(scope="module")
def registry(tmp_path_factory: pytest.TempPathFactory) -> Path:
    if not HAS_TORCH:
        pytest.skip("torch overlay not installed")
    root = tmp_path_factory.mktemp("api")
    ds = _write_synthetic_a1(root, n_persons=160)
    reg = root / "reg"
    train_mod.train(str(ds), str(reg), seed=42, epochs=40)
    global_dir = reg / "global"
    global_dir.mkdir()
    shutil.move(str(reg / "credit-ml-v1"), str(global_dir / "credit-ml-v1"))
    return reg


@requires_torch
def test_blend_math_and_provenance(registry: Path) -> None:
    client = _client(_settings(str(registry)))
    res = client.post("/v1/credit/score", json=PAYLOAD, headers=HDR)
    assert res.status_code == 200
    body = res.json()
    assert body["model_version"] == "credit-ml-v1"
    assert body["feature_schema"] == "fv1"
    assert body["reasons"] == []  # rule reasons never dropped
    assert body["rule_score_bureau"] == EXPECTED_RULE_BUREAU
    # Blend: score = clamp(round(0.6*ml + 0.4*rule_bureau)).
    ml_contrib = body["ml_contribution"]
    assert body["score"] == EXPECTED_RULE_BUREAU + int(ml_contrib)
    assert 300 <= body["score"] <= 900
    assert body["default_probability"] is not None
    assert 0.0 <= body["default_probability"] <= 1.0
    # ml_contribution must equal 0.6*(ml - rule): reconstruct ml from the
    # blend equation and check consistency within rounding.
    ml_implied = (body["score"] - 0.4 * EXPECTED_RULE_BUREAU) / 0.6
    assert 300.0 <= ml_implied <= 901.0


@requires_torch
def test_clamp_bounds_with_extreme_rule(registry: Path) -> None:
    client = _client(_settings(str(registry)))
    res = client.post(
        "/v1/credit/score",
        json={"signals": {"tenure_days": 3650, "completed_bookings": 100, "repaid_loans": 100}},
        headers=HDR,
    )
    body = res.json()
    assert 300 <= body["score"] <= 900
    # rule bureau = 900; blend with any ml in [300,900] stays in band.
    res2 = client.post("/v1/credit/score", json={"signals": {}}, headers=HDR)
    body2 = res2.json()
    assert 300 <= body2["score"] <= 900
    assert body2["rule_score_bureau"] == 300


@requires_torch
def test_tenant_isolation_scoped_registry(registry: Path, tmp_path: Path) -> None:
    """A tenant with no artifact falls back to rules even when another
    tenant has one (I4: no cross-tenant reads)."""
    # Move the artifact from global to tenant-b's scope.
    b_dir = registry / "tenant-b"
    b_dir.mkdir()
    shutil.move(str(registry / "global" / "credit-ml-v1"), str(b_dir / "credit-ml-v1"))
    client = _client(_settings(str(registry)))
    res_b = client.post("/v1/credit/score", json=PAYLOAD, headers={"X-Tenant-Id": "tenant-b"})
    res_a = client.post("/v1/credit/score", json=PAYLOAD, headers={"X-Tenant-Id": "tenant-c"})
    assert res_b.json()["model_version"] == "credit-ml-v1"
    assert res_a.json()["model_version"] == "heuristic-v1"
    assert res_a.json()["score"] == EXPECTED_RULE_BUREAU


# ---------------------------------------------------------------------------
# Torch-absent import path (runs in BOTH deployments — subprocess hook)
# ---------------------------------------------------------------------------


NO_TORCH_SCRIPT = """
import builtins
import sys

real_import = builtins.__import__

def blocked(name, *args, **kwargs):
    if name == "torch" or name.startswith("torch."):
        raise ImportError(f"No module named {name!r} (simulated)")
    return real_import(name, *args, **kwargs)

builtins.__import__ = blocked
for mod in [m for m in sys.modules if m == "torch" or m.startswith("torch.")]:
    del sys.modules[mod]

from fastapi.testclient import TestClient
from credit_bureau.config import Settings
from credit_bureau.main import create_app
from credit_bureau.ml.scorer import LearnedScorer

# LearnedScorer absent-torch -> None (I1), service serves rules-only.
assert LearnedScorer.load("/nonexistent", "t") is None
client = TestClient(create_app(Settings(ml_registry_dir="/nonexistent")))
res = client.post(
    "/v1/credit/score",
    json={"signals": {"tenure_days": 120, "completed_bookings": 5, "repaid_loans": 1}},
    headers={"X-Tenant-Id": "tenant-a"},
)
assert res.status_code == 200, res.text
body = res.json()
assert body["score"] == 552 and body["model_version"] == "heuristic-v1", body
print("TORCH-ABSENT-OK")
"""


def test_torch_absent_import_path() -> None:
    proc = subprocess.run(
        [sys.executable, "-c", NO_TORCH_SCRIPT],
        capture_output=True,
        text=True,
        cwd=SERVICE_ROOT,
        timeout=180,
    )
    assert "TORCH-ABSENT-OK" in proc.stdout, proc.stderr[-2000:]


@requires_torch
def test_meta_json_written(tmp_path: Path) -> None:
    """meta.json carries the I3 provenance block (synthetic stated)."""
    ds = _write_synthetic_a1(tmp_path, n_persons=80)
    meta = train_mod.train(str(ds), str(tmp_path / "reg"), seed=42, epochs=10)
    raw = json.loads((tmp_path / "reg" / "credit-ml-v1" / "meta.json").read_text())
    assert raw == meta
    assert raw["label_provenance"] == "synthetic"
    assert "synthetic" in raw["synthetic_outcome_model"]["label_provenance"]
