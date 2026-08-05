"""graph-ml — per-tenant propensity scoring & offering recommendations (SPEC-W29 §3 WS-A).

Batch scorer over the tenant knowledge graph (FalkorDB). Heuristic-first
(numpy only); optional GraphSAGE backend when GRAPH_ML_BACKEND=gnn and
torch/torch-geometric are installed. Never writes FalkorDB directly — all
writes go through the graph-service internal API with X-Internal-Token.
"""

__version__ = "0.1.0"

MODEL_VERSION_HEURISTIC = "heuristic-v1"
MODEL_VERSION_GNN_PREFIX = "graphsage-v"
