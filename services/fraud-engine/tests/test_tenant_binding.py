"""SPEC-W30 §5 gate 1: every emitted Cypher binds $tenant_id (the one
exempt statement is the sweep's Tenant-node discovery, which has no tenant
parameter by nature). Plus behavioral tenant isolation."""

import pytest

from fraud_engine.detectors import ALL_DETECTORS, DetectionRunner
from fraud_engine.detectors.base import TENANTS_CYPHER, assert_tenant_bound

from conftest import NOW, OTHER_TENANT, TENANT, make_cycle


def test_assert_tenant_bound_rejects_unbound():
    with pytest.raises(ValueError):
        assert_tenant_bound("MATCH (p:Person) RETURN p", {})
    with pytest.raises(ValueError):
        assert_tenant_bound("MATCH (p:Person {tenant_id:$tenant_id}) RETURN p", {})
    assert_tenant_bound(
        "MATCH (p:Person {tenant_id:$tenant_id}) RETURN p", {"tenant_id": "t"}
    )  # no raise


def test_every_detector_cypher_references_tenant_param(settings):
    for det in ALL_DETECTORS:
        cy = det.cypher(settings)
        assert "$tenant_id" in cy, det.name
        params = det.params(TENANT, settings, NOW)
        assert params["tenant_id"] == TENANT, det.name


def test_full_run_binds_tenant_on_every_statement(client, graph, publisher, settings):
    graph.add_tenant(TENANT)
    make_cycle(graph, TENANT, ["p1", "p2", "p3"])
    DetectionRunner(client, publisher, settings).run(now=NOW)
    assert client.calls, "expected Cypher statements to have been emitted"
    for cypher, params in client.calls:
        if cypher.startswith(TENANTS_CYPHER.splitlines()[0]):
            continue  # sweep tenant discovery: no $tenant_id by nature
        assert params.get("tenant_id") == TENANT, cypher[:80]
        assert "$tenant_id" in cypher, cypher[:80]


def test_tenant_isolation(client, graph, publisher, settings):
    # The ring exists only in OTHER_TENANT; running for TENANT sees nothing.
    make_cycle(graph, OTHER_TENANT, ["x1", "x2", "x3"])
    runner = DetectionRunner(client, publisher, settings)
    report = runner.run(tenant_id=TENANT, now=NOW)
    assert report.findings == 0 and report.alerts_created == 0
    assert graph.alerts == {}
    # ...and the other tenant's run produces alerts stamped with ITS tenant
    report2 = runner.run(tenant_id=OTHER_TENANT, now=NOW)
    assert report2.alerts_created == 3
    assert all(a["tenant_id"] == OTHER_TENANT for a in graph.alerts.values())
