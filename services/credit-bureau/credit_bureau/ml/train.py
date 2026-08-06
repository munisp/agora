"""Training CLI for the learned credit scorer (SPEC-W33 §3 B2).

Usage (mirrors the fraud pattern / graph-ml conventions):

    python -m credit_bureau.ml.train --dataset <dir> --out <registry-dir>

``<dir>`` is an A1 dataset dir (``scripts/seeds/naija_transactions.py``
output: ``events.jsonl`` + ``persons.jsonl`` + ``manifest.json``).
Writes ``{out}/credit-ml-v{N}/model.pt`` + ``meta.json``.

SYNTHETIC OUTCOME MODEL (honest metrics, invariant I3 — stated as
synthetic everywhere): A1 carries no lending outcomes, so default-in-12m
labels and score targets are DERIVED from the documented behavioral
proxies of ``features.derive_signals_from_events``:

    logit(p_default) = -2.0 + 2.4*utilization + 1.6*dpd_max_12m
                       + 1.2*dpd_count_12m - 2.0*on_time_rate
                       - 0.9*income_band + 0.5*(1 - tenure_months)
    (all features fv1-normalized to [0,1])
    default ~ Bernoulli(p_default)          [seeded numpy RandomState]
    score_target = clip(900 - 600*p_default + N(0, 15), 300, 900)
                                            [same seeded stream]

The constants above are recorded verbatim in meta.json together with the
dataset hash and seed, so every accuracy number is reproducible and is
labeled ``label_provenance: "synthetic"``.

DETERMINISM (gate GB6/GB1): two invocations with the same seed and the
same dataset produce byte-equal meta.json and bit-identical val_loss —
torch + numpy seeded, single torch thread, fixed epoch count, no
timestamps or absolute paths in meta.json.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import os
import re
import subprocess
import sys
from typing import Any

import numpy as np

from .. import MODEL_VERSION_ML_PREFIX
from .features import (
    FEATURE_DIM,
    build_feature_vector,
    derive_signals_from_events,
    feature_schema_payload,
)
from .model import MLBackendUnavailable, TORCH_AVAILABLE, new_model

if TORCH_AVAILABLE:
    import torch

log = logging.getLogger(__name__)

# Synthetic outcome-model constants (documented in the module docstring;
# recorded verbatim into meta.json — I3).
OUTCOME_MODEL = {
    "kind": "logistic_default_plus_affine_score",
    "label_provenance": "synthetic",
    "description": (
        "A1 carries no lending outcomes; default-in-12m ~ Bernoulli(sigmoid("
        "logit)) over fv1-normalized proxies, score target = clip(900 - "
        "600*p_default + N(0, noise_sd), 300, 900); seeded numpy stream."
    ),
    "logit_intercept": -2.0,
    "logit_weights": {
        "utilization": 2.4,
        "dpd_max_12m": 1.6,
        "dpd_count_12m": 1.2,
        "on_time_rate": -2.0,
        "income_band": -0.9,
        "one_minus_tenure_months": 0.5,
    },
    "score_noise_sd": 15.0,
}

DATASET_FILES = ("events.jsonl", "persons.jsonl", "manifest.json")


def dataset_sha256(dataset_dir: str) -> str:
    """Order-stable hash over the A1 dataset files (I3 provenance)."""
    h = hashlib.sha256()
    for name in sorted(DATASET_FILES):
        path = os.path.join(dataset_dir, name)
        if not os.path.isfile(path):
            continue
        fh = hashlib.sha256()
        with open(path, "rb") as f:
            for chunk in iter(lambda: f.read(1 << 20), b""):
                fh.update(chunk)
        h.update(name.encode())
        h.update(fh.hexdigest().encode())
    return h.hexdigest()


def git_sha() -> str:
    """Best-effort provenance: git rev-parse, else GIT_SHA env, else a
    stable 'unavailable' marker (never a timestamp — determinism)."""
    try:
        out = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            capture_output=True,
            text=True,
            timeout=5,
        )
        if out.returncode == 0 and out.stdout.strip():
            return out.stdout.strip()
    except (OSError, subprocess.SubprocessError):
        pass
    return os.getenv("GIT_SHA", "") or "unavailable"


def next_model_version(registry_dir: str) -> str:
    """Next versioned artifact dir name: credit-ml-v{N+1}."""
    highest = 0
    if os.path.isdir(registry_dir):
        pattern = re.compile(rf"^{re.escape(MODEL_VERSION_ML_PREFIX)}(\d+)$")
        for entry in os.listdir(registry_dir):
            match = pattern.match(entry)
            if match:
                highest = max(highest, int(match.group(1)))
    return f"{MODEL_VERSION_ML_PREFIX}{highest + 1}"


def load_dataset(dataset_dir: str, seed: int) -> tuple[np.ndarray, np.ndarray, np.ndarray, dict[str, int]]:
    """Build (X, y_score, y_default, counts) from an A1 dataset dir.

    Labels are derived by the DOCUMENTED synthetic outcome model (module
    docstring / OUTCOME_MODEL) from a seeded numpy stream — deterministic
    for a given (dataset, seed).
    """
    persons: list[dict[str, Any]] = []
    with open(os.path.join(dataset_dir, "persons.jsonl"), encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line:
                persons.append(json.loads(line))

    horizon_days = 180
    manifest_path = os.path.join(dataset_dir, "manifest.json")
    if os.path.isfile(manifest_path):
        with open(manifest_path, encoding="utf-8") as f:
            manifest = json.load(f)
        horizon_days = int(manifest.get("days", horizon_days))

    events_by_person: dict[str, list[dict[str, Any]]] = {}
    with open(os.path.join(dataset_dir, "events.jsonl"), encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            ev = json.loads(line)
            pid = ev.get("person_id")
            if pid:
                events_by_person.setdefault(pid, []).append(ev)

    rng = np.random.RandomState(seed)
    w = OUTCOME_MODEL["logit_weights"]
    x_rows: list[np.ndarray] = []
    y_score: list[float] = []
    y_default: list[float] = []
    for person in persons:
        events = events_by_person.get(person["person_id"], [])
        signals = derive_signals_from_events(person, events, horizon_days)
        vec = build_feature_vector(signals)
        # Feature order is frozen fv1: utilization=0, dpd_max=1,
        # dpd_count=2, on_time=3, income_band=4, tenure=5.
        logit = (
            OUTCOME_MODEL["logit_intercept"]
            + w["utilization"] * float(vec[0])
            + w["dpd_max_12m"] * float(vec[1])
            + w["dpd_count_12m"] * float(vec[2])
            + w["on_time_rate"] * float(vec[3])
            + w["income_band"] * float(vec[4])
            + w["one_minus_tenure_months"] * (1.0 - float(vec[5]))
        )
        p_default = 1.0 / (1.0 + np.exp(-logit))
        default = float(rng.rand() < p_default)
        score = float(
            np.clip(
                900.0 - 600.0 * p_default + rng.normal(0.0, OUTCOME_MODEL["score_noise_sd"]),
                300.0,
                900.0,
            )
        )
        x_rows.append(vec)
        y_score.append(score)
        y_default.append(default)

    counts = {
        "persons": len(persons),
        "events": sum(len(v) for v in events_by_person.values()),
        "defaults": int(sum(y_default)),
    }
    return (
        np.stack(x_rows).astype(np.float32),
        np.asarray(y_score, dtype=np.float32),
        np.asarray(y_default, dtype=np.float32),
        counts,
    )


def average_precision(y_true: np.ndarray, y_score: np.ndarray) -> float:
    """AUC-PR (average precision), numpy-only — no sklearn dependency."""
    order = np.argsort(-y_score, kind="stable")
    y = y_true[order]
    positives = float(y.sum())
    if positives == 0.0:
        return 0.0
    tp = np.cumsum(y)
    precision = tp / (np.arange(len(y)) + 1.0)
    recall = tp / positives
    ap = 0.0
    prev_recall = 0.0
    for i in range(len(y)):
        ap += (recall[i] - prev_recall) * precision[i]
        prev_recall = recall[i]
    return float(ap)


def brier_score(y_true: np.ndarray, y_prob: np.ndarray) -> float:
    return float(np.mean((y_prob - y_true) ** 2))


def train(
    dataset_dir: str,
    registry_dir: str,
    *,
    seed: int = 42,
    epochs: int = 200,
    hidden_dim: int = 32,
    lr: float = 1e-3,
) -> dict[str, Any]:
    """Train CreditMLP and write the versioned registry artifact.

    Returns the meta.json payload. Deterministic for (dataset, seed,
    hyperparams) — gate GB6/GB1.
    """
    if not TORCH_AVAILABLE:
        raise MLBackendUnavailable(
            "torch is not installed — install requirements-ml.txt to train the learned scorer"
        )
    torch.set_num_threads(1)  # bit-identical CPU linear algebra
    torch.manual_seed(seed)
    np.random.seed(seed)

    X, y_score, y_default, counts = load_dataset(dataset_dir, seed)
    n = len(X)
    if n < 10:
        raise ValueError(f"dataset too small to train honestly ({n} persons)")

    perm = np.random.RandomState(seed).permutation(n)
    n_val = max(1, n // 5)
    val_idx, train_idx = perm[:n_val], perm[n_val:]

    x_train = torch.from_numpy(X[train_idx])
    ys_train = torch.from_numpy(y_score[train_idx])
    yd_train = torch.from_numpy(y_default[train_idx])
    x_val = torch.from_numpy(X[val_idx])
    ys_val = torch.from_numpy(y_score[val_idx])
    yd_val = torch.from_numpy(y_default[val_idx])

    model = new_model(input_dim=FEATURE_DIM, hidden_dim=hidden_dim, seed=seed)
    opt = torch.optim.Adam(model.parameters(), lr=lr)
    bce = torch.nn.BCEWithLogitsLoss()

    for _ in range(epochs):
        model.train()
        opt.zero_grad()
        score_hat, logit_hat = model(x_train)
        mse = torch.mean(((score_hat - ys_train) / 600.0) ** 2)
        loss = mse + bce(logit_hat, yd_train)
        loss.backward()
        opt.step()

    model.eval()
    with torch.no_grad():
        sv, lv = model(x_val)
        val_mse = torch.mean(((sv - ys_val) / 600.0) ** 2)
        val_loss = float(val_mse + bce(lv, yd_val))
        val_mae = float(torch.mean(torch.abs(sv - ys_val)))
        val_prob = torch.sigmoid(lv).numpy()
        st, lt = model(x_train)
        train_loss = float(torch.mean(((st - ys_train) / 600.0) ** 2) + bce(lt, yd_train))

    y_val_np = y_default[val_idx]
    metrics = {
        "train_loss": train_loss,
        "val_loss": val_loss,
        "val_mae_score": val_mae,
        "val_auc_pr": average_precision(y_val_np, val_prob),
        "val_brier": brier_score(y_val_np, val_prob),
        "train_rows": int(len(train_idx)),
        "val_rows": int(n_val),
        "val_default_rate": float(y_val_np.mean()),
    }

    version = next_model_version(registry_dir)
    artifact_dir = os.path.join(registry_dir, version)
    os.makedirs(artifact_dir, exist_ok=True)
    torch.save(
        {
            "state_dict": model.state_dict(),
            "input_dim": FEATURE_DIM,
            "hidden_dim": hidden_dim,
            "feature_schema": "fv1",
        },
        os.path.join(artifact_dir, "model.pt"),
    )

    meta: dict[str, Any] = {
        "model_family": "credit-ml",
        "model_version": version,
        "label_provenance": "synthetic",
        "seed": seed,
        "git_sha": git_sha(),
        "dataset": {
            "kind": "naija_transactions_a1",
            "sha256": dataset_sha256(dataset_dir),
            "row_counts": counts,
        },
        "feature_schema": feature_schema_payload(),
        "synthetic_outcome_model": OUTCOME_MODEL,
        "hyperparams": {
            "epochs": epochs,
            "hidden_dim": hidden_dim,
            "lr": lr,
            "loss": "mse(score/600) + bce_with_logits(default)",
            "optimizer": "adam",
            "split": "80/20 seeded permutation",
        },
        "metrics": metrics,
    }
    with open(os.path.join(artifact_dir, "meta.json"), "w", encoding="utf-8") as f:
        json.dump(meta, f, indent=2, sort_keys=True)
        f.write("\n")
    log.info("wrote %s (val_mae=%.2f auc_pr=%.3f brier=%.3f)",
             artifact_dir, metrics["val_mae_score"], metrics["val_auc_pr"], metrics["val_brier"])
    return meta


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dataset", required=True, help="A1 dataset dir (events/persons/manifest jsonl)")
    parser.add_argument("--out", required=True, help="registry root; artifact lands in <out>/credit-ml-v{N}/")
    parser.add_argument("--seed", type=int, default=int(os.getenv("SEED", "42")))
    parser.add_argument("--epochs", type=int, default=200)
    parser.add_argument("--hidden-dim", type=int, default=32)
    parser.add_argument("--lr", type=float, default=1e-3)
    args = parser.parse_args(argv)

    logging.basicConfig(level=logging.INFO, format="[credit-train] %(message)s")
    meta = train(
        args.dataset,
        args.out,
        seed=args.seed,
        epochs=args.epochs,
        hidden_dim=args.hidden_dim,
        lr=args.lr,
    )
    m = meta["metrics"]
    print(
        f"[credit-train] {meta['model_version']}: val_mae={m['val_mae_score']:.2f} "
        f"auc_pr={m['val_auc_pr']:.3f} brier={m['val_brier']:.3f} "
        f"(labels: SYNTHETIC — see meta.json synthetic_outcome_model)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
