"""Scoring sweep orchestration (SPEC-W29 §3 WS-A).

``run_sweep`` discovers tenants, scores each one in isolation (one tenant's
failure never kills the sweep — SPEC-W29 §3 test requirement), and writes
results back through the graph-service internal API only. Also usable as a
cron-style CLI entry: ``python -m graph_ml.score [--tenant T]``.
"""

from __future__ import annotations

import argparse
import logging
import sys
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from datetime import datetime, timezone

from . import MODEL_VERSION_HEURISTIC
from .config import Settings, load_settings
from .extract import GraphClient
from .gnn import resolve_backend
from .heuristic import RecommendationRecord, ScoreRecord, score_tenant
from .writeback import WritebackClient

log = logging.getLogger(__name__)


@dataclass
class TenantRunResult:
    tenant_id: str
    status: str  # "ok" | "error" | "skipped"
    persons_scored: int = 0
    recommendations: int = 0
    model_version: str = ""
    error: str | None = None
    started_at: str = ""
    finished_at: str = ""


@dataclass
class SweepResult:
    backend: str
    started_at: str
    finished_at: str = ""
    tenants: list[TenantRunResult] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return all(t.status != "error" for t in self.tenants)


def _utcnow_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def score_one_tenant(
    settings: Settings,
    graph_client: GraphClient,
    writer: WritebackClient | None,
    tenant_id: str,
    backend: str,
) -> TenantRunResult:
    """Extract -> feature -> score -> write back for ONE tenant, isolated."""
    result = TenantRunResult(tenant_id=tenant_id, status="ok", started_at=_utcnow_iso())
    try:
        graph = graph_client.fetch_tenant_graph(tenant_id)
        if backend == "gnn":  # pragma: no cover - requires torch
            from .gnn import GraphSAGEBackend

            gnn = GraphSAGEBackend(settings.model_dir)
            try:
                scores, recommendations = gnn.score_tenant(
                    graph, now=datetime.now(timezone.utc), top_k=settings.top_k
                )
                model_version = gnn.model_version
            except NotImplementedError:
                # Training/inference lands with the W31 GPU profile. Degrade
                # to heuristic for this tenant rather than erroring every
                # tenant in the sweep when torch/PyG happen to be installed.
                log.warning(
                    "gnn backend selected but training is not implemented "
                    "yet; scoring tenant with heuristic backend",
                    extra={"tenant_id": tenant_id},
                )
                scores, recommendations = score_tenant(
                    graph,
                    now=datetime.now(timezone.utc),
                    top_k=settings.top_k,
                    model_version=MODEL_VERSION_HEURISTIC,
                )
                model_version = MODEL_VERSION_HEURISTIC
        else:
            scores, recommendations = score_tenant(
                graph,
                now=datetime.now(timezone.utc),
                top_k=settings.top_k,
                model_version=MODEL_VERSION_HEURISTIC,
            )
            model_version = MODEL_VERSION_HEURISTIC

        if writer is not None:
            writer.post_scores(tenant_id, [s.as_payload() for s in scores])
            writer.post_recommendations(
                tenant_id, [r.as_payload() for r in recommendations]
            )

        result.persons_scored = len(scores)
        result.recommendations = len(recommendations)
        result.model_version = model_version
    except Exception as exc:  # noqa: BLE001 - isolation: log, record, continue
        result.status = "error"
        result.error = f"{type(exc).__name__}: {exc}"
        log.exception("tenant scoring failed", extra={"tenant_id": tenant_id})
    finally:
        result.finished_at = _utcnow_iso()
    return result


def run_sweep(
    settings: Settings,
    graph_client: GraphClient,
    writer: WritebackClient | None,
    tenant_id: str | None = None,
) -> SweepResult:
    """Full per-tenant sweep (or a single tenant when tenant_id is given)."""
    backend = resolve_backend(settings)
    sweep = SweepResult(backend=backend, started_at=_utcnow_iso())

    if tenant_id:
        tenants = [tenant_id]
    else:
        try:
            tenants = graph_client.list_tenants()
        except Exception as exc:  # noqa: BLE001
            log.exception("tenant discovery failed")
            sweep.tenants.append(
                TenantRunResult(
                    tenant_id="*",
                    status="error",
                    error=f"tenant discovery: {type(exc).__name__}: {exc}",
                    started_at=sweep.started_at,
                    finished_at=_utcnow_iso(),
                )
            )
            sweep.finished_at = _utcnow_iso()
            return sweep

    workers = max(1, settings.tenant_concurrency)
    if workers == 1 or len(tenants) <= 1:
        for tid in tenants:
            sweep.tenants.append(
                score_one_tenant(settings, graph_client, writer, tid, backend)
            )
    else:
        # TENANT_CONCURRENCY: bounded parallelism, still per-tenant isolated.
        with ThreadPoolExecutor(max_workers=workers) as pool:
            futures = [
                pool.submit(score_one_tenant, settings, graph_client, writer, tid, backend)
                for tid in tenants
            ]
            for fut in futures:
                sweep.tenants.append(fut.result())

    sweep.finished_at = _utcnow_iso()
    return sweep


def main(argv: list[str] | None = None) -> int:
    """Cron-style CLI: ``python -m graph_ml.score [--tenant T]``."""
    parser = argparse.ArgumentParser(prog="graph_ml.score")
    parser.add_argument("--tenant", default=None, help="score one tenant only")
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="compute scores but skip graph-service write-back",
    )
    args = parser.parse_args(argv)

    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s %(message)s")
    settings = load_settings()

    from .extract import client_from_settings
    from .writeback import HttpWritebackClient

    graph_client = client_from_settings(settings)
    writer = None if args.dry_run else HttpWritebackClient(
        base_url=settings.graph_service_url,
        internal_token=settings.internal_token,
        chunk_size=settings.writeback_chunk_size,
        timeout_s=settings.http_timeout_s,
    )
    try:
        sweep = run_sweep(settings, graph_client, writer, tenant_id=args.tenant)
    finally:
        if writer is not None:
            writer.close()
        graph_client.close()

    for t in sweep.tenants:
        log.info(
            "tenant result",
            extra={
                "tenant_id": t.tenant_id,
                "status": t.status,
                "persons": t.persons_scored,
                "recommendations": t.recommendations,
                "error": t.error,
            },
        )
    # Degraded GNN still exits 0 (SPEC-W29 §4 gate 5); only real failures exit 1.
    return 0 if sweep.ok else 1


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
