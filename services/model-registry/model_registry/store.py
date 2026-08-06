"""Postgres store — THE single write path (I4 tenant isolation).

Driver: psycopg v3, SYNC (documented choice). The service workload is
low-QPS control-plane (register/promote/report) plus two batch schedulers;
sync psycopg keeps the transaction semantics (atomic promote, RLS GUCs via
set_config(..., is_local=true)) explicit and matches the slim-image deps.

RLS discipline: every transaction that touches tenant tables sets
`app.tenant_id` transaction-locally. Cross-tenant enumeration exists ONLY for
the internal batch jobs (drift sweep / nightly trainer) and uses
`app.registry_internal='on'` — HTTP request handlers never pass internal=True.
With neither GUC set the RLS policies are fail-closed (no rows visible).
"""

from __future__ import annotations

import logging
from contextlib import contextmanager
from datetime import datetime
from typing import Any, Iterator

import psycopg
from psycopg.rows import dict_row

log = logging.getLogger(__name__)

TENANT_TABLES_ERR = "RLS requires a tenant context"


class NotFound(Exception):
    """Requested row does not exist (or is not in the required stage)."""


class Conflict(Exception):
    """State conflict, e.g. single-production race (partial unique index)."""


def _row_to_dict(row: dict[str, Any]) -> dict[str, Any]:
    out = dict(row)
    for k in ("id", "tenant_id", "experiment_id"):
        if k in out and out[k] is not None:
            out[k] = str(out[k])
    return out


class RegistryStore:
    def __init__(self, dsn: str) -> None:
        self.dsn = dsn

    @contextmanager
    def _tx(self, tenant_id: str | None = None, *,
            internal: bool = False) -> Iterator[Any]:
        conn = psycopg.connect(self.dsn, row_factory=dict_row)
        try:
            with conn.transaction():
                if tenant_id is not None:
                    conn.execute(
                        "SELECT set_config('app.tenant_id', %s, true)",
                        (str(tenant_id),))
                if internal:
                    conn.execute(
                        "SELECT set_config('app.registry_internal', 'on', true)")
                yield conn
        finally:
            conn.close()

    # ------------------------------------------------------------------ health
    def health(self) -> bool:
        try:
            with psycopg.connect(self.dsn) as conn:
                conn.execute("SELECT 1")
            return True
        except Exception:  # noqa: BLE001 — honest health, never raise
            log.exception("health check failed")
            return False

    # ------------------------------------------------------------- model family
    def ensure_family(self, conn, name: str) -> None:
        conn.execute(
            "INSERT INTO model_family (name) VALUES (%s) ON CONFLICT DO NOTHING",
            (name,))

    def list_families(self) -> list[str]:
        with self._tx() as conn:
            rows = conn.execute(
                "SELECT name FROM model_family ORDER BY name").fetchall()
        return [r["name"] for r in rows]

    # ------------------------------------------------------------ model version
    def register_version(self, *, family: str, tenant_id: str,
                         artifact_uri: str, metrics: dict | None = None,
                         seed: int | None = None, dataset_hash: str | None = None,
                         git_sha: str | None = None,
                         version: int | None = None) -> dict[str, Any]:
        """Insert a new version in stage 'staging'. Version auto-assigned as
        max+1 per (family, tenant) under a family row lock (race-safe)."""
        import json as _json
        with self._tx(tenant_id) as conn:
            self.ensure_family(conn, family)
            # Lock the family row: serializes concurrent auto-version assigns.
            conn.execute("SELECT name FROM model_family WHERE name = %s FOR UPDATE",
                         (family,))
            if version is None:
                row = conn.execute(
                    "SELECT COALESCE(MAX(version), 0) + 1 AS next "
                    "FROM model_version WHERE family = %s AND tenant_id = %s",
                    (family, tenant_id)).fetchone()
                version = int(row["next"])
            row = conn.execute(
                "INSERT INTO model_version "
                "(family, tenant_id, version, artifact_uri, metrics, seed,"
                " dataset_hash, git_sha) "
                "VALUES (%s, %s, %s, %s, %s::jsonb, %s, %s, %s) RETURNING *",
                (family, tenant_id, version, artifact_uri,
                 _json.dumps(metrics or {}), seed, dataset_hash, git_sha)
            ).fetchone()
            return _row_to_dict(row)

    def get_production(self, family: str, tenant_id: str) -> dict[str, Any] | None:
        with self._tx(tenant_id) as conn:
            row = conn.execute(
                "SELECT * FROM model_version WHERE family = %s AND tenant_id = %s"
                " AND stage = 'production'", (family, tenant_id)).fetchone()
            return _row_to_dict(row) if row else None

    def get_version(self, family: str, tenant_id: str,
                    version: int) -> dict[str, Any] | None:
        with self._tx(tenant_id) as conn:
            row = conn.execute(
                "SELECT * FROM model_version WHERE family = %s AND tenant_id = %s"
                " AND version = %s", (family, tenant_id, version)).fetchone()
            return _row_to_dict(row) if row else None

    def list_versions(self, family: str, tenant_id: str) -> list[dict[str, Any]]:
        with self._tx(tenant_id) as conn:
            rows = conn.execute(
                "SELECT * FROM model_version WHERE family = %s AND tenant_id = %s"
                " ORDER BY version", (family, tenant_id)).fetchall()
            return [_row_to_dict(r) for r in rows]

    def list_productions(self) -> list[dict[str, Any]]:
        """All production rows across tenants — INTERNAL batch-job read only."""
        with self._tx(internal=True) as conn:
            rows = conn.execute(
                "SELECT * FROM model_version WHERE stage = 'production'"
                " ORDER BY family, tenant_id").fetchall()
            return [_row_to_dict(r) for r in rows]

    def promote(self, family: str, tenant_id: str,
                version: int) -> dict[str, Any]:
        """staging → production, atomically archiving the current production
        in ONE transaction. The partial unique index guarantees
        single-production even under races (GC1): a losing concurrent promote
        gets a UniqueViolation → Conflict (409).
        """
        try:
            with self._tx(tenant_id) as conn:
                conn.execute(
                    "UPDATE model_version SET stage = 'archived' "
                    "WHERE family = %s AND tenant_id = %s AND stage = 'production'",
                    (family, tenant_id))
                row = conn.execute(
                    "UPDATE model_version SET stage = 'production' "
                    "WHERE family = %s AND tenant_id = %s AND version = %s"
                    " AND stage = 'staging' RETURNING *",
                    (family, tenant_id, version)).fetchone()
                if row is None:
                    raise NotFound(
                        f"version {version} of {family}/{tenant_id} not in staging")
                return _row_to_dict(row)
        except psycopg.errors.UniqueViolation as exc:
            raise Conflict(
                f"concurrent promote for {family}/{tenant_id}: {exc}") from exc

    def rollback(self, family: str, tenant_id: str) -> dict[str, Any]:
        """Re-promote the most recent ARCHIVED version (created_at desc,
        version desc), archiving the current production in one transaction."""
        try:
            with self._tx(tenant_id) as conn:
                row = conn.execute(
                    "SELECT version FROM model_version "
                    "WHERE family = %s AND tenant_id = %s AND stage = 'archived' "
                    "ORDER BY created_at DESC, version DESC LIMIT 1 FOR UPDATE",
                    (family, tenant_id)).fetchone()
                if row is None:
                    raise NotFound(f"no archived version for {family}/{tenant_id}")
                target = int(row["version"])
                conn.execute(
                    "UPDATE model_version SET stage = 'archived' "
                    "WHERE family = %s AND tenant_id = %s AND stage = 'production'",
                    (family, tenant_id))
                row = conn.execute(
                    "UPDATE model_version SET stage = 'production' "
                    "WHERE family = %s AND tenant_id = %s AND version = %s"
                    " RETURNING *", (family, tenant_id, target)).fetchone()
                return _row_to_dict(row)
        except psycopg.errors.UniqueViolation as exc:
            raise Conflict(
                f"concurrent rollback for {family}/{tenant_id}: {exc}") from exc

    # ------------------------------------------------------------------ A/B
    def create_experiment(self, *, family: str, tenant_id: str,
                          champion_version: int, challenger_version: int,
                          pct: int, starts_at: datetime | None = None,
                          ends_at: datetime | None = None) -> dict[str, Any]:
        with self._tx(tenant_id) as conn:
            self.ensure_family(conn, family)
            row = conn.execute(
                "INSERT INTO experiments "
                "(family, tenant_id, champion_version, challenger_version, pct,"
                " starts_at, ends_at) "
                "VALUES (%s, %s, %s, %s, %s, COALESCE(%s, now()), %s) RETURNING *",
                (family, tenant_id, champion_version, challenger_version, pct,
                 starts_at, ends_at)).fetchone()
            return _row_to_dict(row)

    def get_experiment(self, experiment_id: str) -> dict[str, Any] | None:
        """Internal read (experiment id is the lookup key; tenant resolved
        from the row for subsequent scoped reads)."""
        with self._tx(internal=True) as conn:
            row = conn.execute(
                "SELECT * FROM experiments WHERE id = %s",
                (experiment_id,)).fetchone()
            return _row_to_dict(row) if row else None

    def get_active_experiment(self, family: str, tenant_id: str,
                              now: datetime) -> dict[str, Any] | None:
        with self._tx(tenant_id) as conn:
            row = conn.execute(
                "SELECT * FROM experiments "
                "WHERE family = %s AND tenant_id = %s AND status = 'active'"
                " AND starts_at <= %s AND (ends_at IS NULL OR ends_at > %s) "
                "ORDER BY starts_at DESC LIMIT 1",
                (family, tenant_id, now, now)).fetchone()
            return _row_to_dict(row) if row else None

    def stop_experiment(self, experiment_id: str,
                        tenant_id: str) -> dict[str, Any]:
        with self._tx(tenant_id) as conn:
            row = conn.execute(
                "UPDATE experiments SET status = 'stopped' WHERE id = %s"
                " RETURNING *", (experiment_id,)).fetchone()
            if row is None:
                raise NotFound(f"experiment {experiment_id} not found")
            return _row_to_dict(row)

    def record_outcome(self, *, experiment_id: str, tenant_id: str,
                       person_id: str, assigned_arm: str,
                       predicted_label: int, predicted_score: float,
                       true_label: int | None = None) -> dict[str, Any]:
        with self._tx(tenant_id) as conn:
            row = conn.execute(
                "INSERT INTO experiment_outcomes "
                "(experiment_id, tenant_id, person_id, assigned_arm,"
                " predicted_label, predicted_score, true_label) "
                "VALUES (%s, %s, %s, %s, %s, %s, %s) RETURNING *",
                (experiment_id, tenant_id, person_id, assigned_arm,
                 predicted_label, predicted_score, true_label)).fetchone()
            return _row_to_dict(row)

    def experiment_report(self, experiment_id: str,
                          tenant_id: str) -> list[dict[str, Any]]:
        """Per-arm precision/recall/Brier over LABELED outcomes (pure SQL)."""
        rows_sql = """
            SELECT assigned_arm,
                   count(*) FILTER (WHERE true_label IS NOT NULL) AS labeled,
                   count(*) AS total,
                   sum(CASE WHEN predicted_label = 1 AND true_label = 1
                            THEN 1 ELSE 0 END) AS tp,
                   sum(CASE WHEN predicted_label = 1 AND true_label = 0
                            THEN 1 ELSE 0 END) AS fp,
                   sum(CASE WHEN predicted_label = 0 AND true_label = 1
                            THEN 1 ELSE 0 END) AS fn,
                   avg(power(predicted_score - true_label, 2))
                       FILTER (WHERE true_label IS NOT NULL) AS brier
            FROM experiment_outcomes
            WHERE experiment_id = %s
            GROUP BY assigned_arm
        """
        with self._tx(tenant_id) as conn:
            rows = conn.execute(rows_sql, (experiment_id,)).fetchall()
        report = []
        for r in rows:
            tp, fp, fn = int(r["tp"] or 0), int(r["fp"] or 0), int(r["fn"] or 0)
            precision = tp / (tp + fp) if (tp + fp) else None
            recall = tp / (tp + fn) if (tp + fn) else None
            report.append({
                "arm": r["assigned_arm"],
                "total": int(r["total"]),
                "labeled": int(r["labeled"]),
                "precision": precision,
                "recall": recall,
                "brier": float(r["brier"]) if r["brier"] is not None else None,
            })
        return report

    # -------------------------------------------------- drift observation sinks
    def record_scores(self, *, family: str, tenant_id: str,
                      scores: list[float],
                      observed_at: datetime | None = None) -> int:
        with self._tx(tenant_id) as conn:
            for s in scores:
                conn.execute(
                    "INSERT INTO score_observations (family, tenant_id, score,"
                    " observed_at) VALUES (%s, %s, %s, COALESCE(%s, now()))",
                    (family, tenant_id, float(s), observed_at))
            return len(scores)

    def record_features(self, *, family: str, tenant_id: str,
                        features: dict[str, list[float]],
                        observed_at: datetime | None = None) -> int:
        n = 0
        with self._tx(tenant_id) as conn:
            for name, values in features.items():
                for v in values:
                    conn.execute(
                        "INSERT INTO feature_observations "
                        "(family, tenant_id, feature, value, observed_at) "
                        "VALUES (%s, %s, %s, %s, COALESCE(%s, now()))",
                        (family, tenant_id, name, float(v), observed_at))
                    n += 1
        return n

    def score_window(self, family: str, tenant_id: str,
                     start: datetime, end: datetime) -> list[float]:
        with self._tx(tenant_id) as conn:
            rows = conn.execute(
                "SELECT score FROM score_observations "
                "WHERE family = %s AND tenant_id = %s"
                " AND observed_at >= %s AND observed_at < %s",
                (family, tenant_id, start, end)).fetchall()
        return [float(r["score"]) for r in rows]

    def feature_window(self, family: str, tenant_id: str,
                       start: datetime, end: datetime) -> dict[str, list[float]]:
        with self._tx(tenant_id) as conn:
            rows = conn.execute(
                "SELECT feature, value FROM feature_observations "
                "WHERE family = %s AND tenant_id = %s"
                " AND observed_at >= %s AND observed_at < %s",
                (family, tenant_id, start, end)).fetchall()
        out: dict[str, list[float]] = {}
        for r in rows:
            out.setdefault(r["feature"], []).append(float(r["value"]))
        return out
