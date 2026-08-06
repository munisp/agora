"""ray_train tests (SPEC-W33 §4 C4): multi-tenant A1 fleet discovery,
serial fallback == Ray path bit-identity (GC5 — the core assertion),
ray-absent import safety (I5), min-size-gate heuristic-fallback reporting,
and CLI smoke.

Fixture style mirrors tests/test_gnn_labels.py (A1 dataset dirs written
programmatically), scaled up so each tenant clears the min-size gate
(>=20 persons / >=30 booked+referred edges) via a seeded rng.

Requires torch (skipped cleanly on heuristic-only deployments); the Ray
path test additionally carries the ``requires_ray`` skip marker (ray is
the optional W33-C overlay — when it is absent the serial fallback above
is still fully tested, and the ray-path test skips cleanly).
"""

from __future__ import annotations

import importlib.util

import pytest

torch = pytest.importorskip("torch")
pytest.importorskip("torch_geometric")

pytestmark = pytest.mark.requires_torch

import json
import random
import subprocess
import sys
from pathlib import Path

from graph_ml import ray_train
from graph_ml.ray_train import (
    RayUnavailable,
    discover_tenant_datasets,
    tenant_seed,
    train_all_tenants,
)

SERVICE_ROOT = Path(__file__).resolve().parents[1]

RAY_PRESENT = importlib.util.find_spec("ray") is not None
#: Skip marker for tests that need the optional Ray overlay installed.
requires_ray = pytest.mark.skipif(
    not RAY_PRESENT,
    reason="ray not installed (optional W33-C overlay; serial fallback "
    "is tested unconditionally)",
)

TENANT_IDS = ("tenant-a", "tenant-b", "tenant-c")
GENERATED_AT = "2026-01-01T00:00:00Z"  # deterministic A1 stamp (GB1)

# Fast training knobs shared by every test (registry math is unchanged,
# just fewer/shorter epochs).
TRAIN_KWARGS = {"epochs": 4, "hidden_dim": 16, "patience": 3}


# ---------------------------------------------------------------------------
# A1 fleet fixture (programmatic, seeded; mirrors test_gnn_labels.py style)
# ---------------------------------------------------------------------------


def _write_tenant_dataset(
    root: Path,
    tenant_id: str,
    seed: int,
    num_persons: int = 24,
    num_offerings: int = 6,
    num_positives: int = 4,
) -> Path:
    """One A1-format tenant dir; default sizes clear the min-size gate."""
    rng = random.Random(seed)
    d = root / tenant_id
    d.mkdir(parents=True)

    pids = [f"per-{i:05d}" for i in range(num_persons)]
    persons = [
        {
            "person_id": pid,
            "persona": "market_trader",
            "name_hash": f"sha256:{tenant_id}:{pid}",
            "phone_hash": "sha256:x",
            "fraud": False,  # labels.json is the ONLY ground truth (A1 quirk)
            "scenario": None,
        }
        for pid in pids
    ]
    (d / "persons.jsonl").write_text(
        "".join(json.dumps(p, sort_keys=True) + "\n" for p in persons),
        encoding="utf-8",
    )

    edges: list[dict] = []
    # Referral ring: num_persons unique REFERRED pairs.
    for i, pid in enumerate(pids):
        edges.append(
            {
                "edge_id": f"edg-ref-{i}",
                "edge_type": "REFERRED",
                "at": "2026-02-01T10:00:00Z",
                "from_person_id": pid,
                "to_person_id": pids[(i + 1) % num_persons],
                "program": "reward-referral",
                "fraud": False,
                "scenario": None,
            }
        )
    # Two BOOKED edges per person across offerings (unique pairs dominate).
    for i, pid in enumerate(pids):
        for k in range(2):
            offering = f"off-{(i + k * (1 + rng.randint(0, 3))) % num_offerings}"
            edges.append(
                {
                    "edge_id": f"edg-boo-{i}-{k}",
                    "edge_type": "BOOKED",
                    "at": "2026-02-02T11:00:00Z",
                    "person_id": pid,
                    "booking_id": f"boo-{i}-{k}",
                    "offering_id": offering,
                    "status": "completed",
                    "showed": True,
                    "fraud": False,
                    "scenario": None,
                }
            )
    (d / "graph_edges.jsonl").write_text(
        "".join(json.dumps(e, sort_keys=True) + "\n" for e in edges),
        encoding="utf-8",
    )

    positives = pids[-num_positives:] if num_positives else []
    labels_doc = {
        "seed": seed,
        "entries": [
            {
                "entity_id": pid,
                "scenario": "referral_ring",
                "fraud": True,
                "injected_at": "2026-02-01T10:00:00Z",
            }
            for pid in positives
        ],
    }
    (d / "labels.json").write_text(
        json.dumps(labels_doc, indent=2) + "\n", encoding="utf-8"
    )
    (d / "manifest.json").write_text(
        json.dumps({"seed": seed, "generated_at": GENERATED_AT}) + "\n",
        encoding="utf-8",
    )
    return d


def _write_fleet(root: Path, *, include_undersized: bool = False) -> Path:
    """3 well-sized tenants (+ optionally one below the min-size gate)."""
    for index, tenant_id in enumerate(TENANT_IDS):
        _write_tenant_dataset(root, tenant_id, seed=100 + index)
    if include_undersized:
        # 5 persons / ring + few bookings: below BOTH gates (20p / 30e).
        _write_tenant_dataset(
            root, "tenant-small", seed=999, num_persons=5,
            num_offerings=2, num_positives=0,
        )
    return root


def _read_meta(artifact_dir: str) -> dict:
    return json.loads((Path(artifact_dir) / "meta.json").read_text(encoding="utf-8"))


# ---------------------------------------------------------------------------
# (a) --all-tenants completes; per-tenant registries populated
# ---------------------------------------------------------------------------


def test_discovery_sorted_and_seeded_scheme(tmp_path):
    root = _write_fleet(tmp_path / "datasets", include_undersized=True)
    discovered = discover_tenant_datasets(root)
    assert [tid for tid, _ in discovered] == sorted(
        [*TENANT_IDS, "tenant-small"]
    )
    # Determinism scheme: per-tenant seed = base seed + sorted index.
    assert [tenant_seed(42, i) for i in range(3)] == [42, 43, 44]


def test_all_tenants_serial_populates_registries(tmp_path):
    root = _write_fleet(tmp_path / "datasets")
    model_dir = tmp_path / "models"
    result = train_all_tenants(
        root, head="link", seed=7, model_dir=str(model_dir),
        ray_address="local", **TRAIN_KWARGS,
    )
    assert result["mode"] == "serial"
    tenants = {t["tenant_id"]: t for t in result["tenants"]}
    assert set(tenants) == set(TENANT_IDS)
    for index, tenant_id in enumerate(sorted(TENANT_IDS)):
        outcome = tenants[tenant_id]
        assert outcome["status"] == "trained"
        assert outcome["seed"] == 7 + index  # base seed + tenant index
        assert outcome["model_version"] == "graphsage-v1"
        artifact_dir = Path(outcome["model_dir"])
        assert artifact_dir.parent == model_dir / tenant_id
        assert (artifact_dir / "model.pt").is_file()
        assert (artifact_dir / "meta.json").is_file()
        meta = _read_meta(str(artifact_dir))
        assert meta["head"] == "link"
        assert meta["seed"] == outcome["seed"]
        assert meta["final_loss"] == outcome["final_loss"]


def test_cli_all_tenants_subprocess(tmp_path):
    """End-to-end CLI smoke: --all-tenants over the fleet (serial mode)."""
    root = _write_fleet(tmp_path / "datasets")
    model_dir = tmp_path / "models"
    proc = subprocess.run(
        [
            sys.executable, "-m", "graph_ml.ray_train",
            "--all-tenants",
            "--datasets-root", str(root),
            "--model-dir", str(model_dir),
            "--seed", "5",
            "--epochs", "4",
            "--hidden-dim", "16",
            "--ray-address", "local",
        ],
        capture_output=True, text=True, cwd=str(SERVICE_ROOT), timeout=600,
    )
    assert proc.returncode == 0, f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
    result = json.loads(proc.stdout)
    assert result["mode"] == "serial"
    tenants = {t["tenant_id"]: t for t in result["tenants"]}
    assert set(tenants) == set(TENANT_IDS)
    assert all(t["status"] == "trained" for t in tenants.values())
    assert all(
        (model_dir / tid / "graphsage-v1" / "meta.json").is_file()
        for tid in TENANT_IDS
    )


# ---------------------------------------------------------------------------
# (b) GC5 core: serial fallback == Ray path, bit-identical
# ---------------------------------------------------------------------------


def test_serial_repeat_is_bit_identical(tmp_path):
    """Baseline: the serial path against itself, separate roots (GC5 floor)."""
    root = _write_fleet(tmp_path / "datasets")
    first = train_all_tenants(
        root, head="classifier", seed=11, model_dir=str(tmp_path / "m1"),
        ray_address="local", **TRAIN_KWARGS,
    )
    second = train_all_tenants(
        root, head="classifier", seed=11, model_dir=str(tmp_path / "m2"),
        ray_address="local", **TRAIN_KWARGS,
    )
    for left, right in zip(first["tenants"], second["tenants"]):
        assert left["tenant_id"] == right["tenant_id"]
        assert left["final_val_loss"] == right["final_val_loss"]  # bit-identical
        assert (
            Path(left["model_dir"], "meta.json").read_bytes()
            == Path(right["model_dir"], "meta.json").read_bytes()
        )


@requires_ray
def test_serial_fallback_matches_ray_path_bit_identical(tmp_path):
    """GC5 core assertion: ray remote tasks vs serial fallback — identical.

    Classifier head gives byte-equal meta.json (deterministic trained_at);
    bit-identical final_val_loss; link-head final_loss also bit-identical
    (its meta differs only in the pre-existing W31 wall-clock trained_at).
    """
    import ray

    root = _write_fleet(tmp_path / "datasets")
    ray.init(num_cpus=2, include_dashboard=False, ignore_reinit_error=True,
             logging_level="ERROR")
    try:
        serial = train_all_tenants(
            root, head="classifier", seed=11, model_dir=str(tmp_path / "serial"),
            ray_address="local", **TRAIN_KWARGS,
        )
        rayed = train_all_tenants(
            root, head="classifier", seed=11, model_dir=str(tmp_path / "rayed"),
            ray_address=None, **TRAIN_KWARGS,
        )
        serial_link = train_all_tenants(
            root, head="link", seed=11, model_dir=str(tmp_path / "serial-link"),
            ray_address="local", **TRAIN_KWARGS,
        )
        rayed_link = train_all_tenants(
            root, head="link", seed=11, model_dir=str(tmp_path / "rayed-link"),
            ray_address=None, **TRAIN_KWARGS,
        )
    finally:
        ray.shutdown()

    assert serial["mode"] == "serial" and rayed["mode"] == "ray"
    by_tenant = lambda result: {t["tenant_id"]: t for t in result["tenants"]}
    serial_by, rayed_by = by_tenant(serial), by_tenant(rayed)
    assert set(serial_by) == set(rayed_by) == set(TENANT_IDS)
    for tenant_id in TENANT_IDS:
        s, r = serial_by[tenant_id], rayed_by[tenant_id]
        assert s["status"] == r["status"] == "trained"
        assert s["seed"] == r["seed"]  # same deterministic per-tenant seed
        assert s["final_val_loss"] == r["final_val_loss"]  # bit-identical
        assert s["final_loss"] == r["final_loss"]
        assert s["epochs_run"] == r["epochs_run"]
        assert s["best_epoch"] == r["best_epoch"]
        # Byte-equal meta.json (model_dir is not recorded inside meta).
        assert (
            Path(s["model_dir"], "meta.json").read_bytes()
            == Path(r["model_dir"], "meta.json").read_bytes()
        )

    for s, r in zip(serial_link["tenants"], rayed_link["tenants"]):
        assert s["final_loss"] == r["final_loss"]  # bit-identical
        meta_s, meta_r = _read_meta(s["model_dir"]), _read_meta(r["model_dir"])
        # The ONLY tolerated drift: W31 link mode's wall-clock trained_at.
        assert meta_s.pop("trained_at") != meta_r.pop("trained_at") or True
        assert meta_s == meta_r


# ---------------------------------------------------------------------------
# (c) ray-absent import safety (subprocess import hook, I5)
# ---------------------------------------------------------------------------

IMPORT_SAFETY_SCRIPT = """
import builtins
import sys
import tempfile

real_import = builtins.__import__

def blocked(name, *args, **kwargs):
    if name == "ray" or name.startswith("ray."):
        raise ImportError(f"No module named {name!r} (simulated)")
    return real_import(name, *args, **kwargs)

builtins.__import__ = blocked
for mod in [m for m in list(sys.modules) if m == "ray" or m.startswith("ray.")]:
    del sys.modules[mod]

import graph_ml.ray_train as rt  # must import without ray (I5)

assert rt._RAY_AVAILABLE is False
assert rt.ray is None

try:
    rt.init_ray()
except rt.RayUnavailable:
    pass
else:
    raise SystemExit("init_ray did not raise RayUnavailable")

empty_root = tempfile.mkdtemp()
try:
    rt.train_all_tenants(empty_root, model_dir=empty_root,
                         ray_address="ray-head:6379")
except rt.RayUnavailable:
    pass
else:
    raise SystemExit("explicit --ray-address without ray did not raise")

# ...but the serial fallback path is fine without ray (no tenants here).
result = rt.train_all_tenants(empty_root, model_dir=empty_root,
                              ray_address="local")
assert result == {"mode": "serial", "tenants": []}

print("RAY-IMPORT-SAFE-OK")
"""


def test_ray_absent_import_safety():
    proc = subprocess.run(
        [sys.executable, "-c", IMPORT_SAFETY_SCRIPT],
        capture_output=True, text=True, cwd=str(SERVICE_ROOT), timeout=120,
    )
    assert proc.returncode == 0, f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
    assert "RAY-IMPORT-SAFE-OK" in proc.stdout


# ---------------------------------------------------------------------------
# (d) undersized tenant -> heuristic-fallback skip, not an error
# ---------------------------------------------------------------------------


def test_undersized_tenant_is_heuristic_fallback_not_error(tmp_path):
    root = _write_fleet(tmp_path / "datasets", include_undersized=True)
    model_dir = tmp_path / "models"
    result = train_all_tenants(
        root, head="classifier", seed=3, model_dir=str(model_dir),
        ray_address="local", **TRAIN_KWARGS,
    )
    tenants = {t["tenant_id"]: t for t in result["tenants"]}
    small = tenants["tenant-small"]
    assert small["status"] == "fallback"
    assert "heuristic fallback" in small["fallback_reason"]
    assert "model_version" not in small
    assert not (model_dir / "tenant-small").exists()  # gate writes nothing
    for tenant_id in TENANT_IDS:  # the rest of the fleet is unaffected
        assert tenants[tenant_id]["status"] == "trained"


# ---------------------------------------------------------------------------
# (e) CLI smoke: --help works WITHOUT ray (import hook)
# ---------------------------------------------------------------------------

CLI_HELP_SCRIPT = """
import builtins
import runpy
import sys

real_import = builtins.__import__

def blocked(name, *args, **kwargs):
    if name == "ray" or name.startswith("ray."):
        raise ImportError(f"No module named {name!r} (simulated)")
    return real_import(name, *args, **kwargs)

builtins.__import__ = blocked
for mod in [m for m in list(sys.modules) if m == "ray" or m.startswith("ray.")]:
    del sys.modules[mod]

sys.argv = ["graph_ml.ray_train", "--help"]
try:
    runpy.run_module("graph_ml.ray_train", run_name="__main__")
except SystemExit as exc:
    assert exc.code == 0, f"--help exited {exc.code}"
else:
    raise SystemExit("--help did not exit")

print("RAY-CLI-HELP-OK")
"""


def test_cli_help_works_without_ray():
    proc = subprocess.run(
        [sys.executable, "-c", CLI_HELP_SCRIPT],
        capture_output=True, text=True, cwd=str(SERVICE_ROOT), timeout=120,
    )
    assert proc.returncode == 0, f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
    assert "--all-tenants" in proc.stdout
    assert "RAY-CLI-HELP-OK" in proc.stdout
