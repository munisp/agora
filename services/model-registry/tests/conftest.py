r"""Test fixtures: REAL embedded Postgres 16 (pgserver), migration applied
VERBATIM via psql stdin (handles \c + DO blocks exactly like the postgres
docker-entrypoint-initdb.d path), RLS exercised with the real app role.
"""

from __future__ import annotations

import os
import uuid
from pathlib import Path

import psycopg
import pytest

os.environ.setdefault("XDG_RUNTIME_DIR", "/tmp/xdg")
Path("/tmp/xdg").mkdir(parents=True, exist_ok=True)

import pgserver  # noqa: E402

from model_registry.config import Settings  # noqa: E402
from model_registry.store import RegistryStore  # noqa: E402

REPO_ROOT = Path(__file__).resolve().parents[3]
MIGRATION = REPO_ROOT / "infra/postgres/init-scripts/30-model-registry.sql"
APP_ROLE = "app_model_registry_login"
BATCH_ROLE = "app_model_registry_batch"

TENANT_A = str(uuid.uuid4())
TENANT_B = str(uuid.uuid4())


@pytest.fixture(scope="session")
def pg(tmp_path_factory):
    server = pgserver.get_server(str(tmp_path_factory.mktemp("pgdata")))
    sql = "\\set ON_ERROR_STOP on\n" + MIGRATION.read_text()
    server.psql(sql)  # raises CalledProcessError on any migration error
    yield server
    server.cleanup()


@pytest.fixture(scope="session")
def super_dsn(pg) -> str:
    return pg.get_uri(database="platform")


@pytest.fixture(scope="session")
def app_dsn(pg, super_dsn) -> str:
    """DSN as the least-privilege app role (RLS subject), like production."""
    info = psycopg.conninfo.conninfo_to_dict(super_dsn)
    info["user"] = APP_ROLE
    return psycopg.conninfo.make_conninfo(**info)


@pytest.fixture(scope="session")
def internal_dsn(pg, super_dsn) -> str:
    """DSN as the internal batch role (member of app_model_registry_internal),
    like the production MODEL_REGISTRY_INTERNAL_DSN."""
    info = psycopg.conninfo.conninfo_to_dict(super_dsn)
    info["user"] = BATCH_ROLE
    return psycopg.conninfo.make_conninfo(**info)


@pytest.fixture(autouse=True)
def clean_tables(pg, super_dsn):
    with psycopg.connect(super_dsn, autocommit=True) as conn:
        conn.execute(
            "TRUNCATE model_version, experiments, experiment_outcomes,"
            " feature_observations, score_observations, model_family CASCADE")
    yield


@pytest.fixture()
def store(app_dsn, internal_dsn) -> RegistryStore:
    """Store wired like production: app role for tenant paths, batch role for
    internal cross-tenant transactions (SPEC-W34 GF1)."""
    return RegistryStore(app_dsn, internal_dsn=internal_dsn)


@pytest.fixture()
def settings(app_dsn, internal_dsn) -> Settings:
    return Settings(pg_dsn=app_dsn, pg_internal_dsn=internal_dsn,
                    kafka_enabled=False,
                    drift_manifest_dir=str(Path(__file__).parent / "fixtures"))


@pytest.fixture()
def client(settings, store):
    from fastapi.testclient import TestClient
    from model_registry.main import create_app
    app = create_app(settings=settings, store=store, enable_scheduler=False)
    with TestClient(app) as c:
        yield c
