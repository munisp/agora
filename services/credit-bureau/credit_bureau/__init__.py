"""credit-bureau service package (SPEC-W33 §3 B2).

Owns the LEARNED credit scorer (``credit_bureau.ml``) and a faithful
Python port of the booking-service Go lending rule score
(``credit_bureau.rules``). Provenance constants (invariant I2): every
score payload carries ``model_version`` — ``credit-ml-v{N}`` when a
registry artifact produced it, ``heuristic-v1`` for the rule-only path.
"""

MODEL_VERSION_ML_PREFIX = "credit-ml-v"
MODEL_VERSION_HEURISTIC = "heuristic-v1"
FEATURE_SCHEMA = "fv1"
