# credit-bureau (SPEC-W33 §3 B2)

Credit scoring service: a **faithful Python port of the booking-service Go
lending rule score** plus a **real PyTorch learned scorer** (CreditMLP:
regression head → bureau score 300–900, classification head →
P(default-in-12m)), blended `0.6*ml + 0.4*rule`, clamped to [300,900].

## Honest status (read first)

- **Rule port source:** `services/booking-service/internal/lending/lending.go:388-426`.
  The Go rule returns a **naive 0..100** score from 3 signals (tenure days,
  completed bookings, repaid loans) and **emits NO reason codes** — the
  "W24 reason codes" do not exist in this repo (SPEC-W24 never touched
  scoring). The port is bit-identical and returns `reasons: []` rather
  than inventing codes. The 300..900 bureau scale is an additive affine
  mapping (`300 + 6*naive`), documented in `credit_bureau/rules.py`.
- **Training labels are SYNTHETIC (I3).** A1
  (`scripts/seeds/naija_transactions.py`) carries no lending outcomes;
  `credit_bureau/ml/train.py` derives default-in-12m labels and score
  targets from a documented, seeded synthetic outcome model (see its
  module docstring + `synthetic_outcome_model` in every meta.json). All
  reported metrics (val MAE on score, AUC-PR/Brier on the default head)
  are validation-set numbers **against synthetic labels** — stated as
  synthetic, never as real-world accuracy.
- **Fallback (I1).** No artifact for the tenant (or torch absent) → the
  API answers the pure ported rule score with `model_version:
  "heuristic-v1"`, byte-stable for a fixed payload. The booking-service
  Go sidecar client (`internal/lending/score_client.go`) fails closed to
  the local Go `Score()` on unset URL / >500ms timeout / non-200 /
  malformed JSON.
- **Provenance (I2).** Every response carries `model_version`
  (`credit-ml-v{N}` from the registry, else `heuristic-v1`),
  `ml_contribution` (blend delta), `feature_schema: "fv1"`.

## API

- `POST /v1/credit/score` — body `{signals: {tenure_days,
  completed_bookings, repaid_loans}, features?: {...fv1 raw signals}}`.
  Tenant via `X-Tenant-Id` (dev mode) or Bearer JWT `sub` (when
  `JWT_PUBLIC_KEY` is set) — same seam as graph-service (I4).
- `GET /healthz`.

## Config (env)

| Var | Default | Meaning |
| --- | --- | --- |
| `PORT` / `HOST` | `7022` / `0.0.0.0` | HTTP bind |
| `CREDIT_ML_REGISTRY_DIR` | empty (ML OFF) | registry root; resolution `{dir}/{tenant}/credit-ml-v{N}` then `{dir}/global/…` |
| `JWT_PUBLIC_KEY` / `JWT_ALGORITHM` | empty / `HS256` | workforce auth seam |

## Training (bootstrap, CPU, deterministic)

```bash
python scripts/seeds/naija_transactions.py --seed 42 --out data --persons 800 --days 120
cd services/credit-bureau
pip install -r requirements-ml.txt   # torch overlay (I5: never in base image)
python -m credit_bureau.ml.train \
  --dataset ../../data/naija_txn/42 --out ./models
# → models/credit-ml-v1/model.pt + meta.json (seed, git sha, dataset hash,
#   fv1 schema, val MAE/AUC-PR/Brier, synthetic outcome model constants)
```

Determinism (gate GB6/GB1): same seed + same dataset → byte-equal
meta.json and bit-identical val_loss (seeded torch/numpy, single torch
thread, fixed epochs, no timestamps in meta.json).

## Tests

```bash
pip install -r requirements.txt        # rules-only suite works without torch
pytest                                 # ML tests skip cleanly w/o torch
```
