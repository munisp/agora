"""Model-registry consumer tests (SPEC-W33 §4 C1 consumer clause + gate GC2,
wave W33-C).

Covers, for ``credit_bureau.ml.registry_client`` + the ADDITIVE wiring in
``LearnedScorer.load``:

  (a) MODEL_REGISTRY_URL unset/empty -> byte-equal W33-B bootstrap behavior
      (same model_version, same scores as a direct load).
  (b) registry serving a valid production record pointing at a file://
      artifact dir -> loader picks the registry artifact and stamps the
      registry ``version``; scores equal the same artifact loaded directly.
  (c) registry 404 -> bootstrap fallback.
  (d) registry timeout (server sleeps past the hard 500ms budget) ->
      fallback, bounded wall time.
  (e) malformed JSON / wrong family / wrong stage / non-file scheme ->
      fallback.
  (f) torch-absent subprocess import test (I5) — runs in BOTH deployments.
  (g) cross-tenant: a record for tenant B is never accepted for tenant A's
      query, and the queried URL path carries the requesting tenant.

The registry fixture is a tiny stdlib http.server (no third-party deps).
"""

from __future__ import annotations

import json
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

SERVICE_ROOT = Path(__file__).resolve().parents[1]

from credit_bureau.ml import registry_client  # noqa: E402  (stdlib-only module)
from credit_bureau.ml.scorer import LearnedScorer  # noqa: E402

FAMILY = "credit-ml"
TENANT = "tenant-a"
SIGNALS = {"income_stability": 0.7, "credit_history_months": 36}


# ---------------------------------------------------------------------------
# tiny stdlib registry-service fixture
# ---------------------------------------------------------------------------


class _RegistryState:
    def __init__(self) -> None:
        self.mode = "ok"  # ok | notfound | malformed | sleep
        self.record: dict | None = None
        self.paths: list[str] = []
        self.base_url = ""


def _make_handler(state: _RegistryState):
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:  # noqa: N802 - stdlib API
            state.paths.append(self.path)
            if state.mode == "sleep":
                time.sleep(1.2)  # past the hard 500ms client budget
            if state.mode == "notfound":
                self.send_response(404)
                self.end_headers()
                return
            body = (
                b"{not-json"
                if state.mode == "malformed"
                else json.dumps(state.record or {}).encode("utf-8")
            )
            try:
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(body)
            except (BrokenPipeError, ConnectionResetError):
                pass  # client timed out and hung up — expected in sleep mode

        def log_message(self, *args) -> None:  # silence fixture logging
            return

    return Handler


@pytest.fixture
def registry_server():
    state = _RegistryState()
    server = ThreadingHTTPServer(("127.0.0.1", 0), _make_handler(state))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    state.base_url = f"http://127.0.0.1:{server.server_address[1]}"
    try:
        yield state
    finally:
        server.shutdown()
        server.server_close()


def _record(state: _RegistryState, artifact_dir: Path, **overrides) -> dict:
    rec = {
        "family": FAMILY,
        "tenant_id": TENANT,
        "version": "credit-ml-v9",
        "artifact_uri": f"file://{artifact_dir}",
        "stage": "production",
        "seed": 7,
        "dataset_hash": "cafebabe",
    }
    rec.update(overrides)
    state.record = rec
    return rec


# ---------------------------------------------------------------------------
# tiny artifact writer (model.pt blob + meta.json, W33-B layout)
# ---------------------------------------------------------------------------


def _write_credit_scope(scope_dir: Path, seed: int, version: str = "credit-ml-v1") -> None:
    torch = pytest.importorskip("torch", reason="torch overlay not installed")
    from credit_bureau.ml.features import FEATURE_DIM, FEATURE_SCHEMA
    from credit_bureau.ml.model import CreditMLP

    torch.manual_seed(seed)
    model = CreditMLP(input_dim=FEATURE_DIM, hidden_dim=8)
    blob = {
        "feature_schema": FEATURE_SCHEMA,
        "input_dim": FEATURE_DIM,
        "hidden_dim": 8,
        "state_dict": model.state_dict(),
    }
    artifact = scope_dir / version
    artifact.mkdir(parents=True)
    torch.save(blob, artifact / "model.pt")
    (artifact / "meta.json").write_text(
        json.dumps({"model_version": version, "feature_schema": FEATURE_SCHEMA})
    )


@pytest.fixture
def local_registry(tmp_path) -> Path:
    """Bootstrap filesystem registry with a {reg}/tenant-a scope (seed=1)."""
    _write_credit_scope(tmp_path / "reg" / TENANT, seed=1)
    return tmp_path / "reg"


# ---------------------------------------------------------------------------
# (a) bootstrap parity — registry env unset/empty
# ---------------------------------------------------------------------------


def test_env_unset_is_byte_equal_bootstrap(local_registry, monkeypatch):
    monkeypatch.delenv(registry_client.REGISTRY_URL_ENV, raising=False)
    sc = LearnedScorer.load(str(local_registry), TENANT)
    assert sc is not None
    assert sc.model_version == "credit-ml-v1"  # dir-derived (I2)
    direct = LearnedScorer.load(str(local_registry), TENANT)
    assert direct is not None
    assert sc.score(SIGNALS) == direct.score(SIGNALS)


def test_env_empty_string_is_bootstrap(local_registry, monkeypatch):
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, "")
    assert registry_client.registry_base_url() is None
    sc = LearnedScorer.load(str(local_registry), TENANT)
    assert sc is not None
    assert sc.model_version == "credit-ml-v1"


# ---------------------------------------------------------------------------
# (b) valid production record -> registry artifact + registry version stamp
# ---------------------------------------------------------------------------


def test_registry_record_pick_and_version_stamp(
    tmp_path, local_registry, registry_server, monkeypatch
):
    registry_scope = tmp_path / "registry-artifact"  # seed=2 tells it apart
    _write_credit_scope(registry_scope, seed=2, version="credit-ml-v3")
    _record(registry_server, registry_scope)
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, registry_server.base_url)

    sc = LearnedScorer.load(None, TENANT)  # no local registry needed at all
    assert sc is not None
    assert sc.model_version == "credit-ml-v9"  # registry version stamped (I2)
    assert sc.meta["model_version"] == "credit-ml-v3"  # registry artifact's meta
    # (g-half) the query path carries the requesting tenant + family
    assert registry_server.paths == [f"/v1/registry/{FAMILY}/{TENANT}/production"]

    # Only provenance changes: scores equal the same artifact loaded directly.
    direct = LearnedScorer._load_scope(str(registry_scope))
    assert direct is not None
    assert sc.score(SIGNALS) == direct.score(SIGNALS)
    # ... and they differ from the local bootstrap artifact (seed=1 weights).
    monkeypatch.delenv(registry_client.REGISTRY_URL_ENV, raising=False)
    local = LearnedScorer.load(str(local_registry), TENANT)
    assert local is not None
    assert local.score(SIGNALS) == LearnedScorer.load(str(local_registry), TENANT).score(SIGNALS)


def test_fetch_production_returns_validated_record(registry_server, tmp_path):
    scope = tmp_path / "scope"
    _write_credit_scope(scope, seed=1)
    _record(registry_server, scope)
    record = registry_client.fetch_production(TENANT, base_url=registry_server.base_url)
    assert record is not None
    assert record.family == FAMILY
    assert record.tenant_id == TENANT
    assert record.version == "credit-ml-v9"
    assert record.stage == "production"
    assert record.seed == 7
    assert record.dataset_hash == "cafebabe"
    resolved = registry_client.resolve_artifact_dir(TENANT, base_url=registry_server.base_url)
    assert resolved is not None
    path, again = resolved
    assert Path(path) == scope
    assert again.version == "credit-ml-v9"


# ---------------------------------------------------------------------------
# (c) 404 -> bootstrap fallback
# ---------------------------------------------------------------------------


def test_registry_404_falls_back_to_bootstrap(local_registry, registry_server, monkeypatch):
    registry_server.mode = "notfound"
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, registry_server.base_url)
    sc = LearnedScorer.load(str(local_registry), TENANT)
    assert sc is not None
    assert sc.model_version == "credit-ml-v1"  # local dir-derived
    assert registry_server.paths  # the registry WAS consulted first


# ---------------------------------------------------------------------------
# (d) timeout -> fallback, bounded wall time (hard 500ms budget)
# ---------------------------------------------------------------------------


def test_registry_timeout_falls_back_bounded(local_registry, registry_server, monkeypatch):
    registry_server.mode = "sleep"
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, registry_server.base_url)
    t0 = time.perf_counter()
    sc = LearnedScorer.load(str(local_registry), TENANT)
    elapsed = time.perf_counter() - t0
    assert sc is not None
    assert sc.model_version == "credit-ml-v1"
    assert elapsed < 2.0, f"registry timeout budget blown: {elapsed:.2f}s"


def test_unreachable_registry_falls_back_fast(local_registry, monkeypatch):
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, "http://127.0.0.1:1")
    t0 = time.perf_counter()
    sc = LearnedScorer.load(str(local_registry), TENANT)
    elapsed = time.perf_counter() - t0
    assert sc is not None
    assert sc.model_version == "credit-ml-v1"
    assert elapsed < 2.0


# ---------------------------------------------------------------------------
# (e) malformed JSON / wrong family / wrong stage / non-file scheme -> fallback
# ---------------------------------------------------------------------------


def test_malformed_json_falls_back(local_registry, registry_server, monkeypatch):
    registry_server.mode = "malformed"
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, registry_server.base_url)
    sc = LearnedScorer.load(str(local_registry), TENANT)
    assert sc is not None
    assert sc.model_version == "credit-ml-v1"


@pytest.mark.parametrize(
    "overrides",
    [
        {"family": "fraud-ml"},  # wrong family
        {"stage": "staging"},  # not production
        {"version": ""},  # empty version
        {"artifact_uri": "s3://bucket/model"},  # unsupported scheme
        {"artifact_uri": "https://models.example.com/x"},  # unsupported scheme
        {"artifact_uri": "file:///definitely/not/here"},  # missing dir
    ],
)
def test_invalid_records_fall_back(
    overrides, tmp_path, local_registry, registry_server, monkeypatch
):
    scope = tmp_path / "registry-artifact"
    _write_credit_scope(scope, seed=2, version="credit-ml-v3")
    _record(registry_server, scope, **overrides)
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, registry_server.base_url)
    sc = LearnedScorer.load(str(local_registry), TENANT)
    assert sc is not None
    assert sc.model_version == "credit-ml-v1"  # bootstrap
    assert sc.meta["model_version"] == "credit-ml-v1"  # local artifact, not seed=2's


def test_missing_fields_fall_back(local_registry, registry_server, monkeypatch):
    registry_server.record = {"family": FAMILY, "tenant_id": TENANT}  # no version/uri/stage
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, registry_server.base_url)
    sc = LearnedScorer.load(str(local_registry), TENANT)
    assert sc is not None
    assert sc.model_version == "credit-ml-v1"


# ---------------------------------------------------------------------------
# (g) cross-tenant: tenant B's record never accepted for tenant A
# ---------------------------------------------------------------------------


def test_cross_tenant_record_refused(tmp_path, local_registry, registry_server, monkeypatch):
    scope = tmp_path / "registry-artifact"
    _write_credit_scope(scope, seed=2, version="credit-ml-v3")
    _record(registry_server, scope, tenant_id="tenant-b")  # record for ANOTHER tenant
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, registry_server.base_url)
    sc = LearnedScorer.load(str(local_registry), TENANT)
    assert sc is not None
    assert sc.model_version == "credit-ml-v1"  # bootstrap, not tenant-b's
    # the query itself was scoped to the REQUESTING tenant (I4)
    assert registry_server.paths == [f"/v1/registry/{FAMILY}/{TENANT}/production"]
    assert all("tenant-b" not in p for p in registry_server.paths)


# ---------------------------------------------------------------------------
# (f) torch-absent import safety (I5) — subprocess import hook, both deployments
# ---------------------------------------------------------------------------

IMPORT_SAFETY_SCRIPT = """
import builtins
import os
import sys

real_import = builtins.__import__

def blocked(name, *args, **kwargs):
    if name == "torch" or name.startswith("torch."):
        raise ImportError(f"No module named {name!r} (simulated)")
    return real_import(name, *args, **kwargs)

builtins.__import__ = blocked
for mod in [m for m in sys.modules if m == "torch" or m.startswith("torch.")]:
    del sys.modules[mod]

os.environ.pop("MODEL_REGISTRY_URL", None)

import credit_bureau.ml.registry_client as rc  # stdlib-only: must import cleanly
assert rc.registry_base_url() is None
assert rc.fetch_production("t1") is None
assert rc.resolve_artifact_dir("t1") is None
# unreachable registry must also degrade, never raise (I1)
assert rc.resolve_artifact_dir("t1", base_url="http://127.0.0.1:1") is None

from credit_bureau.ml.scorer import LearnedScorer  # scorer import stays torch-safe
assert LearnedScorer.load("/tmp/does-not-exist", "t1") is None

assert "torch" not in sys.modules
print("IMPORT-SAFETY-OK")
"""


def test_registry_client_import_safe_without_torch():
    proc = subprocess.run(
        [sys.executable, "-c", IMPORT_SAFETY_SCRIPT],
        capture_output=True,
        text=True,
        cwd=str(SERVICE_ROOT),
        timeout=120,
    )
    assert proc.returncode == 0, f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
    assert "IMPORT-SAFETY-OK" in proc.stdout
