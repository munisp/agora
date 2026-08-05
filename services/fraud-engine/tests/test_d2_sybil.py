"""D2 sybil_cluster: near-identical name embeddings + same agent + same
location + burst timing."""

from fraud_engine.detectors.d2_sybil import SybilClusterDetector, cosine_similarity

from conftest import NOW, TENANT, add_capture, ts

BASE = [1.0, 0.0, 0.0]
NEAR = [0.9999, 0.01, 0.0]  # cosine with BASE ~ 0.9999 >= 0.98
FAR = [0.0, 1.0, 0.0]  # orthogonal -> cosine 0


def test_cosine_similarity_sanity():
    assert cosine_similarity(BASE, BASE) == 1.0
    assert cosine_similarity(BASE, FAR) == 0.0
    assert cosine_similarity(BASE, NEAR) > 0.98
    assert cosine_similarity([], BASE) == 0.0


def _sybil_burst(graph, n, agent="agent-1", embedding=NEAR):
    for i in range(n):
        add_capture(
            graph, TENANT, f"person-{i}", f"lead-{i}", agent, ts(10 + i),
            embedding=embedding, lga="Ikeja",
        )


def test_fires_medium_on_similar_name_burst(client, graph, settings):
    _sybil_burst(graph, 3)
    findings = SybilClusterDetector().detect(client, TENANT, settings, NOW)
    assert {f.person_id for f in findings} == {"person-0", "person-1", "person-2"}
    assert all(f.severity == "medium" for f in findings)
    assert all(f.agent_id == "agent-1" for f in findings)
    assert findings[0].evidence["cluster_size"] == 3


def test_high_when_cluster_has_five(client, graph, settings):
    _sybil_burst(graph, 5)
    findings = SybilClusterDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 5
    assert all(f.severity == "high" for f in findings)


def test_silent_when_embeddings_dissimilar(client, graph, settings):
    # pairwise-orthogonal name embeddings: same agent/location/timing is not enough
    for i, emb in enumerate(([1.0, 0.0, 0.0], [0.0, 1.0, 0.0], [0.0, 0.0, 1.0])):
        add_capture(graph, TENANT, f"person-{i}", f"lead-{i}", "agent-1", ts(10 + i),
                    embedding=emb, lga="Ikeja")
    assert SybilClusterDetector().detect(client, TENANT, settings, NOW) == []


def test_silent_when_different_locations(client, graph, settings):
    for i, lga in enumerate(["Ikeja", "Surulere", "Yaba"]):
        add_capture(graph, TENANT, f"person-{i}", f"lead-{i}", "agent-1", ts(10 + i),
                    embedding=NEAR, lga=lga)
    assert SybilClusterDetector().detect(client, TENANT, settings, NOW) == []


def test_silent_when_outside_window(client, graph, settings):
    # same agent+location+embedding but captures 3 hours apart (> 60 min window)
    for i in range(3):
        add_capture(graph, TENANT, f"person-{i}", f"lead-{i}", "agent-1", ts(10 + i * 180),
                    embedding=NEAR, lga="Ikeja")
    assert SybilClusterDetector().detect(client, TENANT, settings, NOW) == []
