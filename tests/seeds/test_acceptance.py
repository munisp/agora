"""§8.7 acceptance gates that run DB-free (SPEC-W17, Agent D).

Gates covered (mapping in docs/data-seeding.md §6):
  #1 idempotency  — double --dry-run equality for every seed script
  #2 completeness — generator cardinalities (774/8812/32/768 + scale math + FX ≥365)
  #3 collision    — collision_guard.py PASS on the regenerated seeded idspace,
                    and FAIL when a guard id is planted in the idspace
  #5 drift        — drift.sql parses (sqlparse when installed, structural
                    check otherwise — the test says which) and names all
                    contract tables/checks
plus orchestration contract checks: bootstrap.sh/snapshot.sh bash -n,
Makefile seed targets, dashboard JSON contract, datasource provisioning,
master-doc adaptation log, and the Go consent fast-path (runs when a Go
toolchain is on PATH or in a well-known location — environmental skip only).

Run:  pytest tests/seeds/test_acceptance.py        (repo root, seed venv)
Everything here is hermetic: no DB, no Kafka, no network, tmp-path outboxes.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
SEEDS = ROOT / "scripts" / "seeds"
sys.path.insert(0, str(SEEDS))  # also done by conftest.py; explicit for direct imports

SEED_SCRIPTS = [
    "seed_lgas.py",
    "seed_wards.py",
    "seed_channels.py",
    "seed_channel_costs.py",
    "seed_locale.py",
    "seed_agents.py",
    "seed_customers.py",
    "seed_events.py",
    "seed_fx.py",
]

# Tiny scale keeps the suite fast; entity seeds still exercise the scale math.
GATE_SCALE = "0.01"


def run_seed(script: str, scale: str = GATE_SCALE) -> str:
    env = dict(os.environ, SEED_SCALE=scale, SEED_KAFKA="off",
               SEED_REPORT_PATH="/tmp/acceptance_seed_reports.jsonl")
    out = subprocess.run(
        [sys.executable, str(SEEDS / script), "--dry-run", "--scale", scale],
        capture_output=True, text=True, env=env, timeout=300,
    )
    assert out.returncode == 0, f"{script} --dry-run failed: {out.stderr[-2000:]}"
    return out.stdout


def normalized(out: str) -> str:
    """Deterministic projection of a dry-run: drop free-text sample lines
    (Pidgin samples, sample envelopes) that are illustrative, not contract."""
    drop = re.compile(r"sample", re.IGNORECASE)
    return "\n".join(ln for ln in out.splitlines() if not drop.search(ln))


# ---------------------------------------------------------------------------
# Orchestration contract
# ---------------------------------------------------------------------------

def test_bootstrap_and_snapshot_shell_syntax():
    for script in ["bootstrap.sh", "snapshot.sh"]:
        r = subprocess.run(["bash", "-n", str(SEEDS / script)], capture_output=True, text=True)
        assert r.returncode == 0, f"bash -n {script}: {r.stderr}"


def test_makefile_seed_targets():
    text = (ROOT / "Makefile").read_text(encoding="utf-8")
    for target in ["seed-all:", "seed-ci:", "seed-drift:"]:
        assert target in text, f"Makefile missing target {target}"
    assert "SEED_SCALE=0.05" in text, "seed-ci must pin SEED_SCALE=0.05"
    assert "scripts/seeds/drift.sql" in text


def test_bootstrap_dry_run_step_sequence():
    """Whole 9-step orchestrator runs DB-free and hits every step in order."""
    env = dict(os.environ, SEED_SCALE=GATE_SCALE, SEED_KAFKA="off",
               SEED_PYTHON=sys.executable,  # seed venv (argon2-cffi) — not the system python
               SEED_REPORT_PATH="/tmp/acceptance_seed_reports.jsonl")
    r = subprocess.run(
        ["bash", str(SEEDS / "bootstrap.sh"), "--dry-run"],
        capture_output=True, text=True, env=env, timeout=600, cwd=ROOT,
    )
    assert r.returncode == 0, f"bootstrap --dry-run failed:\n{r.stdout[-1500:]}\n{r.stderr[-1500:]}"
    steps = re.findall(r"step (\d)/9: .*— START", r.stdout)
    assert steps == [str(i) for i in range(1, 10)], f"step sequence: {steps}"
    assert "ALL 9 SEED STEPS COMPLETE" in r.stdout


# ---------------------------------------------------------------------------
# §8.7 #1 idempotency — double --dry-run equality
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("script", SEED_SCRIPTS)
def test_double_dry_run_idempotent(script):
    first = normalized(run_seed(script))
    second = normalized(run_seed(script))
    assert first == second, f"{script}: two --dry-run runs differ (non-deterministic output)"


# ---------------------------------------------------------------------------
# §8.7 #2 completeness — generator cardinalities
# ---------------------------------------------------------------------------

def rows_of(out: str) -> int:
    m = re.search(r"rows=(\d+)", out)
    assert m, f"no rows= in dry-run output: {out[:300]}"
    return int(m.group(1))


def test_reference_cardinality():
    # Fixed reference data (scale-independent) at scale 1.0.
    assert rows_of(run_seed("seed_lgas.py", "1.0")) == 774
    assert rows_of(run_seed("seed_channels.py", "1.0")) == 32
    assert rows_of(run_seed("seed_channel_costs.py", "1.0")) == 32 * 24
    assert rows_of(run_seed("seed_wards.py", "1.0")) == 8812


def test_scaled_cardinality_math():
    scale = 0.02
    assert rows_of(run_seed("seed_agents.py", str(scale))) == int(5000 * scale)
    assert rows_of(run_seed("seed_customers.py", str(scale))) == int(200000 * scale)
    assert rows_of(run_seed("seed_wards.py", str(scale))) == int(8812 * scale)


def test_fx_contiguity_and_floor():
    out = run_seed("seed_fx.py", "1.0")
    n = rows_of(out)
    assert n >= 365, "§8.7: FX series must have >=365 contiguous points"
    m = re.search(r"rows=\d+ (\d{4}-\d{2}-\d{2})\.\.(\d{4}-\d{2}-\d{2})", out)
    assert m, f"FX dry-run must print the date span: {out[:300]}"
    from datetime import date

    d0, d1 = (date.fromisoformat(m.group(1)), date.fromisoformat(m.group(2)))
    assert (d1 - d0).days + 1 == n, f"FX series not contiguous: {n} rows over {(d1 - d0).days + 1} days"


def test_events_envelope_shape():
    """FunnelEvent dry-run sample must carry the W13 envelope fields."""
    out = run_seed("seed_events.py", GATE_SCALE)
    m = re.search(r"sample: (\{.*\})", out)
    assert m, f"seed_events dry-run prints no sample envelope: {out[:300]}"
    env = json.loads(m.group(1))
    assert env["type"] == "com.opendesk.cac.FunnelEvent"
    assert env["specversion"] == "1.0"
    data = env["data"]
    for key in ["event_id", "tenant_id", "entity_type", "entity_id", "event_name",
                "event_ts", "channel", "campaign_id", "lga_id", "amount_ngn",
                "idempotency_key"]:
        assert key in data, f"FunnelEvent data missing {key}"


# ---------------------------------------------------------------------------
# §8.7 #3 collision guard
# ---------------------------------------------------------------------------

def test_collision_guard_pass_db_free():
    r = subprocess.run([sys.executable, str(SEEDS / "collision_guard.py")],
                       capture_output=True, text=True, timeout=300)
    assert r.returncode == 0, f"collision guard FAILED: {r.stdout}\n{r.stderr}"
    summary = json.loads(r.stdout.strip().splitlines()[-1])
    assert summary["status"] == "PASS"
    assert summary["guard_bvns"] == 1000
    assert summary["collisions"] == 0
    assert summary["idspace_size"] > 0, "idspace must be regenerated DB-free, not skipped"
    assert summary["id_path"] == "scripts/seeds/_lib.py", "guard must use the contract-A _lib path"


def test_collision_guard_detects_planted_collision(tmp_path):
    import _lib  # contract A lib (conftest sys.path)

    import collision_guard

    victim = _lib.deterministic_id(collision_guard.guard_bvns(1)[0])
    idspace = tmp_path / "idspace.txt"
    idspace.write_text(victim + "\n", encoding="utf-8")
    r = subprocess.run(
        [sys.executable, str(SEEDS / "collision_guard.py"), "--idspace", str(idspace)],
        capture_output=True, text=True, timeout=120,
    )
    assert r.returncode == 1, "guard must fail when a guard id is planted in the idspace"
    assert json.loads(r.stdout.strip().splitlines()[-1])["collisions"] == 1


def test_seeded_idspace_matches_agent_b_contract():
    """Guard's regenerated idspace uses the exact Agent-B natural keys."""
    import _lib

    import collision_guard

    ids, source = collision_guard.seeded_idspace(_lib.deterministic_id, lambda n: n)
    assert _lib.deterministic_id("customer:00000000") in ids
    assert _lib.deterministic_id("agent:000000") in ids
    assert len(ids) == 205000, f"{source}: {len(ids)}"


# ---------------------------------------------------------------------------
# §8.7 #5 drift gate artifacts
# ---------------------------------------------------------------------------

def test_drift_sql_parses():
    sql = (SEEDS / "drift.sql").read_text(encoding="utf-8")
    try:
        import sqlparse  # type: ignore

        statements = [s for s in sqlparse.parse(sql) if str(s).strip()]
        assert len(statements) == 1, f"sqlparse: {len(statements)} statements"
        assert str(statements[0]).rstrip().endswith(";")
        parser_used = "sqlparse"
    except ImportError:
        # Structural fallback (no sqlparse installed): balanced parens, no
        # unterminated single-quoted strings, exactly one terminal semicolon
        # outside psql meta-commands.
        body = "\n".join(ln for ln in sql.splitlines() if not ln.lstrip().startswith("\\"))
        assert body.count("(") == body.count(")"), "unbalanced parens"
        assert body.count("'") % 2 == 0, "unterminated quoted string"
        assert body.rstrip().endswith(";"), "must end a single SELECT with ;"
        parser_used = "structural(self-review)"
    print(f"drift.sql parse check via {parser_used}")


def test_drift_sql_covers_contract():
    sql = (SEEDS / "drift.sql").read_text(encoding="utf-8")
    for table in ["lgas", "wards", "channels", "channel_unit_costs",
                  "agents", "customers", "fx_series", "seed_run_log"]:
        assert f"cac.{table}" in sql or f"'{table}'" in sql, f"drift.sql ignores {table}"
    for check in ["missing_table", "missing_seed_run_log_row", "rowcount_mismatch",
                  "cardinality_drift", "empty_table", "non_synthetic_rows", "fx_series_gap"]:
        assert check in sql, f"drift.sql missing check {check}"


# ---------------------------------------------------------------------------
# Dashboard + datasource + docs contract
# ---------------------------------------------------------------------------

def test_seed_report_dashboard():
    dash = json.loads((ROOT / "infra/observability/dashboards/seed-report.json").read_text(encoding="utf-8"))
    titles = [p["title"] for p in dash["panels"]]
    assert "seed_report_summary" in titles, f"panels: {titles}"
    summary = next(p for p in dash["panels"] if p["title"] == "seed_report_summary")
    assert "cac.seed_run_log" in summary["targets"][0]["rawSql"]
    for p in dash["panels"]:
        assert p["datasource"]["uid"] == "opendesk-postgres", p["title"]


def test_postgres_datasource_provisioned():
    yml = (ROOT / "infra/observability/grafana/provisioning/datasources/datasources.yml").read_text(encoding="utf-8")
    assert "uid: opendesk-postgres" in yml and "type: postgres" in yml
    assert "analytics_meta" in yml


def test_master_doc_adaptations():
    doc = (ROOT / "docs/data-seeding.md").read_text(encoding="utf-8")
    for token in ["Iceberg", "n8n", "mimesis", "Langflow", "Wagtail", "Prism",
                  "§8.6", "§8.7", "§8.8", "§8.9", "account_type=90",
                  "opendesk-dev-seed-salt-change-in-prod",
                  "preferred_language",  # accepted deviation (B flag #2)
                  "infra/observability/dashboards"]:  # dashboard path adaptation
        assert token in doc, f"docs/data-seeding.md missing '{token}'"


# ---------------------------------------------------------------------------
# Go consent erasure fast-path (environmental: needs a Go toolchain)
# ---------------------------------------------------------------------------

def _go_binary() -> str | None:
    for cand in [shutil.which("go"),
                 "/usr/local/go/bin/go",
                 os.path.expanduser("~/tools/go/bin/go")]:
        if cand and Path(cand).exists():
            return cand
    return None


def test_go_consent_erasure_fastpath():
    go = _go_binary()
    if go is None:
        pytest.skip("no Go toolchain available (environmental; Go gates run in service CI)")
    env = dict(os.environ,
               GOPROXY=os.environ.get("GOPROXY", "https://goproxy.cn,direct"),
               GOFLAGS=os.environ.get("GOFLAGS", "-mod=mod"))
    r = subprocess.run(
        [go, "test", "./internal/consent/", "-run", "Synthetic|Eligibility", "-count=1", "-v"],
        capture_output=True, text=True, cwd=ROOT / "services/identity-service",
        env=env, timeout=600,
    )
    assert r.returncode == 0, f"go test consent fast-path failed:\n{r.stdout[-2000:]}\n{r.stderr[-2000:]}"
    for name in ["TestIsSyntheticSubject", "TestEvaluateErasureEligibility",
                 "TestErasureSyntheticFastPath", "TestErasureRealSubjectNotSynthetic"]:
        assert f"--- PASS: {name}" in r.stdout, name
