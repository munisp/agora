"""Prometheus metrics (workforce conventions: /metrics on the sidecar port)."""

from __future__ import annotations

from prometheus_client import Counter, Histogram

http_requests = Counter(
    "graph_http_requests_total",
    "HTTP requests by route and status",
    ["method", "route", "status"],
)
graph_queries = Counter(
    "graph_queries_total",
    "Graph queries executed",
    ["kind", "result"],
)
graph_query_latency = Histogram(
    "graph_query_duration_seconds",
    "Graph query latency",
    ["kind"],
    buckets=(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0),
)
ask_requests = Counter(
    "graph_ask_requests_total",
    "NL->Cypher ask outcomes",
    ["result"],  # ok | unavailable | unanswerable | invalid_params
)
audience_members = Histogram(
    "graph_audience_members",
    "Materialized audience sizes",
    buckets=(0, 1, 5, 10, 25, 50, 100, 250, 500, 1000, 5000, 10000),
)
