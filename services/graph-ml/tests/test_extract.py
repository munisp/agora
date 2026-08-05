"""Extraction-layer tests: parameterized Cypher (never string-built),
preamble escaping, and the in-memory Cypher-mock client."""

from __future__ import annotations

import pytest

from graph_ml.extract import (
    ALL_TENANT_QUERIES,
    TENANTS_QUERY,
    StaticGraphClient,
    TenantGraph,
    build_query,
    quote_param,
)


def test_all_tenant_queries_bind_tenant_id():
    """SPEC §3: all Cypher parameterized with bound $tenant_id."""
    for query in ALL_TENANT_QUERIES:
        assert "$tenant_id" in query
        # static templates: no format placeholders that could be interpolated
        assert "{" not in query.replace("{tenant_id: $tenant_id}", "")
        assert "%" not in query
    assert TENANTS_QUERY == "MATCH (t:Tenant) RETURN t.tenant_id AS tenant_id"


def test_build_query_uses_preamble_not_interpolation():
    q = build_query("MATCH (p:Person {tenant_id: $tenant_id}) RETURN p", {"tenant_id": "t1"})
    assert q.startswith("CYPHER tenant_id='t1' ")
    assert "$tenant_id" in q  # statement keeps the bound parameter
    assert "'t1' RETURN" not in q


def test_quote_param_escapes_injection():
    evil = "t1' DETACH DELETE p //"
    literal = quote_param(evil)
    assert literal == "'t1\\' DETACH DELETE p //'"
    # the escaped quote is inside the literal; the value cannot break out
    q = build_query("MATCH (p:Person {tenant_id: $tenant_id}) RETURN p", {"tenant_id": evil})
    assert q.count("\\'") == 1


def test_quote_param_types():
    assert quote_param(None) == "null"
    assert quote_param(True) == "true"
    assert quote_param(5) == "5"
    assert quote_param(2.5) == "2.5"
    with pytest.raises(TypeError):
        quote_param([1, 2])


def test_static_graph_client_tenant_scoping(tenant_graph):
    client = StaticGraphClient({"t1": tenant_graph})
    assert client.list_tenants() == ["t1"]
    fetched = client.fetch_tenant_graph("t1")
    assert fetched.tenant_id == "t1"
    assert len(fetched.persons) == 5
    with pytest.raises(KeyError):
        client.fetch_tenant_graph("t2")  # other tenant invisible
    with pytest.raises(ValueError):
        client.fetch_tenant_graph("")


def test_parse_scalar_node_and_primitives():
    from graph_ml.extract import _parse_scalar

    assert _parse_scalar([1, None]) is None
    assert _parse_scalar([2, b"hello"]) == "hello"
    assert _parse_scalar([3, b"7"]) == 7
    assert _parse_scalar([4, 1]) is True
    assert _parse_scalar([5, b"0.5"]) == 0.5
    assert _parse_scalar([6, [[3, b"1"], [3, b"2"]]]) == [1, 2]
    node = _parse_scalar(
        [8, [9, [b"Person"], [[b"person_id", [2, b"p1"]], [b"quarantine", [4, 0]]]]]
    )
    assert node["_labels"] == ["Person"]
    assert node["person_id"] == "p1"
    assert node["quarantine"] is False
