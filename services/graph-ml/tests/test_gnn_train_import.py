"""gnn_train import-safety without torch (SPEC-W31 §0 invariant 5).

``main.py`` imports ``gnn_train`` at module top even in the heuristic base
image, so the module must import cleanly with torch/torch-geometric absent
and raise ``GNNBackendUnavailable`` at CALL time. Verified in a subprocess
with an import hook blocking the torch stack (NOT marked requires_torch —
this test must run in BOTH deployments)."""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

SERVICE_ROOT = Path(__file__).resolve().parents[1]

SCRIPT = """
import builtins
import sys

real_import = builtins.__import__

def blocked(name, *args, **kwargs):
    if name == "torch" or name.startswith("torch_geometric"):
        raise ImportError(f"No module named {name!r} (simulated)")
    return real_import(name, *args, **kwargs)

builtins.__import__ = blocked
for mod in [m for m in sys.modules if m == "torch" or m.startswith("torch_geometric")]:
    del sys.modules[mod]

from types import SimpleNamespace

import graph_ml.gnn_train as gnn_train  # must import without torch
import graph_ml.main  # noqa: F401 - heuristic service boot must not crash
from graph_ml.extract import TenantGraph
from graph_ml.gnn import GNNBackendUnavailable

assert gnn_train._TORCH_AVAILABLE is False
assert gnn_train.GraphSAGEModel is None
# main.py's `except gnn_train.GNNBackendUnavailable` resolves:
assert gnn_train.GNNBackendUnavailable is GNNBackendUnavailable

try:
    gnn_train.train_tenant(TenantGraph("t1"), "t1", SimpleNamespace(model_dir="/tmp/m"))
except GNNBackendUnavailable:
    pass
else:
    raise SystemExit("train_tenant did not raise GNNBackendUnavailable")

try:
    gnn_train.load_latest("/tmp/m", "t1")
except GNNBackendUnavailable:
    pass
else:
    raise SystemExit("load_latest did not raise GNNBackendUnavailable")

print("IMPORT-SAFE-OK")
"""


def test_gnn_train_import_safe_and_raises_without_torch():
    proc = subprocess.run(
        [sys.executable, "-c", SCRIPT],
        capture_output=True,
        text=True,
        cwd=str(SERVICE_ROOT),
        timeout=120,
    )
    assert proc.returncode == 0, f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
    assert "IMPORT-SAFE-OK" in proc.stdout
