"""Model-registry consumer tests (SPEC-W33 §4 C1 consumer clause + gate GC2,
wave W33-C).

Covers, for ``graph_ml.registry_client`` + the ADDITIVE load-resolution seam
in ``graph_ml.gnn.GraphSAGEBackend.score_tenant``:

  (a) MODEL_REGISTRY_URL unset/empty -> byte-equal W31/W33-B bootstrap
      behavior (same model_version, same recommendations as a direct load).
  (b) registry serving a valid production record pointing at a file://
      artifact dir -> the seam picks the registry artifact and stamps the
      registry ``version``; recommendations equal the same artifact loaded
      via the bootstrap scan.
  (c) registry 404 -> bootstrap fallback.
  (d) registry timeout (server sleeps past the hard 500ms budget) ->
      fallback, bounded wall time.
  (e) malformed JSON / wrong family / wrong stage / non-file scheme ->
      fallback.
  (f) torch-absent subprocess import test (I5) — runs in BOTH deployments.
  (g) cross-tenant: a record for tenant B is never accepted for tenant A's
      query, and the queried URL path carries the requesting tenant.

The registry fixture is a tiny stdlib http.server (no third-party deps).
Client-level tests run in heuristic deployments too; artifact-seam tests are
marked ``requires_torch``.
"""

from __future__ import annotations

import json
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

SERVICE_ROOT = Path(__file__).resolve().parents[1]

from graph_ml import registry_client  # noqa: E402  (stdlib-only module)

FAMILY = "graphsage"
TENANT = "t1"
NOW = datetime(2026, 8, 5, 12, 0, 0, tzinfo=timezone.utc)


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
        "version": "graphsage-v9",
        "artifact_uri": f"file://{artifact_dir}",
        "stage": "production",
        "seed": 42,
        "dataset_hash": "beefcafe",
    }
    rec.update(overrides)
    state.record = rec
    return rec


# ---------------------------------------------------------------------------
# client-level tests (run in BOTH deployments — no torch needed)
# ---------------------------------------------------------------------------


def test_env_unset_and_empty_disable_registry(tmp_path, monkeypatch):
    monkeypatch.delenv(registry_client.REGISTRY_URL_ENV, raising=False)
    assert registry_client.registry_base_url() is None
    assert registry_client.fetch_production(TENANT) is None
    assert registry_client.resolve_artifact_dir(TENANT) is None
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, "")
    assert registry_client.registry_base_url() is None
    assert registry_client.resolve_artifact_dir(TENANT) is None


def test_fetch_production_returns_validated_record(registry_server, tmp_path):
    scope = tmp_path / "scope"
    scope.mkdir()
    _record(registry_server, scope)
    record = registry_client.fetch_production(TENANT, base_url=registry_server.base_url)
    assert record is not None
    assert record.family == FAMILY
    assert record.tenant_id == TENANT
    assert record.version == "graphsage-v9"
    assert record.stage == "production"
    assert record.seed == 42
    assert record.dataset_hash == "beefcafe"
    resolved = registry_client.resolve_artifact_dir(TENANT, base_url=registry_server.base_url)
    assert resolved is not None
    path, again = resolved
    assert Path(path) == scope
    assert again.version == "graphsage-v9"
    # (g-half) every query path carries the requesting tenant + family
    expected = f"/v1/registry/{FAMILY}/{TENANT}/production"
    assert registry_server.paths
    assert set(registry_server.paths) == {expected}


@pytest.mark.parametrize(
    "overrides",
    [
        {"family": "fraud-ml"},  # wrong family
        {"tenant_id": "tenant-b"},  # cross-tenant record (g)
        {"stage": "staging"},  # not production
        {"version": ""},  # empty version
        {"artifact_uri": "s3://bucket/model"},  # unsupported scheme
        {"artifact_uri": "https://models.example.com/x"},  # unsupported scheme
        {"artifact_uri": "file:///definitely/not/here"},  # missing dir
    ],
)
def test_invalid_records_resolve_none(overrides, registry_server, tmp_path):
    scope = tmp_path / "scope"
    scope.mkdir()
    _record(registry_server, scope, **overrides)
    assert (
        registry_client.resolve_artifact_dir(TENANT, base_url=registry_server.base_url)
        is None
    )
    # every query was still scoped to the REQUESTING tenant (I4)
    assert registry_server.paths
    assert all(f"/{TENANT}/" in p for p in registry_server.paths)
    assert all("tenant-b" not in p for p in registry_server.paths)


def test_malformed_and_404_resolve_none(registry_server):
    registry_server.mode = "malformed"
    assert (
        registry_client.resolve_artifact_dir(TENANT, base_url=registry_server.base_url)
        is None
    )
    registry_server.mode = "notfound"
    assert (
        registry_client.resolve_artifact_dir(TENANT, base_url=registry_server.base_url)
        is None
    )


def test_timeout_and_unreachable_resolve_none_bounded(registry_server):
    registry_server.mode = "sleep"
    t0 = time.perf_counter()
    assert (
        registry_client.resolve_artifact_dir(TENANT, base_url=registry_server.base_url)
        is None
    )
    assert time.perf_counter() - t0 < 2.0
    t0 = time.perf_counter()
    assert registry_client.resolve_artifact_dir(TENANT, base_url="http://127.0.0.1:1") is None
    assert time.perf_counter() - t0 < 2.0


# ---------------------------------------------------------------------------
# trained-artifact seam tests (requires torch + torch-geometric)
# ---------------------------------------------------------------------------

pytestmark_torch = pytest.mark.requires_torch


@pytest.fixture(scope="module")
def trained(tmp_path_factory):
    torch = pytest.importorskip("torch")  # noqa: F841
    pytest.importorskip("torch_geometric")
    from tests.test_gnn_train import make_toy_graph, train_settings
    from graph_ml.gnn_train import train_tenant

    model_root = tmp_path_factory.mktemp("gnn-models")
    settings = train_settings(model_root)
    train_tenant(make_toy_graph(TENANT), TENANT, settings)
    return {
        "model_dir": str(model_root),
        "scope_dir": model_root / TENANT,  # contains graphsage-v1/
    }


def _score(backend, tenant=TENANT):
    from tests.test_gnn_train import make_toy_graph

    return backend.score_tenant(make_toy_graph(tenant), now=NOW, top_k=3)


def _rec_tuples(recs):
    return [(r.person_id, r.offering_id, r.score, r.rank) for r in recs]


def _score_tuples(scores):
    return [
        (s.person_id, s.propensity_churn, s.propensity_convert, s.propensity_turnout, s.risk_score, s.model_version)
        for s in scores
    ]


@pytestmark_torch
def test_env_unset_is_byte_equal_bootstrap(trained, monkeypatch):
    from graph_ml import gnn as gnn_mod

    monkeypatch.delenv(registry_client.REGISTRY_URL_ENV, raising=False)
    backend = gnn_mod.GraphSAGEBackend(trained["model_dir"])
    scores, recs = _score(backend)
    assert backend.model_version == "graphsage-v1"  # dir-derived (I2)
    assert recs and all(r.model_version == "graphsage-v1" for r in recs)
    again = gnn_mod.GraphSAGEBackend(trained["model_dir"])
    scores2, recs2 = _score(again)
    assert _rec_tuples(recs) == _rec_tuples(recs2)
    assert _score_tuples(scores) == _score_tuples(scores2)


@pytestmark_torch
def test_registry_record_pick_and_version_stamp(trained, registry_server, monkeypatch, tmp_path):
    from graph_ml import gnn as gnn_mod

    _record(registry_server, trained["scope_dir"])
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, registry_server.base_url)
    empty_model_dir = tmp_path / "empty-models"  # bootstrap alone would find nothing
    backend = gnn_mod.GraphSAGEBackend(str(empty_model_dir))
    scores, recs = _score(backend)
    assert backend.model_version == "graphsage-v9"  # registry version stamped (I2)
    assert recs and all(r.model_version == "graphsage-v9" for r in recs)
    assert registry_server.paths == [f"/v1/registry/{FAMILY}/{TENANT}/production"]

    # Only provenance changes: recommendations equal the same artifact
    # resolved through the bootstrap scan.
    monkeypatch.delenv(registry_client.REGISTRY_URL_ENV, raising=False)
    bootstrap = gnn_mod.GraphSAGEBackend(trained["model_dir"])
    scores_b, recs_b = _score(bootstrap)
    assert _rec_tuples(recs) == _rec_tuples(recs_b)
    assert _score_tuples(scores) == _score_tuples(scores_b)


@pytestmark_torch
def test_registry_404_falls_back_to_bootstrap(trained, registry_server, monkeypatch):
    from graph_ml import gnn as gnn_mod

    registry_server.mode = "notfound"
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, registry_server.base_url)
    backend = gnn_mod.GraphSAGEBackend(trained["model_dir"])
    _scores, recs = _score(backend)
    assert backend.model_version == "graphsage-v1"
    assert recs and all(r.model_version == "graphsage-v1" for r in recs)
    assert registry_server.paths  # the registry WAS consulted first


@pytestmark_torch
def test_registry_timeout_falls_back_bounded(trained, registry_server, monkeypatch):
    from graph_ml import gnn as gnn_mod

    registry_server.mode = "sleep"
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, registry_server.base_url)
    backend = gnn_mod.GraphSAGEBackend(trained["model_dir"])
    t0 = time.perf_counter()
    _scores, recs = _score(backend)
    elapsed = time.perf_counter() - t0
    assert backend.model_version == "graphsage-v1"
    assert recs
    assert elapsed < 5.0, f"registry timeout budget blown: {elapsed:.2f}s"


@pytestmark_torch
def test_malformed_json_falls_back(trained, registry_server, monkeypatch):
    from graph_ml import gnn as gnn_mod

    registry_server.mode = "malformed"
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, registry_server.base_url)
    backend = gnn_mod.GraphSAGEBackend(trained["model_dir"])
    _scores, recs = _score(backend)
    assert backend.model_version == "graphsage-v1"
    assert recs


@pytestmark_torch
@pytest.mark.parametrize(
    "overrides",
    [
        {"family": "fraud-ml"},  # wrong family
        {"tenant_id": "t2"},  # cross-tenant record (g)
        {"stage": "staging"},  # not production
        {"artifact_uri": "s3://bucket/model"},  # unsupported scheme
        {"artifact_uri": "file:///definitely/not/here"},  # missing dir
    ],
)
def test_invalid_records_fall_back(overrides, trained, registry_server, monkeypatch):
    from graph_ml import gnn as gnn_mod

    _record(registry_server, trained["scope_dir"], **overrides)
    monkeypatch.setenv(registry_client.REGISTRY_URL_ENV, registry_server.base_url)
    backend = gnn_mod.GraphSAGEBackend(trained["model_dir"])
    _scores, recs = _score(backend)
    assert backend.model_version == "graphsage-v1"  # bootstrap, honestly stamped
    assert recs and all(r.model_version == "graphsage-v1" for r in recs)
    # the query itself was scoped to the REQUESTING tenant (I4)
    assert registry_server.paths == [f"/v1/registry/{FAMILY}/{TENANT}/production"]
    assert all("t2" not in p for p in registry_server.paths)


# ---------------------------------------------------------------------------
# (f) torch-absent import safety (I5) — subprocess import hook, both deployments
# ---------------------------------------------------------------------------

IMPORT_SAFETY_SCRIPT = """
import builtins
import os
import sys

real_import = builtins.__import__

def blocked(name, *args, **kwargs):
    if name == "torch" or name.startswith("torch.") or name.startswith("torch_geometric"):
        raise ImportError(f"No module named {name!r} (simulated)")
    return real_import(name, *args, **kwargs)

builtins.__import__ = blocked
for mod in [m for m in sys.modules if m == "torch" or m.startswith("torch")]:
    del sys.modules[mod]

os.environ.pop("MODEL_REGISTRY_URL", None)

import graph_ml.registry_client as rc  # stdlib-only: must import cleanly
assert rc.registry_base_url() is None
assert rc.fetch_production("t1") is None
assert rc.resolve_artifact_dir("t1") is None
# unreachable registry must also degrade, never raise (I1)
assert rc.resolve_artifact_dir("t1", base_url="http://127.0.0.1:1") is None

import graph_ml.gnn as gnn  # backend module import stays torch-safe
assert gnn.GNN_AVAILABLE is False
assert gnn._load_latest_from_scope("/tmp/does-not-exist") is None

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
