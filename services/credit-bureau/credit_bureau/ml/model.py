"""CreditMLP (SPEC-W33 §3 B2): shared MLP trunk with two heads.

  * regression head     — bureau score in [300,900]
                          (sigmoid * 600 + 300, so the blend clamp is
                          never reachable from the ML side alone);
  * classification head — P(default within 12 months), logit output.

Import-guarded torch (invariant I5, same idiom as
services/graph-ml/graph_ml/gnn.py): the module imports cleanly without
torch; constructing a model raises ``MLBackendUnavailable``.
"""

from __future__ import annotations

from typing import Any

from .features import FEATURE_DIM

try:  # torch is the requirements-ml.txt overlay only
    import torch
    from torch import nn

    TORCH_AVAILABLE = True
except ImportError:  # the normal case in rules-only deployments
    torch = None  # type: ignore[assignment]
    nn = None  # type: ignore[assignment]
    TORCH_AVAILABLE = False

SCORE_MIN = 300.0
SCORE_MAX = 900.0


class MLBackendUnavailable(RuntimeError):
    """Raised when the learned path is exercised without torch."""


if TORCH_AVAILABLE:

    class CreditMLP(nn.Module):
        """12 -> hidden -> hidden trunk; score + default heads."""

        def __init__(self, input_dim: int = FEATURE_DIM, hidden_dim: int = 32) -> None:
            super().__init__()
            self.input_dim = input_dim
            self.hidden_dim = hidden_dim
            self.trunk = nn.Sequential(
                nn.Linear(input_dim, hidden_dim),
                nn.ReLU(),
                nn.Linear(hidden_dim, hidden_dim // 2),
                nn.ReLU(),
            )
            self.score_head = nn.Linear(hidden_dim // 2, 1)
            self.default_head = nn.Linear(hidden_dim // 2, 1)

        def forward(self, x: Any) -> tuple[Any, Any]:
            """Returns (score in [300,900], default logit)."""
            h = self.trunk(x)
            score = torch.sigmoid(self.score_head(h)) * (SCORE_MAX - SCORE_MIN) + SCORE_MIN
            default_logit = self.default_head(h)
            return score.squeeze(-1), default_logit.squeeze(-1)

else:

    class CreditMLP:  # type: ignore[no-redef]
        """Torch-absent stub: importable, unusable (I5)."""

        def __init__(self, *args: Any, **kwargs: Any) -> None:
            raise MLBackendUnavailable(
                "torch is not installed — install requirements-ml.txt for the learned scorer"
            )


def new_model(input_dim: int = FEATURE_DIM, hidden_dim: int = 32, seed: int = 42) -> Any:
    """Seeded model construction (deterministic init, gate GB6)."""
    if not TORCH_AVAILABLE:
        raise MLBackendUnavailable(
            "torch is not installed — install requirements-ml.txt for the learned scorer"
        )
    torch.manual_seed(seed)
    return CreditMLP(input_dim=input_dim, hidden_dim=hidden_dim)
