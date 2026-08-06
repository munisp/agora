"""C3: report endpoint — per-arm precision/recall/Brier on a hand-computed
fixture over labeled outcomes; unlabeled rows excluded; unknown id → 404."""

from __future__ import annotations

import pytest

from conftest import TENANT_A


def _setup_experiment(client):
    client.post("/v1/registry/register", json={
        "family": "fraud-clf", "tenant_id": TENANT_A,
        "artifact_uri": "s3://x/v1", "version": 1})
    client.post("/v1/registry/register", json={
        "family": "fraud-clf", "tenant_id": TENANT_A,
        "artifact_uri": "s3://x/v2", "version": 2})
    r = client.post("/v1/registry/experiments", json={
        "family": "fraud-clf", "tenant_id": TENANT_A,
        "champion_version": 1, "challenger_version": 2, "pct": 50})
    return r.json()["id"]


def test_report_hand_computed(client):
    exp_id = _setup_experiment(client)
    # champion arm, labeled: (pred_label, score, true)
    #   (1, 0.9, 1) TP; (1, 0.8, 0) FP; (0, 0.1, 1) FN; (0, 0.2, 0) TN
    #   precision = 1/2, recall = 1/2, brier = (0.01 + 0.64 + 0.81 + 0.04)/4 = 0.375
    champion_rows = [(1, 0.9, 1), (1, 0.8, 0), (0, 0.1, 1), (0, 0.2, 0)]
    # challenger arm, labeled: (1, 0.7, 1), (0, 0.3, 0) + one UNLABELED row
    #   precision = 1/1, recall = 1/1, brier = (0.09 + 0.09)/2 = 0.09
    challenger_rows = [(1, 0.7, 1), (0, 0.3, 0)]
    for i, (pl, sc, tl) in enumerate(champion_rows):
        r = client.post(f"/v1/registry/experiments/{exp_id}/outcomes", json={
            "tenant_id": TENANT_A, "person_id": f"c-{i}", "assigned_arm": "champion",
            "predicted_label": pl, "predicted_score": sc, "true_label": tl})
        assert r.status_code == 201, r.text
    for i, (pl, sc, tl) in enumerate(challenger_rows):
        client.post(f"/v1/registry/experiments/{exp_id}/outcomes", json={
            "tenant_id": TENANT_A, "person_id": f"h-{i}", "assigned_arm": "challenger",
            "predicted_label": pl, "predicted_score": sc, "true_label": tl})
    # unlabeled outcome must be excluded from metrics but counted in total
    client.post(f"/v1/registry/experiments/{exp_id}/outcomes", json={
        "tenant_id": TENANT_A, "person_id": "h-x", "assigned_arm": "challenger",
        "predicted_label": 1, "predicted_score": 0.6, "true_label": None})

    r = client.get(f"/v1/registry/experiments/{exp_id}/report")
    assert r.status_code == 200, r.text
    arms = {a["arm"]: a for a in r.json()["arms"]}

    champ = arms["champion"]
    assert champ["labeled"] == 4
    assert champ["precision"] == pytest.approx(0.5)
    assert champ["recall"] == pytest.approx(0.5)
    assert champ["brier"] == pytest.approx(0.375)

    chall = arms["challenger"]
    assert chall["total"] == 3
    assert chall["labeled"] == 2
    assert chall["precision"] == pytest.approx(1.0)
    assert chall["recall"] == pytest.approx(1.0)
    assert chall["brier"] == pytest.approx(0.09)


def test_report_unknown_experiment_404(client):
    import uuid
    r = client.get(f"/v1/registry/experiments/{uuid.uuid4()}/report")
    assert r.status_code == 404
