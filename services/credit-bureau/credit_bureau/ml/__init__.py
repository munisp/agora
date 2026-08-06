"""credit_bureau.ml — learned credit scorer (SPEC-W33 §3 B2).

Import-safe WITHOUT torch (invariant I5): ``model``/``train``/``scorer``
guard the torch import exactly like services/graph-ml/graph_ml/gnn.py;
the base service image runs rules-only.
"""
