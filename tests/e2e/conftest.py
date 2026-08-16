"""E2E fixtures: docker-compose stack lifecycle, health waits, tenant seed.

These tests run ONLY against a live docker host with the full OpenDesk
compose stack (see README.md). Everything below is real: no mocks, no fakes.
Set E2E_COMPOSE_UP=1 to have the fixture bring the stack up itself.

The stack fixture exports AUTHZ_DISABLED=true and E2E_FIXTURES=1 (in the
environment of the `docker compose` processes it spawns) for the stack it
manages — an e2e-only posture; neither is a code default.
"""
from __future__ import annotations

import json
import os
import secrets
import shutil
import subprocess
import time
from pathlib import Path

import pytest
import requests

REPO_ROOT = Path(__file__).resolve().parents[2]

# Base URLs (direct service ports; the gateway is exercised separately).
GW = os.environ.get("E2E_GW", "http://localhost:9080")
IDENTITY = os.environ.get("E2E_IDENTITY", "http://localhost:7001")
BOOKING = os.environ.get("E2E_BOOKING", "http://localhost:7002")
CRM_SYNC = os.environ.get("E2E_CRM_SYNC", "http://localhost:7010")
OPENSEARCH = os.environ.get("E2E_OPENSEARCH", "http://localhost:9200")
TRINO = os.environ.get("E2E_TRINO", "http://localhost:8088")

HEALTH_ENDPOINTS = {
    "identity": os.environ.get("E2E_IDENTITY", "http://localhost:7001"),
    "booking": os.environ.get("E2E_BOOKING", "http://localhost:7002"),
    "notification": os.environ.get("E2E_NOTIFICATION", "http://localhost:7003"),
    "payments": os.environ.get("E2E_PAYMENTS", "http://localhost:7004"),
    "edge": os.environ.get("E2E_EDGE", "http://localhost:7005"),
    "voice": os.environ.get("E2E_VOICE", "http://localhost:7006"),
    "conversation": os.environ.get("E2E_CONVERSATION", "http://localhost:7007"),
    "knowledge": os.environ.get("E2E_KNOWLEDGE", "http://localhost:7008"),
    "analytics": os.environ.get("E2E_ANALYTICS", "http://localhost:7009"),
}

COMPOSE_UP = os.environ.get("E2E_COMPOSE_UP") == "1"
COMPOSE_DOWN = os.environ.get("E2E_COMPOSE_DOWN") == "1"
HEALTH_TIMEOUT_S = int(os.environ.get("E2E_HEALTH_TIMEOUT", "600"))


def run(
    cmd: list[str],
    timeout: int = 60,
    check: bool = True,
    cwd: Path | None = None,
    env: dict | None = None,
) -> subprocess.CompletedProcess:
    return subprocess.run(
        cmd, capture_output=True, text=True, timeout=timeout, check=check,
        cwd=cwd or REPO_ROOT, env=env,
    )


def docker_available() -> bool:
    if shutil.which("docker") is None:
        return False
    try:
        run(["docker", "compose", "version"], timeout=15)
        run(["docker", "info"], timeout=15)
        return True
    except Exception:
        return False


def wait_http_ok(url: str, timeout_s: float, interval: float = 3.0) -> bool:
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            r = requests.get(url, timeout=5)
            if r.status_code == 200:
                return True
        except requests.RequestException:
            pass
        time.sleep(interval)
    return False


@pytest.fixture(scope="session")
def stack():
    """Session fixture: (optionally) bring the compose stack up, then wait
    for every application service to report healthy.

    Skips the whole suite when there is no docker host — this is the guard
    that lets CI collect the suite on non-docker runners without failing.
    """
    if not docker_available():
        pytest.skip("no docker host available (e2e requires docker + compose)")

    # E2E-only posture: the stack this fixture manages gets an EXPLICIT
    # opt-in to the dev auth seam (X-Tenant-Slug / X-Tenant-Id pass-through)
    # and the graph internal-fixtures seed route. Post-W39 SIM-028,
    # infra/docker-compose.graph.yml (lines ~211-237) defaults these
    # fail-closed (${AUTHZ_DISABLED:-false} / ${E2E_FIXTURES:-0}), so an
    # unset env no longer silently disables authz or enables fixtures —
    # e2e must opt in explicitly here. NEVER mirrored into production env.
    e2e_stack_env = {**os.environ, "AUTHZ_DISABLED": "true", "E2E_FIXTURES": "1"}
    if COMPOSE_UP:
        run(["docker", "compose", "up", "-d", "--build"], timeout=3600, env=e2e_stack_env)
    if COMPOSE_DOWN:
        # Schedule teardown regardless of test outcome.
        def _down():
            run(["docker", "compose", "down"], timeout=600, check=False, env=e2e_stack_env)
        import atexit
        atexit.register(_down)

    unhealthy = []
    deadline = time.time() + HEALTH_TIMEOUT_S
    for name, base in HEALTH_ENDPOINTS.items():
        remaining = max(1.0, deadline - time.time())
        if not wait_http_ok(f"{base}/healthz", remaining):
            unhealthy.append(name)
    if unhealthy:
        pytest.fail(f"services never became healthy: {unhealthy}")
    return {"gw": GW}


@pytest.fixture(scope="session")
def tenant(stack):
    """Provision a fresh tenant and seed a bookable catalog end-to-end:
    identity POST /v1/tenants (kicks the TenantOnboardingWorkflow which seeds
    the default public site), then booking catalog (offering, team member,
    weekly availability rules) through the tenant-scoped API.

    booking-service runs with AUTHZ_DISABLED=true in dev compose, so the
    management API is reachable with just the X-Tenant-Slug header — the same
    pattern scripts/seed-demo.sh uses.
    """
    slug = f"e2e-{secrets.token_hex(3)}"
    r = requests.post(
        f"{IDENTITY}/v1/tenants",
        json={
            "slug": slug,
            "name": f"E2E Salon {slug}",
            "timezone": "UTC",
            "currency": "GBP",
            "locale": "en-GB",
            "plan": "pro",
        },
        timeout=15,
    )
    assert r.status_code in (200, 201), f"tenant create failed: {r.status_code} {r.text}"
    tenant_body = r.json()

    headers = {"X-Tenant-Slug": slug, "content-type": "application/json"}

    # Offering.
    r = requests.post(
        f"{BOOKING}/v1/offerings",
        headers=headers,
        json={"name": "E2E Haircut", "duration_min": 30, "buffer_min": 0,
              "price_cents": 2500, "currency": "GBP", "capacity": 1, "bookable": True},
        timeout=15,
    )
    assert r.status_code in (200, 201), f"offering create failed: {r.status_code} {r.text}"
    offering = r.json()

    # Team member.
    r = requests.post(
        f"{BOOKING}/v1/team-members",
        headers=headers,
        json={"name": "E2E Stylist", "email": f"{slug}@example.com", "role": "staff"},
        timeout=15,
    )
    assert r.status_code in (200, 201), f"team member create failed: {r.status_code} {r.text}"
    member = r.json()

    # Availability: 09:00-18:00 every weekday so any near-future day has slots.
    rules = [{"weekday": wd, "start_min": 9 * 60, "end_min": 18 * 60} for wd in range(7)]
    r = requests.put(
        f"{BOOKING}/v1/team-members/{member['id']}/availability",
        headers=headers,
        json={"rules": rules},
        timeout=15,
    )
    assert r.status_code in (200, 204), f"availability put failed: {r.status_code} {r.text}"

    # The onboarding workflow seeds the public site asynchronously; poll the
    # public context through the gateway until the site resolves.
    ctx_url = f"{GW}/api/bookings/public/sites/{slug}/context"
    assert wait_http_ok(ctx_url, 180), "public site never appeared (onboarding workflow?)"
    context = requests.get(ctx_url, timeout=15).json()

    return {
        "slug": slug,
        "tenant": tenant_body,
        "offering": offering,
        "member": member,
        "context": context,
        "headers": headers,
    }


@pytest.fixture(scope="session")
def flow():
    """Mutable session state shared across the ordered test steps."""
    return {}


def kafka_read(topic: str, timeout_ms: int = 15000) -> str:
    """Read a Kafka topic from the beginning via the broker container."""
    cp = run(
        [
            "docker", "exec", "kafka",
            "/opt/bitnami/kafka/bin/kafka-console-consumer.sh",
            "--bootstrap-server", "localhost:9092",
            "--topic", topic,
            "--from-beginning",
            "--timeout-ms", str(timeout_ms),
        ],
        timeout=timeout_ms / 1000 + 30,
        check=False,
    )
    return cp.stdout


def psql(db: str, sql: str) -> str:
    cp = run(
        ["docker", "exec", "postgres", "psql", "-U", "opendesk", "-d", db, "-tAc", sql],
        timeout=30,
    )
    return cp.stdout.strip()


def trino_query(sql: str, timeout_s: float = 30) -> list[list]:
    """Run a query through the Trino HTTP API and return data rows."""
    r = requests.post(
        f"{TRINO}/v1/statement",
        data=sql,
        headers={"X-Trino-User": "e2e"},
        timeout=30,
    )
    r.raise_for_status()
    payload = r.json()
    rows: list[list] = []
    deadline = time.time() + timeout_s
    while True:
        if "data" in payload:
            rows.extend(payload["data"])
        next_uri = payload.get("nextUri")
        if not next_uri:
            return rows
        if time.time() > deadline:
            raise TimeoutError(f"trino query did not finish: {sql}")
        time.sleep(0.5)
        payload = requests.get(next_uri, headers={"X-Trino-User": "e2e"}, timeout=30).json()


def poll(predicate, timeout_s: float, interval: float = 5.0, desc: str = "condition"):
    """Poll predicate() until truthy; returns the value or None on timeout."""
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            value = predicate()
        except Exception:
            value = None
        if value:
            return value
        time.sleep(interval)
    return None


def json_lines(text: str):
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            yield json.loads(line)
        except json.JSONDecodeError:
            continue
