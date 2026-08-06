"""SPEC-W33 §2 A2: internal graph export endpoints (graph_export.py producer).

Covers: X-Internal-Token auth (missing/wrong -> 401; JWTs never accepted on
internal routes), mandatory tenant scoping (no cross-tenant rows, missing
tenant param -> 422), JSONL (ND-JSON) response shape matching the Spark
graph_export.py node/edge input contracts, snapshot_date stamping
(default/pinned/invalid), and PII absence (Person ids only as the W28
sha256(salt|tenant|id) hash; no names/phones anywhere in the stream).

Run:
    python -m pytest tests/test_internal_export.py
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json

import pytest
from fastapi.testclient import TestClient

from app.backend import InMemoryBackend
from app.config import Settings
from app.export import w28_hash
from app.main import create_app
from app.store import SegmentStore
from conftest import StubLLM, build_graph

TOKEN = "internal-test-token"
SALT = "export-test-salt"

NODE_KEYS = [
    "snapshot_date", "tenant_id", "label", "node_id", "in_degree", "out_degree",
    "consent_marketing", "quarantine", "bookings_total", "ltv_cents",
    "no_show_rate", "propensity_show", "propensity_convert",
    "channel_of_first_touch", "last_active_at",
]
EDGE_KEYS = [
    "snapshot_date", "tenant_id", "edge_type", "src_label", "src_id",
    "dst_label", "dst_id", "weight", "edge_at",
]

# Raw identifiers / PII that must NEVER appear in an export stream.
FORBIDDEN_TOKENS = ["pa1", "pa2", "pa3", "pa4", "pa5", "pa6", "pa7", "pb1",
                    "Ada", "Bola", "Chidi", "Dara", "Efe", "Femi", "Goke",
                    "hash-pa1", "hash-pb1", "phone", "name"]


def _hs256_token(claims: dict, secret: str) -> str:
    def enc(obj) -> str:
        raw = json.dumps(obj, separators=(",", ":")).encode()
        return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()

    header = enc({"alg": "HS256", "typ": "JWT"})
    payload = enc(claims)
    signing = f"{header}.{payload}"
    sig = hmac.new(secret.encode(), signing.encode(), hashlib.sha256).digest()
    return f"{signing}.{base64.urlsafe_b64encode(sig).rstrip(b'=').decode()}"


@pytest.fixture()
def export_client(tmp_path):
    settings = Settings(
        graph_backend="memory",
        segment_store_dir=str(tmp_path / "store"),
        jwt_public_key="",
        internal_token=TOKEN,
        phone_hash_salt=SALT,
    )
    backend = InMemoryBackend(build_graph())
    app = create_app(
        settings,
        backend=backend,
        llm=StubLLM(),
        store=SegmentStore(str(tmp_path / "seg")),
    )
    return TestClient(app)


def _get_jsonl(client, path, **kwargs):
    resp = client.get(path, **kwargs)
    assert resp.status_code == 200, resp.text
    assert resp.headers["content-type"].startswith("application/x-ndjson")
    lines = [ln for ln in resp.text.splitlines() if ln.strip()]
    rows = [json.loads(ln) for ln in lines]
    assert int(resp.headers["x-row-count"]) == len(rows)
    return resp, rows


# ---------------------------------------------------------------- auth
@pytest.mark.parametrize("path", [
    "/v1/graph/internal/export/nodes?tenant_id=tenant-a",
    "/v1/graph/internal/export/edges?tenant_id=tenant-a",
])
def test_missing_token_401(export_client, path):
    assert export_client.get(path).status_code == 401


@pytest.mark.parametrize("path", [
    "/v1/graph/internal/export/nodes?tenant_id=tenant-a",
    "/v1/graph/internal/export/edges?tenant_id=tenant-a",
])
def test_wrong_token_401(export_client, path):
    resp = export_client.get(path, headers={"X-Internal-Token": "nope"})
    assert resp.status_code == 401


def test_jwt_never_accepted_on_export_routes(export_client):
    """A valid JWT must not authenticate an internal route (SPEC-W29 §3)."""
    jwt = _hs256_token({"sub": "tenant-a"}, "secret")
    resp = export_client.get(
        "/v1/graph/internal/export/nodes?tenant_id=tenant-a",
        headers={"Authorization": f"Bearer {jwt}"},
    )
    assert resp.status_code == 401


def test_unconfigured_internal_token_fails_closed(tmp_path):
    settings = Settings(
        graph_backend="memory",
        segment_store_dir=str(tmp_path / "store"),
        jwt_public_key="",
        internal_token="",  # fail-closed
        phone_hash_salt=SALT,
    )
    app = create_app(
        settings,
        backend=InMemoryBackend(build_graph()),
        llm=StubLLM(),
        store=SegmentStore(str(tmp_path / "seg")),
    )
    client = TestClient(app)
    resp = client.get(
        "/v1/graph/internal/export/nodes?tenant_id=tenant-a",
        headers={"X-Internal-Token": "anything"},
    )
    assert resp.status_code == 401


# ------------------------------------------------------- tenant scoping
def test_missing_tenant_param_422(export_client):
    resp = export_client.get(
        "/v1/graph/internal/export/nodes",
        headers={"X-Internal-Token": TOKEN},
    )
    assert resp.status_code == 422


def test_export_is_tenant_scoped(export_client):
    hdr = {"X-Internal-Token": TOKEN}
    _, nodes_a = _get_jsonl(
        export_client, "/v1/graph/internal/export/nodes?tenant_id=tenant-a",
        headers=hdr,
    )
    assert nodes_a, "tenant-a graph fixture is non-empty"
    assert {r["tenant_id"] for r in nodes_a} == {"tenant-a"}
    _, nodes_b = _get_jsonl(
        export_client, "/v1/graph/internal/export/nodes?tenant_id=tenant-b",
        headers=hdr,
    )
    assert {r["tenant_id"] for r in nodes_b} == {"tenant-b"}
    # tenant-a persons are hashed under tenant-a; the same hash must NOT
    # appear in tenant-b's stream and vice versa
    pa1_hash = w28_hash(SALT, "tenant-a", "pa1")
    assert pa1_hash in {r["node_id"] for r in nodes_a}
    assert pa1_hash not in {r["node_id"] for r in nodes_b}
    pb1_hash = w28_hash(SALT, "tenant-b", "pb1")
    assert pb1_hash in {r["node_id"] for r in nodes_b}
    # tenant hash-domain separation: hashing pa1 under tenant-b differs
    assert w28_hash(SALT, "tenant-b", "pa1") != pa1_hash
    _, edges_a = _get_jsonl(
        export_client, "/v1/graph/internal/export/edges?tenant_id=tenant-a",
        headers=hdr,
    )
    assert {r["tenant_id"] for r in edges_a} == {"tenant-a"}


# ------------------------------------------------------------- JSONL shape
def test_nodes_contract_shape_and_snapshot_date(export_client):
    hdr = {"X-Internal-Token": TOKEN}
    resp, rows = _get_jsonl(
        export_client,
        "/v1/graph/internal/export/nodes?tenant_id=tenant-a&snapshot_date=2026-08-06",
        headers=hdr,
    )
    assert resp.headers["x-snapshot-date"] == "2026-08-06"
    for row in rows:
        assert sorted(row.keys()) == sorted(NODE_KEYS)
        assert row["snapshot_date"] == "2026-08-06"
        assert row["label"] in {"Person", "Contact", "Offering", "Campaign"}
        assert isinstance(row["in_degree"], int)
        assert isinstance(row["out_degree"], int)
        assert isinstance(row["node_id"], str) and row["node_id"]


def test_edges_contract_shape(export_client):
    hdr = {"X-Internal-Token": TOKEN}
    _, rows = _get_jsonl(
        export_client,
        "/v1/graph/internal/export/edges?tenant_id=tenant-a&snapshot_date=2026-08-06",
        headers=hdr,
    )
    assert rows, "tenant-a fixture has edges"
    for row in rows:
        assert sorted(row.keys()) == sorted(EDGE_KEYS)
        assert row["snapshot_date"] == "2026-08-06"
        assert row["src_id"] and row["dst_id"] and row["edge_type"]


def test_invalid_snapshot_date_422(export_client):
    resp = export_client.get(
        "/v1/graph/internal/export/nodes?tenant_id=tenant-a&snapshot_date=06-08-2026",
        headers={"X-Internal-Token": TOKEN},
    )
    assert resp.status_code == 422


def test_default_snapshot_date_is_today_utc(export_client):
    from datetime import datetime, timezone

    _, rows = _get_jsonl(
        export_client,
        "/v1/graph/internal/export/nodes?tenant_id=tenant-a",
        headers={"X-Internal-Token": TOKEN},
    )
    today = datetime.now(timezone.utc).date().isoformat()
    assert {r["snapshot_date"] for r in rows} == {today}


# --------------------------------------------------------------- PII (I6)
def test_no_plaintext_pii_anywhere_in_export(export_client):
    hdr = {"X-Internal-Token": TOKEN}
    for path in (
        "/v1/graph/internal/export/nodes?tenant_id=tenant-a",
        "/v1/graph/internal/export/edges?tenant_id=tenant-a",
    ):
        resp = export_client.get(path, headers=hdr)
        assert resp.status_code == 200
        body = resp.text
        for token in FORBIDDEN_TOKENS:
            assert token not in body, f"PII/identifier {token!r} leaked in {path}"
        # every Person reference is a 64-char lowercase hex sha256
        for line in body.splitlines():
            row = json.loads(line)
            for key in ("node_id", "src_id", "dst_id"):
                label = row.get("label") or row.get(
                    "src_label" if key == "src_id" else "dst_label"
                )
                if key in row and label == "Person":
                    value = row[key]
                    assert len(value) == 64 and all(
                        c in "0123456789abcdef" for c in value
                    ), f"unhashed Person id in {path}: {value!r}"


def test_person_hash_matches_w28_scheme(export_client):
    """sha256(salt|tenant|person_id) — the graph-sync PhoneHash scheme."""
    hdr = {"X-Internal-Token": TOKEN}
    expected = hashlib.sha256(f"{SALT}|tenant-a|pa1".encode()).hexdigest()
    assert w28_hash(SALT, "tenant-a", "pa1") == expected
    _, rows = _get_jsonl(
        export_client, "/v1/graph/internal/export/nodes?tenant_id=tenant-a",
        headers=hdr,
    )
    person_ids = {r["node_id"] for r in rows if r["label"] == "Person"}
    assert expected in person_ids


def test_edges_join_nodes_through_shared_hash(export_client):
    """REFERRED pa1->pa2: edge endpoint hashes equal the node export ids."""
    hdr = {"X-Internal-Token": TOKEN}
    _, nodes = _get_jsonl(
        export_client, "/v1/graph/internal/export/nodes?tenant_id=tenant-a",
        headers=hdr,
    )
    node_ids = {r["node_id"] for r in nodes}
    _, edges = _get_jsonl(
        export_client, "/v1/graph/internal/export/edges?tenant_id=tenant-a",
        headers=hdr,
    )
    referred = [r for r in edges if r["edge_type"] == "REFERRED"]
    assert referred, "fixture has a REFERRED edge"
    for row in referred:
        assert row["src_label"] == "Person" and row["dst_label"] == "Person"
        assert row["src_id"] in node_ids and row["dst_id"] in node_ids


# ---------------------------------------------------- feature aggregates
def test_person_feature_aggregates(export_client):
    hdr = {"X-Internal-Token": TOKEN}
    _, rows = _get_jsonl(
        export_client, "/v1/graph/internal/export/nodes?tenant_id=tenant-a",
        headers=hdr,
    )
    by_hash = {r["node_id"]: r for r in rows}
    pa1 = by_hash[w28_hash(SALT, "tenant-a", "pa1")]
    assert pa1["consent_marketing"] is True      # valid marketing consent
    assert pa1["quarantine"] is False
    assert pa1["bookings_total"] == 1            # one BOOKED edge (b1)
    assert pa1["no_show_rate"] == 0.0            # b1 showed=True
    assert pa1["channel_of_first_touch"] == "field_pwa"
    assert pa1["out_degree"] >= 2                # CONSENTED + HAS_CONTACT + ...
    pa4 = by_hash[w28_hash(SALT, "tenant-a", "pa4")]
    assert pa4["quarantine"] is True
    pa2 = by_hash[w28_hash(SALT, "tenant-a", "pa2")]
    assert pa2["consent_marketing"] is False     # no consent edges at all
    pa3 = by_hash[w28_hash(SALT, "tenant-a", "pa3")]
    assert pa3["consent_marketing"] is False     # consent REVOKED
    pa6 = by_hash[w28_hash(SALT, "tenant-a", "pa6")]
    assert pa6["channel_of_first_touch"] is None  # pa6 has no Contact
    # non-Person rows carry label-appropriate nulls
    offerings = [r for r in rows if r["label"] == "Offering"]
    assert offerings and all(r["consent_marketing"] is None for r in offerings)
    contacts = [r for r in rows if r["label"] == "Contact"]
    assert contacts and all(
        r["channel_of_first_touch"] == "field_pwa" for r in contacts
    )
