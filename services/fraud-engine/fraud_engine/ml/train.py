"""train.py — W33-B fraud learned-scorer training CLI (SPEC-W33 §3 B1).

    python -m fraud_engine.ml.train --dataset <dir-with-jsonl> --out <registry-dir>

Reads an A1 dataset dir (``events.jsonl`` / ``persons.jsonl`` /
``graph_edges.jsonl`` / ``labels.json`` / ``manifest.json`` from
``scripts/seeds/naija_transactions.py``), builds fv1 person vectors, and
writes versioned artifacts:

    {out}/fraud-ae-v{N}/model.pt + meta.json     (N = next free integer)
    {out}/fraud-clf-v{N}/model.pt + meta.json

LABEL JOIN (documented): ``labels.json`` is the ONLY ground truth. Person
rows in ``persons.jsonl`` carry ``fraud:false`` even for fraud-injected
persons (known A1 quirk), so person labels are derived by joining labels.json
``entity_id`` to persons three ways: (a) direct ``per-*`` person entries,
(b) ``evt-*`` entries via the event's ``person_id``, (c) ``edg-*`` entries
via the edge's actor (``agent_id`` / ``from_person_id`` / ``to_person_id`` /
``person_id``). A person touched by ANY ``fraud:true`` entry is a positive;
``benign_*`` entries are hard negatives (fraud=false, they only confirm the
default-0 label). Unlabeled persons are benign by GA2 label completeness.

The autoencoder trains on BENIGN vectors only (label 0, including the
``benign_*`` hard negatives — they are benign behaviour). The classifier
trains on ALL persons with the derived labels.

Determinism (GB1): single-threaded torch, ``seed_everything``,
``torch.use_deterministic_algorithms(True)`` where accepted, deterministic
90/10 splits, and meta.json carries NO wall-clock timestamps — two identical
invocations produce byte-equal meta.json and bit-identical final val loss.
``trained_at`` is intentionally the dataset manifest sha, not a timestamp
(I3: provenance binds to the dataset, not the clock).

meta.json: seed (SEED env / --seed, default 42), git sha ("unknown" when the
repo has no .git), dataset manifest sha256, feature_schema, val metrics,
training args, row/label counts. torch.save uses the default zipfile
serialization; loads always use map_location="cpu" (I5).
"""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import os
import subprocess
import sys
from typing import Any

from . import (
    DEFAULT_SEED,
    MODEL_VERSION_AE_PREFIX,
    MODEL_VERSION_CLF_PREFIX,
    _TORCH_AVAILABLE,
    _require_torch,
    next_model_version,
    resolve_device,
)
from .features import FEATURE_SCHEMA, build_feature_vector

if _TORCH_AVAILABLE:
    import torch
else:
    torch = None  # type: ignore[assignment]

log = logging.getLogger("fraud_engine.ml.train")

DATASET_FILES = ("events.jsonl", "persons.jsonl", "graph_edges.jsonl", "labels.json")


def _sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _git_sha() -> str:
    """Best-effort repo HEAD sha; "unknown" outside a git checkout."""
    try:
        out = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            capture_output=True,
            check=True,
            cwd=os.path.dirname(os.path.abspath(__file__)),
        )
        return out.stdout.decode().strip() or "unknown"
    except Exception:  # noqa: BLE001 - no .git in CI image / tarball deploys
        return "unknown"


def load_dataset(dataset_dir: str) -> dict[str, Any]:
    """Load an A1 dataset dir into aligned person vectors + labels.

    Returns dict with: person_ids (sorted), vectors (fv1 per person, aligned),
    labels (0/1 per person, aligned), fraud_count, benign_count,
    hard_negative_count, manifest_sha256, row_counts.
    """
    labels_path = os.path.join(dataset_dir, "labels.json")
    events_path = os.path.join(dataset_dir, "events.jsonl")
    persons_path = os.path.join(dataset_dir, "persons.jsonl")
    edges_path = os.path.join(dataset_dir, "graph_edges.jsonl")
    manifest_path = os.path.join(dataset_dir, "manifest.json")
    for path in (labels_path, events_path, persons_path, edges_path):
        if not os.path.isfile(path):
            raise FileNotFoundError(f"A1 dataset file missing: {path}")

    with open(labels_path, encoding="utf-8") as fh:
        label_entries = json.load(fh)["entries"]

    # Pass 1: events grouped by person; event_id -> person_id for the join.
    events_by_person: dict[str, list[dict[str, Any]]] = {}
    event_owner: dict[str, str] = {}
    n_events = 0
    with open(events_path, encoding="utf-8") as fh:
        for line in fh:
            row = json.loads(line)
            n_events += 1
            pid = str(row.get("person_id") or "")
            if not pid:
                continue
            events_by_person.setdefault(pid, []).append(row)
            eid = row.get("event_id")
            if eid:
                event_owner[str(eid)] = pid

    # Pass 2: referral degrees + edge actor map for the edg-* label join.
    referral_degree: dict[str, int] = {}
    edge_actors: dict[str, set[str]] = {}
    n_edges = 0
    with open(edges_path, encoding="utf-8") as fh:
        for line in fh:
            row = json.loads(line)
            n_edges += 1
            etype = str(row.get("edge_type") or "")
            actors: set[str] = set()
            for key in ("agent_id", "from_person_id", "to_person_id", "person_id"):
                val = row.get(key)
                if val:
                    actors.add(str(val))
            eid = row.get("edge_id")
            if eid:
                edge_actors[str(eid)] = actors
            if etype == "REFERRED":
                a, b = row.get("from_person_id"), row.get("to_person_id")
                if a:
                    referral_degree[str(a)] = referral_degree.get(str(a), 0) + 1
                if b:
                    referral_degree[str(b)] = referral_degree.get(str(b), 0) + 1

    # Person universe: persons.jsonl plus any event/edge actor (robustness).
    person_ids: set[str] = set()
    n_persons = 0
    with open(persons_path, encoding="utf-8") as fh:
        for line in fh:
            row = json.loads(line)
            n_persons += 1
            pid = row.get("person_id")
            if pid:
                person_ids.add(str(pid))
    person_ids |= set(events_by_person)
    for actors in edge_actors.values():
        person_ids |= actors

    # Label join (see module docstring). Positives OR together; benign_*
    # entries are hard negatives and never flip a positive back.
    fraud_persons: set[str] = set()
    hard_negatives: set[str] = set()
    for entry in label_entries:
        entity = str(entry.get("entity_id") or "")
        scenario = str(entry.get("scenario") or "")
        touched: set[str] = set()
        if entity.startswith("per-"):
            touched = {entity}
        elif entity.startswith("evt-"):
            owner = event_owner.get(entity)
            touched = {owner} if owner else set()
        elif entity.startswith("edg-"):
            touched = edge_actors.get(entity, set())
        if entry.get("fraud"):
            fraud_persons |= touched
        elif scenario.startswith("benign_"):
            hard_negatives |= touched
    hard_negatives -= fraud_persons  # a fraud touch always wins

    ordered = sorted(person_ids)
    vectors = [
        build_feature_vector(events_by_person.get(pid, []), referral_degree.get(pid, 0))
        for pid in ordered
    ]
    labels = [1 if pid in fraud_persons else 0 for pid in ordered]

    return {
        "person_ids": ordered,
        "vectors": vectors,
        "labels": labels,
        "fraud_count": sum(labels),
        "benign_count": len(labels) - sum(labels),
        "hard_negative_count": len(hard_negatives & set(ordered)),
        "manifest_sha256": (
            _sha256_file(manifest_path) if os.path.isfile(manifest_path) else "unknown"
        ),
        "row_counts": {
            "events": n_events,
            "persons": n_persons,
            "graph_edges": n_edges,
            "labels": len(label_entries),
        },
    }


def _write_artifact(
    out_dir: str,
    prefix: str,
    state_dict: dict[str, Any],
    meta: dict[str, Any],
) -> str:
    version = next_model_version(out_dir, prefix)
    artifact_dir = os.path.join(out_dir, version)
    os.makedirs(artifact_dir, exist_ok=True)
    torch.save(state_dict, os.path.join(artifact_dir, "model.pt"))
    meta = dict(meta, model_version=version, family=prefix.rstrip("-v"))
    with open(os.path.join(artifact_dir, "meta.json"), "w", encoding="utf-8") as fh:
        json.dump(meta, fh, indent=2, sort_keys=True)
        fh.write("\n")
    log.info("wrote %s -> %s", version, artifact_dir)
    return version


def train_models(
    dataset_dir: str,
    out_dir: str,
    seed: int = DEFAULT_SEED,
    device: str = "auto",
    ae_epochs: int | None = None,
    clf_epochs: int | None = None,
) -> dict[str, Any]:
    """Train FraudAE (benign-only) + FraudCLF (all labels); write artifacts.

    Returns a summary dict with both model_versions and val metrics.
    """
    _require_torch()
    try:
        torch.use_deterministic_algorithms(True)
    except Exception:  # noqa: BLE001 - older torch: CPU linear ops stay deterministic
        log.warning("use_deterministic_algorithms(True) unavailable; continuing")
    torch.set_num_threads(1)  # fixed reduction order -> GB1 bit-identical loss

    from .autoencoder import AE_MAX_EPOCHS, train_autoencoder
    from .classifier import CLF_MAX_EPOCHS, train_classifier

    resolved_device = resolve_device(device)
    data = load_dataset(dataset_dir)
    vectors, labels = data["vectors"], data["labels"]
    benign_vectors = [v for v, y in zip(vectors, labels) if y == 0]
    if len(benign_vectors) < 10:
        raise ValueError(
            f"dataset too small: {len(benign_vectors)} benign vectors (< 10)"
        )

    ae_model, ae_hist = train_autoencoder(
        benign_vectors, seed=seed, device=resolved_device,
        epochs=ae_epochs or AE_MAX_EPOCHS,
    )
    clf_model, clf_hist = train_classifier(
        vectors, labels, seed=seed, device=resolved_device,
        epochs=clf_epochs or CLF_MAX_EPOCHS,
    )

    base_meta: dict[str, Any] = {
        "seed": seed,
        "git_sha": _git_sha(),
        "dataset_manifest_sha256": data["manifest_sha256"],
        "feature_schema": FEATURE_SCHEMA,
        "device": resolved_device,
        "row_counts": data["row_counts"],
        "label_counts": {
            "persons": len(labels),
            "fraud": data["fraud_count"],
            "benign": data["benign_count"],
            "benign_hard_negatives": data["hard_negative_count"],
        },
    }
    os.makedirs(out_dir, exist_ok=True)
    ae_version = _write_artifact(
        out_dir,
        MODEL_VERSION_AE_PREFIX,
        ae_model.state_dict(),
        dict(
            base_meta,
            training_args={
                "epochs_max": ae_epochs or AE_MAX_EPOCHS,
                "lr": 1e-3,
                "patience": 20,
                "val_fraction": 0.10,
                "loss": "mse",
                "architecture": "16-8-3-8-16",
                "trained_on": "benign_only",
            },
            val_metrics={
                "final_train_loss": ae_hist["final_train_loss"],
                "final_val_loss": ae_hist["final_val_loss"],
                "epochs_run": ae_hist["epochs_run"],
                "stopped_early": ae_hist["stopped_early"],
            },
            ae_error_stats={"err_min": ae_hist["err_min"], "err_max": ae_hist["err_max"]},
        ),
    )
    clf_version = _write_artifact(
        out_dir,
        MODEL_VERSION_CLF_PREFIX,
        clf_model.state_dict(),
        dict(
            base_meta,
            training_args={
                "epochs_max": clf_epochs or CLF_MAX_EPOCHS,
                "lr": 1e-3,
                "patience": 20,
                "val_fraction": 0.10,
                "loss": "bce_with_logits",
                "architecture": "16-32-16-1",
                "trained_on": "all_labels",
            },
            val_metrics={
                "final_train_loss": clf_hist["final_train_loss"],
                "final_val_loss": clf_hist["final_val_loss"],
                "epochs_run": clf_hist["epochs_run"],
                "stopped_early": clf_hist["stopped_early"],
                "auc_pr": clf_hist["val_auc_pr"],
                "auc_roc": clf_hist["val_auc_roc"],
                "brier": clf_hist["val_brier"],
                "val_positives": clf_hist["val_positives"],
                "val_samples": clf_hist["val_samples"],
            },
        ),
    )
    return {
        "ae_version": ae_version,
        "clf_version": clf_version,
        "ae_val_metrics": ae_hist,
        "clf_val_metrics": clf_hist,
        "out_dir": out_dir,
        "device": resolved_device,
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m fraud_engine.ml.train",
        description="Train the W33-B fraud learned scorer (FraudAE + FraudCLF) "
        "on an A1 labeled dataset and write versioned registry artifacts.",
    )
    parser.add_argument("--dataset", required=True, help="A1 dataset dir (jsonl files)")
    parser.add_argument("--out", required=True, help="registry output dir (tenant or _global)")
    parser.add_argument(
        "--seed",
        type=int,
        default=None,
        help="training seed (default: SEED env, else 42)",
    )
    parser.add_argument("--device", default="auto", help="auto|cpu|cuda (default: auto)")
    parser.add_argument("--ae-epochs", type=int, default=None, help="override AE max epochs")
    parser.add_argument("--clf-epochs", type=int, default=None, help="override CLF max epochs")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    logging.basicConfig(level=os.getenv("LOG_LEVEL", "INFO"))
    seed = args.seed if args.seed is not None else int(os.getenv("SEED", str(DEFAULT_SEED)))
    summary = train_models(
        args.dataset,
        args.out,
        seed=seed,
        device=args.device,
        ae_epochs=args.ae_epochs,
        clf_epochs=args.clf_epochs,
    )
    print(json.dumps(summary, indent=2, sort_keys=True, default=str))
    return 0


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
