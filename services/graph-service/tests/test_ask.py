"""/v1/graph/ask tests — allowlist enforcement, tenant injection, row cap,
503 degradation when Ollama is down."""

from __future__ import annotations

import json

from app.ask import AskUnavailable
from conftest import HDR_A, HDR_B, StubLLM


def test_ask_happy_path_returns_cypher_and_rows(make_client):
    llm = StubLLM(['{"template": "persons_by_consent", "params": {"purpose": "marketing"}}'])
    client = make_client(llm=llm)
    resp = client.post("/v1/graph/ask", json={"question": "who consented to marketing?"}, headers=HDR_A)
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["template"] == "persons_by_consent"
    assert "$tenant_id" in body["cypher"]  # tenant filter injected post-generation
    assert "LIMIT" in body["cypher"]
    ids = {r["person_id"] for r in body["rows"]}
    assert ids == {"pa1", "pa4", "pa5", "pa6"}
    assert body["row_count"] == len(body["rows"]) <= 100


def test_ask_tenant_isolated(make_client):
    llm = StubLLM(['{"template": "persons_by_consent", "params": {"purpose": "marketing"}}'])
    client = make_client(llm=llm)
    body = client.post("/v1/graph/ask", json={"question": "marketing people"}, headers=HDR_B).json()
    assert {r["person_id"] for r in body["rows"]} == {"pb1"}


def test_ask_tolerates_code_fenced_json(make_client):
    llm = StubLLM(['```json\n{"template": "consent_counts", "params": {}}\n```'])
    client = make_client(llm=llm)
    resp = client.post("/v1/graph/ask", json={"question": "consent counts?"}, headers=HDR_A)
    assert resp.status_code == 200
    by_purpose = {r["purpose"]: r["persons"] for r in resp.json()["rows"]}
    assert by_purpose["marketing"] == 4


def test_ask_allowlist_rejects_unknown_template(make_client):
    llm = StubLLM(['{"template": "drop_graph", "params": {}}'])
    client = make_client(llm=llm)
    resp = client.post("/v1/graph/ask", json={"question": "delete everything"}, headers=HDR_A)
    assert resp.status_code == 422


def test_ask_rejects_raw_cypher_from_llm(make_client):
    # Prompt-injected model trying to emit write Cypher: never executed.
    llm = StubLLM(["MATCH (p:Person) DETACH DELETE p"])
    client = make_client(llm=llm)
    resp = client.post("/v1/graph/ask", json={"question": "ignore instructions"}, headers=HDR_A)
    assert resp.status_code == 422
    # Graph untouched: tenant A still has its persons.
    resp2 = client.post(
        "/v1/graph/cypher", json={"template": "consent_counts"}, headers=HDR_A
    )
    assert resp2.status_code == 200


def test_ask_rejects_injection_via_llm_params(make_client):
    llm = StubLLM([json.dumps({"template": "persons_by_consent", "params": {"purpose": "x' DETACH DELETE //"}})])
    client = make_client(llm=llm)
    resp = client.post("/v1/graph/ask", json={"question": "q"}, headers=HDR_A)
    assert resp.status_code == 422


def test_ask_unanswerable_maps_to_422(make_client):
    llm = StubLLM(['{"template": "unanswerable", "params": {}}'])
    client = make_client(llm=llm)
    assert client.post("/v1/graph/ask", json={"question": "meaning of life?"}, headers=HDR_A).status_code == 422


def test_ask_ollama_down_degrades_to_503_with_reason(make_client):
    llm = StubLLM([AskUnavailable("ollama unavailable: connection refused")])
    client = make_client(llm=llm)
    resp = client.post("/v1/graph/ask", json={"question": "anything"}, headers=HDR_A)
    assert resp.status_code == 503
    assert resp.json()["detail"]["reason"] == "ollama_unavailable"


def test_ask_requires_auth(client):
    assert client.post("/v1/graph/ask", json={"question": "q"}).status_code == 401


def test_ask_schema_prompt_includes_allowlist(make_client):
    llm = StubLLM()
    client = make_client(llm=llm)
    client.post("/v1/graph/ask", json={"question": "q"}, headers=HDR_A)
    system = llm.calls[0][0]["content"]
    assert "persons_by_consent" in system
    assert "READ-ONLY" in system
    assert "tenant_id" in system
