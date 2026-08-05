"""GNN import-guard tests (SPEC-W29 §4 gate 5): without torch installed the
gnn backend request degrades to heuristic with a logged warning."""

from __future__ import annotations

import logging
from types import SimpleNamespace

import pytest

from graph_ml import gnn


def test_torch_stack_absent_or_guarded():
    """Heuristic mode needs zero ML deps: either torch/pyg are absent, or the
    import guard has already disarmed the backend (GNN_AVAILABLE False)."""
    import importlib.util

    both_present = (
        importlib.util.find_spec("torch") is not None
        and importlib.util.find_spec("torch_geometric") is not None
    )
    if not both_present:
        assert gnn.GNN_AVAILABLE is False
        assert isinstance(gnn.GNN_IMPORT_ERROR, ImportError)
    else:  # pragma: no cover - only on GPU dev boxes
        assert gnn.GNN_AVAILABLE is True


def test_import_guard_simulates_missing_torch(monkeypatch):
    """Force ImportError on torch and prove the module still imports and
    cleanly reports GNN_AVAILABLE=False (the degraded-deploy path)."""
    import builtins
    import importlib

    real_import = builtins.__import__

    def blocked(name, *args, **kwargs):
        if name == "torch" or name.startswith("torch_geometric"):
            raise ImportError(f"No module named {name!r} (simulated)")
        return real_import(name, *args, **kwargs)

    monkeypatch.setattr(builtins, "__import__", blocked)
    reloaded = importlib.reload(gnn)
    try:
        assert reloaded.GNN_AVAILABLE is False
        assert isinstance(reloaded.GNN_IMPORT_ERROR, ImportError)
        with pytest.raises(reloaded.GNNBackendUnavailable):
            reloaded.GraphSAGEBackend(model_dir="/tmp/models")
    finally:
        monkeypatch.undo()
        importlib.reload(gnn)


def test_resolve_backend_gnn_falls_back_with_warning(caplog):
    settings = SimpleNamespace(backend="gnn")
    with caplog.at_level(logging.WARNING, logger="graph_ml.gnn"):
        backend = gnn.resolve_backend(settings)
    assert backend == "heuristic"
    assert any("falling back to heuristic" in r.message for r in caplog.records)


def test_resolve_backend_default_heuristic():
    assert gnn.resolve_backend(SimpleNamespace(backend="heuristic")) == "heuristic"
    assert gnn.resolve_backend(SimpleNamespace(backend="")) == "heuristic"


def test_graphsage_backend_unavailable_raises():
    with pytest.raises(gnn.GNNBackendUnavailable):
        gnn.GraphSAGEBackend(model_dir="/tmp/models")


def test_next_model_version_increments(tmp_path):
    (tmp_path / "graphsage-v1").mkdir()
    (tmp_path / "graphsage-v3").mkdir()
    (tmp_path / "unrelated").mkdir()
    assert gnn.next_model_version(str(tmp_path)) == "graphsage-v4"
    assert gnn.next_model_version(str(tmp_path / "missing")) == "graphsage-v1"
