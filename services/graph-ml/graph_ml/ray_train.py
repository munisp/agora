"""Ray distributed multi-tenant training driver (SPEC-W33 §4 C4).

HONEST FRAMING (SPEC-W33 §4): Ray here exists for MULTI-TENANT PARALLELISM
and future horizontal scale — nothing more. One tenant's GraphSAGE trains
fine in a single process (``gnn_train.py``); this module's job is to train
a FLEET of tenants concurrently without one tenant's sweep starving the
others. The dev topology is a single-node CPU Ray cluster (see
``infra/docker-compose.ray.yml``: 1-CPU head + 2 1-CPU workers, tunable via
env); multi-node scale-out is one ``RAY_ADDRESS`` away — pass
``--ray-address ray://<head>:10001`` (or ``<head>:6379``) and the same
driver submits to the existing cluster instead of initializing a local one.
No GPU claims, no data-parallel claims: parallelism is ACROSS tenants, not
within one tenant's model.

IMPORT SAFETY (SPEC-W31 §0 invariant 5 idiom, I5): ``ray`` sits behind a
guarded import exactly like ``torch`` in ``gnn_train.py`` — this module is
IMPORT-SAFE WITHOUT ray. When ray is absent, ``_RAY_AVAILABLE`` is False
and ray-required entry points (``init_ray``) raise :class:`RayUnavailable`
at CALL time, never at import time. The CLI/``train_all_tenants`` never
hard-require ray: with ray absent (or the cluster unreachable, or
``--ray-address local``) they take the LOCAL FALLBACK and train tenants
SERIALLY in the driver process.

DETERMINISM SCHEME (GC5 core assertion): the serial fallback and the Ray
path produce BIT-IDENTICAL per-tenant results.
  * Per-tenant seed: ``tenant_seed(base_seed, tenant_index) = base_seed +
    tenant_index``, where ``tenant_index`` is the tenant's position in the
    sorted discovery order under the datasets root. The seed depends only
    on the tenant set — never on scheduling, worker assignment, or timing —
    so the same tenant trains with the same seed on both paths.
  * Both paths funnel through the SAME worker function
    (:func:`_train_one_tenant`), which pins ``torch.set_num_threads(1)``
    (and interop threads) before training — mirroring the
    ``gnn_train.seed_everything`` conventions — so CPU thread-count
    differences between driver and worker processes cannot perturb the
    float math.
  * Classifier-head meta.json is byte-equal across paths (its
    ``trained_at`` is the A1 dataset's deterministic stamp by W33-B3
    design). Link-head meta.json is deterministic except its pre-existing
    W31 wall-clock ``trained_at``; ``final_loss`` is still bit-identical.

CLI (offline A1 datasets, multi-tenant bootstrap):
``python -m graph_ml.ray_train --all-tenants --datasets-root DIR
[--head link|classifier] [--seed N] [--epochs N] [--model-dir DIR]
[--ray-address ADDR]``

Exit codes: 0 = every discovered tenant either trained or was skipped by
the min-size gate (heuristic fallback is NOT an error, SPEC-W31 §0
invariant 1); 2 = usage/config error (e.g. an explicit ``--ray-address``
with ray not installed).
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sys
from pathlib import Path
from typing import Any

from .gnn import GNNInsufficientData

try:  # same guard idiom as torch in gnn_train.py (SPEC-W31 §0 invariant 5, I5)
    import ray

    _RAY_AVAILABLE = True
except ImportError:  # ray is an optional overlay (infra/docker-compose.ray.yml)
    ray = None  # type: ignore[assignment]
    _RAY_AVAILABLE = False

log = logging.getLogger(__name__)

#: Sentinel value for --ray-address that forces the serial fallback (tests,
#: debugging, boxes where a local cluster init is unwanted).
FORCE_SERIAL_ADDRESS = "local"


class RayUnavailable(RuntimeError):
    """Raised at CALL time when a ray-required path runs without ray.

    Mirrors ``gnn.GNNBackendUnavailable``: import of this module never
    fails for a missing ray; only an explicit ray-required call does.
    """


def _require_ray() -> None:
    """Raise :class:`RayUnavailable` at CALL time (never import time)."""
    if not _RAY_AVAILABLE:
        raise RayUnavailable(
            "ray is not installed; distributed multi-tenant training "
            "unavailable — use the serial fallback (train_all_tenants / "
            "--ray-address local) or the ray overlay image "
            "(infra/docker-compose.ray.yml)"
        )


def tenant_seed(base_seed: int, tenant_index: int) -> int:
    """Deterministic per-tenant seed: ``base_seed + tenant_index``.

    ``tenant_index`` is the tenant's position in the sorted discovery order
    (see :func:`discover_tenant_datasets`), so a tenant's seed is stable
    across the Ray and serial paths and across runs over the same tenant
    set. Adding a tenant shifts the seeds of tenants that sort after it —
    documented, accepted: determinism is per fixed tenant fleet (GC5).
    """
    return base_seed + tenant_index


def discover_tenant_datasets(datasets_root: str | Path) -> list[tuple[str, str]]:
    """Sorted ``(tenant_id, dataset_dir)`` pairs under ``datasets_root``.

    A subdirectory is a tenant dataset when it contains ``persons.jsonl``
    (the A1 format marker; the other required files are validated by
    ``gnn_labels.load_labeled_graph`` at train time, which raises an honest
    ``FileNotFoundError``). Tenant id = subdirectory name. Sorted order
    makes the tenant index — and therefore the per-tenant seed — stable.
    """
    root = Path(datasets_root)
    if not root.is_dir():
        raise FileNotFoundError(f"datasets root not found: {root}")
    discovered = [
        (entry.name, str(entry))
        for entry in root.iterdir()
        if entry.is_dir() and (entry / "persons.jsonl").is_file()
    ]
    discovered.sort(key=lambda pair: pair[0])
    return discovered


def _pin_cpu_threads() -> None:
    """Pin torch to 1 thread for cross-process CPU bit-identity (GC5)."""
    from . import gnn_train  # lazy: torch-guarded module

    if not gnn_train._TORCH_AVAILABLE:  # heuristic deployment: nothing to pin
        return
    gnn_train.torch.set_num_threads(1)
    try:  # interop threads may only be set once per process
        gnn_train.torch.set_num_interop_threads(1)
    except RuntimeError:  # already set in this process — same value anyway
        pass


def _train_one_tenant(
    dataset_dir: str,
    tenant_id: str,
    tenant_index: int,
    *,
    head: str,
    base_seed: int,
    model_dir: str,
    epochs: int | None = None,
    hidden_dim: int | None = None,
    patience: int | None = None,
    val_fraction: float | None = None,
) -> dict[str, Any]:
    """Train ONE tenant from its A1 dataset dir; the shared worker function.

    This exact function runs as the Ray remote task body AND inline in the
    serial fallback, which is what makes the two paths bit-identical
    (GC5): same code, same per-tenant seed, same thread pinning.

    Returns a JSON-serializable summary dict. A tenant below the min-size
    gate yields ``status: "fallback"`` with ``fallback_reason`` (the
    heuristic-fallback mapping of ``GNNInsufficientData`` — NOT an error,
    SPEC-W31 §0 invariant 1).
    """
    _pin_cpu_threads()
    from .config import Settings
    from . import gnn_train
    from .gnn_labels import load_labeled_graph

    seed = tenant_seed(base_seed, tenant_index)
    overrides: dict[str, Any] = {"model_dir": model_dir, "seed": seed}
    if epochs is not None:
        overrides["gnn_epochs"] = epochs
    if hidden_dim is not None:
        overrides["gnn_hidden_dim"] = hidden_dim
    if patience is not None:
        overrides["gnn_head_patience"] = patience
    if val_fraction is not None:
        overrides["gnn_head_val_fraction"] = val_fraction
    settings = Settings(**overrides)

    labeled = load_labeled_graph(dataset_dir, tenant_id=tenant_id)
    summary: dict[str, Any] = {
        "tenant_id": tenant_id,
        "tenant_index": tenant_index,
        "seed": seed,
        "head": head,
        "dataset_sha256": labeled.dataset_sha256,
    }
    try:
        if head == "classifier":
            result = gnn_train.train_tenant_classifier(
                labeled.graph,
                labeled.labels,
                tenant_id,
                settings,
                dataset_sha256=labeled.dataset_sha256,
                dataset_seed=labeled.dataset_seed,
                dataset_generated_at=labeled.dataset_generated_at,
                masked_out=labeled.masked_out,
            )
            summary.update(
                status="trained",
                model_version=result.model_version,
                model_dir=result.model_dir,
                final_loss=result.final_loss,
                final_val_loss=result.final_val_loss,
                epochs_run=result.epochs_run,
                best_epoch=result.best_epoch,
                stopped_early=result.stopped_early,
            )
        else:  # W31 link-prediction mode (the default)
            result = gnn_train.train_tenant(labeled.graph, tenant_id, settings)
            summary.update(
                status="trained",
                model_version=result.model_version,
                model_dir=result.model_dir,
                final_loss=result.final_loss,
                epochs=result.epochs,
            )
    except GNNInsufficientData as exc:
        # Min-size gate -> per-tenant heuristic fallback, NOT an error.
        summary.update(status="fallback", fallback_reason=str(exc))
    log.info(
        "tenant %s (%s) -> %s", tenant_id, dataset_dir, summary["status"],
        extra={"tenant_id": tenant_id},
    )
    return summary


def init_ray(ray_address: str | None = None, **init_kwargs: Any) -> Any:
    """Connect to (or locally initialize) the Ray cluster. Ray-REQUIRED path.

    ``ray_address=None`` initializes a local single-node cluster (the dev
    topology); an address string submits to an existing cluster — multi-node
    is one RAY_ADDRESS away. Raises :class:`RayUnavailable` at call time
    when ray is not installed; propagates connection failures so the caller
    can apply the serial fallback.
    """
    _require_ray()
    if ray.is_initialized():  # reuse the caller's context (tests pin num_cpus)
        return ray.get_runtime_context()
    kwargs: dict[str, Any] = {"ignore_reinit_error": True}
    if ray_address is None:
        kwargs["include_dashboard"] = False
    else:
        kwargs["address"] = ray_address
    kwargs.update(init_kwargs)
    return ray.init(**kwargs)


def _train_serial(
    tenants: list[tuple[str, str]], **train_kwargs: Any
) -> list[dict[str, Any]]:
    """LOCAL FALLBACK: train tenants serially in this process (GC5 path)."""
    return [
        _train_one_tenant(dataset_dir, tenant_id, index, **train_kwargs)
        for index, (tenant_id, dataset_dir) in enumerate(tenants)
    ]


def _train_ray(
    tenants: list[tuple[str, str]], ray_address: str | None, **train_kwargs: Any
) -> list[dict[str, Any]]:
    """Train tenants as Ray remote tasks (one task per tenant, fanned out)."""
    init_ray(ray_address)
    remote_one = ray.remote(_train_one_tenant)
    refs = [
        remote_one.remote(dataset_dir, tenant_id, index, **train_kwargs)
        for index, (tenant_id, dataset_dir) in enumerate(tenants)
    ]
    return list(ray.get(refs))


def train_all_tenants(
    datasets_root: str | Path,
    *,
    head: str = "link",
    seed: int = 42,
    model_dir: str,
    epochs: int | None = None,
    hidden_dim: int | None = None,
    patience: int | None = None,
    val_fraction: float | None = None,
    ray_address: str | None = None,
) -> dict[str, Any]:
    """Train every discovered tenant; returns ``{"mode", "tenants": [...]}``.

    Path selection (GC5 — all paths produce identical per-tenant results):
      * ``ray_address == "local"`` -> forced serial fallback (tests).
      * ray not installed -> serial fallback, unless an explicit
        ``ray_address`` was requested, which is ray-required and raises
        :class:`RayUnavailable`.
      * ray installed but cluster init/connect fails -> serial fallback
        (cluster unreachable is a degradation, not an error).
      * otherwise -> Ray remote tasks (one per tenant).
    """
    tenants = discover_tenant_datasets(datasets_root)
    train_kwargs: dict[str, Any] = {
        "head": head,
        "base_seed": seed,
        "model_dir": model_dir,
        "epochs": epochs,
        "hidden_dim": hidden_dim,
        "patience": patience,
        "val_fraction": val_fraction,
    }

    if ray_address == FORCE_SERIAL_ADDRESS:
        log.info("ray-address=local: forced serial fallback")
        return {"mode": "serial", "tenants": _train_serial(tenants, **train_kwargs)}

    if not _RAY_AVAILABLE:
        if ray_address is not None:
            _require_ray()  # explicit cluster requested -> RayUnavailable
        log.warning("ray not installed; serial fallback for %d tenants", len(tenants))
        return {"mode": "serial", "tenants": _train_serial(tenants, **train_kwargs)}

    try:
        results = _train_ray(tenants, ray_address, **train_kwargs)
    except RayUnavailable:  # pragma: no cover - guarded above; defensive
        raise
    except Exception as exc:  # cluster unreachable -> degrade, don't fail
        log.warning("ray path unavailable (%s); serial fallback", exc)
        return {"mode": "serial", "tenants": _train_serial(tenants, **train_kwargs)}
    return {"mode": "ray", "tenants": results}


def main(argv: list[str] | None = None) -> int:
    """Multi-tenant training CLI over a root of A1 dataset dirs (SPEC-W33 §4)."""
    parser = argparse.ArgumentParser(
        prog="graph_ml.ray_train",
        description="Ray-parallel multi-tenant GraphSAGE trainer over A1 "
        "dataset dirs (serial fallback when ray is absent/unreachable — "
        "bit-identical results, GC5).",
    )
    parser.add_argument(
        "--all-tenants",
        action="store_true",
        help="discover + train every tenant dataset under --datasets-root",
    )
    parser.add_argument(
        "--datasets-root",
        default=os.getenv("RAY_DATASETS_ROOT", "./datasets"),
        help="root holding per-tenant A1 dataset dirs (default: env "
        "RAY_DATASETS_ROOT or ./datasets)",
    )
    parser.add_argument(
        "--head",
        choices=("link", "classifier"),
        default=os.getenv("GNN_HEAD", "link"),
        help="training head (default: env GNN_HEAD or 'link')",
    )
    parser.add_argument(
        "--seed",
        type=int,
        default=int(os.getenv("GRAPH_ML_SEED", "42")),
        help="base seed; per-tenant seed = base + tenant index (GC5 scheme)",
    )
    parser.add_argument("--epochs", type=int, default=None)
    parser.add_argument("--hidden-dim", type=int, default=None)
    parser.add_argument("--patience", type=int, default=None)
    parser.add_argument("--val-fraction", type=float, default=None)
    parser.add_argument(
        "--model-dir",
        default=os.getenv("GRAPH_ML_MODEL_DIR", "./models"),
        help="registry root (default: env GRAPH_ML_MODEL_DIR or ./models)",
    )
    parser.add_argument(
        "--ray-address",
        default=os.getenv("RAY_ADDRESS") or None,
        help="None = local cluster init; 'local' = forced serial fallback; "
        "otherwise an existing cluster address (multi-node is one address away)",
    )
    args = parser.parse_args(argv)

    if not args.all_tenants:
        parser.error("--all-tenants is required (per-tenant training is "
                     "python -m graph_ml.gnn_train)")

    try:
        result = train_all_tenants(
            args.datasets_root,
            head=args.head,
            seed=args.seed,
            model_dir=args.model_dir,
            epochs=args.epochs,
            hidden_dim=args.hidden_dim,
            patience=args.patience,
            val_fraction=args.val_fraction,
            ray_address=args.ray_address,
        )
    except RayUnavailable as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    json.dump(result, sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":  # pragma: no cover - CLI entry
    raise SystemExit(main())
