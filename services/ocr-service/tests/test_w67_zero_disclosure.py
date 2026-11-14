"""SPEC-W67 verification battery (RECON §5) — Coder C, CONTRACT-DRIVEN.

Cross-tenant document-fraud registry: zero cross-tenant disclosure,
oracle/timing invariance, idempotency (re-extract, backfill re-run,
concurrent same-hash race), erasure consistency, RLS/auth, schema + payload
allowlists, and source-grep gates (no phash cross-tenant; app.ocr_registry
GUC only in pinned paths; analytics-pipeline ignores the new event type).

These tests are written against the PINNED SPEC-W67 contracts (table
names/columns, event type com.opendesk.ocr.CrossTenantDocumentMatch with
payload {document_id, sha256, kind, tenant_count}, first-use-wins anchor
semantics) — NOT against Coder A's implementation internals. They run in
the MERGED tree (the verifier runs the full merged suite). On the pristine
tree the registry module/endpoint does not exist and the affected tests
FAIL LOUDLY with a "merged tree required" message — never pass vacuously.

Conftest is Coder A's (read-only here): we use the existing client/deps/
make_png fixtures exactly as test_extract_mock.py/test_backfill.py do. All
additional fixtures (extra tenants, seeding, fake fan-out) are defined
inside THIS file (self-contained rule).

Registry enable idiom (G1): OCR_REGISTRY_HMAC_SECRET unset => registry
DISABLED. Tests set a test secret via monkeypatch on the env var AND the
assumed settings attributes (registry_hmac_secret /
ocr_registry_hmac_secret — the OCR_INTERNAL_TOKEN -> settings.internal_token
naming idiom); a missing wiring fails loudly on the match assertions.
"""

from __future__ import annotations

import ast
import hashlib
import json
import os
import re
import threading
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

from app.config import settings
from app.store import DDL

from conftest import INTERNAL_TOKEN, TENANT_A, TENANT_B, make_png

REPO_ROOT = Path(__file__).resolve().parents[3]
INIT_SQL_PATH = REPO_ROOT / "infra/postgres/init-scripts/31-ocr-schema.sql"
REGISTRY_MODULE_PATH = REPO_ROOT / "services/ocr-service/app/registry.py"
EVENTS_PATH = REPO_ROOT / "services/ocr-service/app/events.py"

MATCH_EVENT_TYPE = "com.opendesk.ocr.CrossTenantDocumentMatch"
MATCH_PAYLOAD_KEYS = {"document_id", "sha256", "kind", "tenant_count"}
BACKFILL_REGISTRY_ROUTE = "/v1/internal/ocr/backfill-document-registry"
BACKFILL_RESPONSE_KEYS = {
    "upserted",
    "memberships_added",
    "matches",
    "scanned",
    "skipped",
    "next_cursor",
}
GUC = "app.ocr_registry"
REGISTRY_TEST_SECRET = "w67-test-registry-secret"
REGISTRY_SECRET_ATTRS = ("registry_hmac_secret", "ocr_registry_hmac_secret")
AUTH = {"X-Internal-Token": INTERNAL_TOKEN}

# Extra tenants (uuids resolve without the identity hop, like TENANT_A/B).
TENANT_C = "22222222-3333-4444-5555-666666666666"
TENANT_D = "33333333-4444-5555-6666-777777777777"

# Pinned schema allowlist (SPEC-W67 "Tables" — NO other columns).
REGISTRY_TABLE_COLUMNS = {
    "ocr_doc_registry": {
        "registry_key",
        "first_seen",
        "last_seen",
        "tenant_count",
        "doc_count",
    },
    "ocr_doc_registry_tenants": {
        "registry_key",
        "tenant_id",
        "first_seen",
        "alerted",
    },
}
BANNED_REGISTRY_COLUMNS = {
    "phash",
    "document_id",
    "storage_key",
    "storage_bucket",
    "kind",
    "extracted_text",
    "extracted_fields",
    "subject_phone",
    "mime",
    "bytes",
}

#: Pinned paths where the GUC string may appear (plus tests/docs).
GUC_PINNED_PATHS = {
    "services/ocr-service/app/registry.py",
    "services/ocr-service/app/store.py",
    "infra/postgres/init-scripts/31-ocr-schema.sql",
}


def _merged_tree_required(condition: bool, what: str) -> None:
    """Fail LOUDLY (never skip, never pass vacuously) when the pinned W67
    surface is absent — expected on the pristine tree, green in the merge."""
    if not condition:
        pytest.fail(
            f"SPEC-W67 merged tree required: {what} is missing at this "
            "restore point (Coder A's ocr registry core). Expected to FAIL "
            "on the pristine tree; must be GREEN in the merged tree."
        )


@pytest.fixture
def registry_on(monkeypatch):
    """Enable the registry with a test secret (G1). The merged tree's
    Settings carries the field (monkeypatch-managed); on the pristine tree
    it does not exist yet, so force-set + delete manually (pydantic
    BaseSettings rejects unknown-field assignment)."""
    monkeypatch.setenv("OCR_REGISTRY_HMAC_SECRET", REGISTRY_TEST_SECRET)
    forced: list[str] = []
    for attr in REGISTRY_SECRET_ATTRS:
        if hasattr(settings, attr):
            monkeypatch.setattr(settings, attr, REGISTRY_TEST_SECRET)
        else:
            object.__setattr__(settings, attr, REGISTRY_TEST_SECRET)
            forced.append(attr)
    yield
    for attr in forced:
        try:
            object.__delattr__(settings, attr)
        except AttributeError:
            pass


@pytest.fixture
def registry_off(monkeypatch):
    """G1 fail-closed posture: secret unset everywhere."""
    monkeypatch.delenv("OCR_REGISTRY_HMAC_SECRET", raising=False)
    forced: list[str] = []
    for attr in REGISTRY_SECRET_ATTRS:
        if hasattr(settings, attr):
            monkeypatch.setattr(settings, attr, "")
        else:
            object.__setattr__(settings, attr, "")
            forced.append(attr)
    yield
    for attr in forced:
        try:
            object.__delattr__(settings, attr)
        except AttributeError:
            pass


def _extract(
    client,
    tenant: str,
    data: bytes,
    *,
    kind: str = "receipt",
    filename: str = "doc.png",
    subject_phone: str | None = None,
):
    """POST /v1/ocr/extract exactly like test_extract_mock._post_receipt."""
    form = {"kind": kind, "tenant": tenant}
    if subject_phone is not None:
        form["subject_phone"] = subject_phone
    return client.post(
        "/v1/ocr/extract",
        files={"file": (filename, data, "image/png")},
        data=form,
        headers=AUTH,
    )


def _match_events(deps) -> list[dict]:
    """CrossTenantDocumentMatch events captured by the FakeDapr sink."""
    return [
        payload
        for _pubsub, topic, payload in deps.dapr.published
        if topic == settings.ocr_events_topic
        and isinstance(payload, dict)
        and payload.get("type") == MATCH_EVENT_TYPE
    ]


def _assert_match_event_contract(evt: dict, *, later_tenant: str) -> None:
    """The pinned zero-disclosure event contract (SPEC-W67 'Match event')."""
    assert evt["tenantid"] == later_tenant, "CloudEvents tenantid = LATER tenant"
    data = evt["data"]
    assert set(data) == MATCH_PAYLOAD_KEYS, (
        f"match payload keys {sorted(data)} != pinned allowlist "
        f"{sorted(MATCH_PAYLOAD_KEYS)}"
    )
    assert evt["subject"] == data["document_id"], "subject = own document_id"
    assert isinstance(data["tenant_count"], int) and data["tenant_count"] >= 2
    assert isinstance(data["document_id"], str)
    assert isinstance(data["sha256"], str) and len(data["sha256"]) == 64
    assert isinstance(data["kind"], str) and data["kind"]


def _assert_no_cross_tenant_leak(evt: dict, forbidden: list[str]) -> None:
    blob = json.dumps(evt)
    for token in forbidden:
        assert token not in blob, (
            f"zero-disclosure: {token!r} (other tenant's identifier) appears "
            "in the later tenant's match event"
        )


# ---------------------------------------------------------------------------
# 1) DDL schema allowlist + no-phash grep gate (source-level, both copies)
# ---------------------------------------------------------------------------
def _create_table_block(sql: str, table: str) -> str | None:
    m = re.search(
        rf"CREATE TABLE IF NOT EXISTS {table}\s*\((.*?)\)\s*;", sql, re.DOTALL
    )
    return m.group(1) if m else None


def _split_top_level_commas(block: str) -> list[str]:
    parts, depth, cur = [], 0, []
    for ch in block:
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
        if ch == "," and depth == 0:
            parts.append("".join(cur))
            cur = []
        else:
            cur.append(ch)
    parts.append("".join(cur))
    return parts


def _column_names(block: str) -> set[str]:
    names = set()
    for part in _split_top_level_commas(block):
        tokens = part.split()
        if not tokens:
            continue
        if tokens[0].upper() in {
            "PRIMARY",
            "FOREIGN",
            "CONSTRAINT",
            "CHECK",
            "UNIQUE",
            "REFERENCES",
        }:
            continue
        names.add(tokens[0].strip('"'))
    return names


def test_registry_ddl_schema_allowlist_both_ddl_copies():
    """Pinned columns EXACTLY, in BOTH DDL copies (store.py + 31-ocr-schema.sql)."""
    init_sql = INIT_SQL_PATH.read_text() if INIT_SQL_PATH.exists() else ""
    for name, sql in (("store.DDL", DDL), ("31-ocr-schema.sql", init_sql)):
        for table, expected in REGISTRY_TABLE_COLUMNS.items():
            block = _create_table_block(sql, table)
            _merged_tree_required(block is not None, f"{table} DDL in {name}")
            cols = _column_names(block)
            assert cols == expected, (
                f"{name}:{table} columns {sorted(cols)} != pinned "
                f"{sorted(expected)} (schema allowlist)"
            )
            assert not (cols & BANNED_REGISTRY_COLUMNS), (
                f"{name}:{table} carries banned column(s) "
                f"{sorted(cols & BANNED_REGISTRY_COLUMNS)} (minimization)"
            )
        for table in REGISTRY_TABLE_COLUMNS:
            assert re.search(
                rf"ALTER TABLE {table} ENABLE ROW LEVEL SECURITY", sql
            ), f"{name}:{table} missing ENABLE RLS"
            assert re.search(
                rf"ALTER TABLE {table} FORCE ROW LEVEL SECURITY", sql
            ), f"{name}:{table} missing FORCE RLS"
        assert GUC in sql, (
            f"{name}: registry policy pass must be the {GUC} GUC (G4)"
        )


def test_registry_ddl_excludes_phash_both_copies():
    """G2 grep gate: 'phash' never appears in the registry DDL section."""
    init_sql = INIT_SQL_PATH.read_text() if INIT_SQL_PATH.exists() else ""
    for name, sql in (("store.DDL", DDL), ("31-ocr-schema.sql", init_sql)):
        idx = sql.find("ocr_doc_registry")
        _merged_tree_required(idx >= 0, f"registry DDL in {name}")
        registry_section = sql[idx:].lower()
        assert "phash" not in registry_section, (
            f"{name}: phash is EXCLUDED from every cross-tenant artifact (G2)"
        )


def test_registry_module_hmac_keyed_and_phash_free():
    """G2/G3 grep gate on the registry module: HMAC-keyed, phash-free,
    sole owner of the GUC escape."""
    _merged_tree_required(
        REGISTRY_MODULE_PATH.exists(), "services/ocr-service/app/registry.py"
    )
    src = REGISTRY_MODULE_PATH.read_text()
    _merged_tree_required(src.strip(), "non-empty registry module")
    assert "phash" not in src.lower(), (
        "registry module must never touch phash — sha256-EXACT is the only "
        "cross-tenant signal (G2)"
    )
    assert "hmac" in src.lower(), (
        "registry_key = HMAC-SHA256(OCR_REGISTRY_HMAC_SECRET, sha256) (G3) — "
        "no hmac usage found in the registry module"
    )
    assert GUC in src, (
        f"the registry module must be the dedicated connection path that "
        f"sets {GUC} (G4)"
    )


def test_match_event_builder_never_references_phash():
    """G2 grep gate on the match-event builder (events.py or registry.py):
    whichever function(s) build CrossTenantDocumentMatch must not reference
    phash (payload allowlist is asserted behaviorally below)."""
    candidates: list[tuple[str, str, str]] = []
    for path in (EVENTS_PATH, REGISTRY_MODULE_PATH):
        if not path.exists():
            continue
        src = path.read_text()
        if "CrossTenantDocumentMatch" not in src:
            continue
        const_names = set(
            re.findall(
                r"([A-Z][A-Z0-9_]*)\s*=\s*[\"']"
                + re.escape(MATCH_EVENT_TYPE)
                + r"[\"']",
                src,
            )
        )
        tokens = {MATCH_EVENT_TYPE, "CrossTenantDocumentMatch"} | const_names
        tree = ast.parse(src)
        for node in ast.walk(tree):
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                seg = ast.get_source_segment(src, node) or ""
                if any(tok in seg for tok in tokens):
                    candidates.append((path.name, node.name, seg))
    _merged_tree_required(
        bool(candidates),
        f"a builder function for {MATCH_EVENT_TYPE} in events.py/registry.py",
    )
    for fname, fn, seg in candidates:
        assert "phash" not in seg.lower(), (
            f"{fname}:{fn} builds the cross-tenant match event and must not "
            "reference phash (G2)"
        )


def test_guc_string_only_in_pinned_paths():
    """G4 grep gate: the app.ocr_registry GUC string appears ONLY in the
    registry module, the two DDL copies, tests, and docs."""
    hits: list[str] = []
    skip_dirs = {
        ".git",
        "node_modules",
        "__pycache__",
        ".pytest_cache",
        ".mypy_cache",
        ".venv",
        "dist",
        "build",
    }
    for path in REPO_ROOT.rglob("*"):
        if not path.is_file() or any(p in skip_dirs for p in path.parts):
            continue
        try:
            text = path.read_text(errors="ignore")
        except (OSError, UnicodeError):
            continue
        if GUC in text:
            hits.append(path.relative_to(REPO_ROOT).as_posix())
    allowed = (
        lambda rel: rel in GUC_PINNED_PATHS
        or rel.startswith("services/ocr-service/tests/")
        or rel.startswith("docs/")
        or rel.startswith("tests/e2e/")
    )
    bad = [h for h in hits if not allowed(h)]
    assert not bad, (
        f"{GUC} found OUTSIDE the pinned paths (G4: set only inside the "
        f"registry module's dedicated connection path): {bad}"
    )
    _merged_tree_required(
        "services/ocr-service/app/registry.py" in hits,
        f"registry module setting {GUC}",
    )
    assert "services/ocr-service/app/store.py" in hits, "DDL policy copy 1"
    assert "infra/postgres/init-scripts/31-ocr-schema.sql" in hits, "DDL copy 2"


# ---------------------------------------------------------------------------
# 2) Zero cross-tenant disclosure + first-use-wins (behavioral, two tenants)
# ---------------------------------------------------------------------------
def test_two_tenant_first_use_wins_match_event_zero_disclosure(
    client, deps, monkeypatch
, registry_on):
    """Earlier tenant gets NOTHING; later tenant gets ONE event addressed to
    itself, carrying only its own ids + tenant_count."""
    payload = make_png(seed=11)
    digest = hashlib.sha256(payload).hexdigest()

    r1 = _extract(client, TENANT_A, payload)
    assert r1.status_code == 200, r1.text
    doc_a = r1.json()["document_id"]
    assert _match_events(deps) == [], (
        "first-use-wins (G5): the FIRST tenant for a key must never be alerted"
    )

    r2 = _extract(client, TENANT_B, payload)
    assert r2.status_code == 200, r2.text
    doc_b = r2.json()["document_id"]

    matches = _match_events(deps)
    _merged_tree_required(
        bool(matches),
        "CrossTenantDocumentMatch emission on the extract path (assumed "
        "wiring: OCR_REGISTRY_HMAC_SECRET -> settings.registry_hmac_secret)",
    )
    assert len(matches) == 1, (
        f"exactly one match event per (later tenant, hash); got {len(matches)}"
    )
    evt = matches[0]
    _assert_match_event_contract(evt, later_tenant=TENANT_B)
    assert evt["data"]["document_id"] == doc_b, "payload carries OWN document_id"
    assert evt["data"]["sha256"] == digest, "payload sha256 is the own bytes' digest"
    assert evt["data"]["kind"] == "receipt"
    assert evt["data"]["tenant_count"] == 2
    _assert_no_cross_tenant_leak(evt, [TENANT_A, doc_a])
    # The earlier tenant receives NO match event, ever.
    for evt in _match_events(deps):
        assert evt["tenantid"] != TENANT_A, (
            "earlier tenant must NEVER receive a CrossTenantDocumentMatch (G5)"
        )


def test_no_match_for_unique_document(client, deps, monkeypatch, registry_on):
    assert _extract(client, TENANT_A, make_png(seed=12)).status_code == 200
    assert _extract(client, TENANT_B, make_png(seed=13)).status_code == 200
    assert _match_events(deps) == []


# ---------------------------------------------------------------------------
# 3) Oracle/timing: extract response byte-identity match vs no-match (G5)
# ---------------------------------------------------------------------------
def test_extract_response_invariance_match_vs_no_match(client, deps, monkeypatch, registry_on):
    shared = make_png(seed=21)
    other = make_png(seed=22)
    assert _extract(client, TENANT_A, shared).status_code == 200

    resp_match = _extract(client, TENANT_B, shared)  # fires the registry match
    resp_none = _extract(client, TENANT_B, other)  # unique bytes, no match
    # The comparison is only meaningful if the match REALLY fired — never
    # pass vacuously on a pristine/disabled tree.
    _merged_tree_required(
        len(_match_events(deps)) == 1,
        "a real match event (response-invariance comparison would be vacuous)",
    )
    assert resp_match.status_code == resp_none.status_code == 200
    assert (
        resp_match.headers["content-type"] == resp_none.headers["content-type"]
    )
    # Byte-identity modulo the caller's OWN document_id (the one field that
    # legitimately differs per upload).
    body_match = resp_match.content.replace(
        resp_match.json()["document_id"].encode(), b"<DOC>"
    )
    body_none = resp_none.content.replace(
        resp_none.json()["document_id"].encode(), b"<DOC>"
    )
    assert body_match == body_none, (
        "oracle/timing gate: the extract response must be byte-identical "
        "(status, headers, body modulo own document_id) whether or not a "
        "cross-tenant match fired"
    )


def test_registry_disabled_when_secret_unset(client, deps, monkeypatch, registry_off):
    """G1 fail-closed: unset OCR_REGISTRY_HMAC_SECRET => registry DISABLED,
    upsert skipped, NO match — and the extract flow is otherwise unchanged."""
    payload = make_png(seed=31)
    r1 = _extract(client, TENANT_A, payload)
    r2 = _extract(client, TENANT_B, payload)
    assert r1.status_code == r2.status_code == 200
    assert _match_events(deps) == [], (
        "registry must be DISABLED when OCR_REGISTRY_HMAC_SECRET is unset "
        "(G1 fail-closed, KYC_HASH_SECRET idiom)"
    )


# ---------------------------------------------------------------------------
# 4) Idempotency: re-extract + concurrent same-hash race
# ---------------------------------------------------------------------------
def test_reextract_same_tenant_same_hash_emits_no_second_match(
    client, deps, monkeypatch
, registry_on):
    """Re-extract (same tenant, same bytes, new document_id) re-upserts the
    SAME membership: the alerted flag gives at-most-one event per
    (tenant, hash)."""
    payload = make_png(seed=41)
    assert _extract(client, TENANT_A, payload).status_code == 200
    assert _extract(client, TENANT_B, payload).status_code == 200
    assert _extract(client, TENANT_B, payload).status_code == 200  # re-extract
    matches = _match_events(deps)
    _merged_tree_required(bool(matches), "match event emission")
    assert len(matches) == 1, (
        "at-most-one match event per (tenant, hash) — re-extract must not "
        "re-emit (alerted flag + ON CONFLICT DO NOTHING)"
    )


def test_concurrent_same_hash_two_tenants_exactly_one_match(
    client, deps, monkeypatch
, registry_on):
    """Race gate: concurrent same-hash first-uploads at two tenants serialize
    on the registry PK row lock => exactly one match event total."""
    payload = make_png(seed=42)
    barrier = threading.Barrier(2)
    results: dict[str, object] = {}
    errors: list[BaseException] = []

    def upload(tenant: str) -> None:
        try:
            barrier.wait(timeout=15)
            results[tenant] = _extract(client, tenant, payload)
        except BaseException as exc:  # noqa: BLE001 - surface in assertion
            errors.append(exc)

    threads = [
        threading.Thread(target=upload, args=(t,)) for t in (TENANT_A, TENANT_B)
    ]
    for t in threads:
        t.start()
    for t in threads:
        t.join(timeout=60)
    assert not errors, f"concurrent uploads errored: {errors!r}"
    assert set(results) == {TENANT_A, TENANT_B}
    assert all(r.status_code == 200 for r in results.values())
    matches = _match_events(deps)
    _merged_tree_required(bool(matches), "match event emission under race")
    assert len(matches) == 1, (
        "PK row lock serializes concurrent first-uploads: exactly one alert "
        f"total for the later tenant; got {len(matches)}"
    )
    evt = matches[0]
    assert evt["tenantid"] in (TENANT_A, TENANT_B)
    assert evt["data"]["tenant_count"] == 2


# ---------------------------------------------------------------------------
# 5) Erasure consistency (G7)
# ---------------------------------------------------------------------------
def test_subject_erasure_leaves_registry_untouched(client, deps, monkeypatch, registry_on):
    """Subject erasure tombstones the ocr row (hash retained for audit) and
    MUST NOT touch the registry (no subject linkage): a third tenant
    uploading the same bytes still matches."""
    phone = "+2348011112222"
    payload = make_png(seed=51)
    ra = _extract(
        client, TENANT_A, payload, kind="kyc_nin_slip", subject_phone=phone
    )
    assert ra.status_code == 200, ra.text
    rb = _extract(client, TENANT_B, payload, kind="kyc_nin_slip")
    assert rb.status_code == 200, rb.text
    matches = _match_events(deps)
    _merged_tree_required(len(matches) == 1, "live match before erasure")
    assert matches[0]["tenantid"] == TENANT_B

    erased = client.post(
        "/v1/ocr/events/consent-erasure",
        json={
            "specversion": "1.0",
            "id": str(uuid.uuid4()),
            "source": "identity-service",
            "type": "com.opendesk.consent.ErasureRequested",
            "subject": phone,
            "tenantid": TENANT_A,
            "time": datetime.now(timezone.utc).isoformat(),
            "data": {
                "tenant_id": TENANT_A,
                "data_subject_id": phone,
                "purpose": "kyc_document",
                "erased_records": 1,
                "erasure_ts": datetime.now(timezone.utc).isoformat(),
            },
        },
    )
    assert erased.status_code == 200, erased.text
    assert erased.json()["status"] == "SUCCESS"
    assert erased.json()["erased"] == 1
    # Tombstone retains the hash (audit idiom).
    row_a = deps.db.rows[ra.json()["document_id"]]
    assert row_a["status"] == "erased" and row_a["sha256"]

    rc = _extract(client, TENANT_C, payload, kind="kyc_nin_slip")
    assert rc.status_code == 200, rc.text
    matches = _match_events(deps)
    assert len(matches) == 2, (
        "subject erasure must leave registry rows/memberships untouched "
        "(G7: no subject linkage) — the third tenant still matches"
    )
    evt = matches[-1]
    _assert_match_event_contract(evt, later_tenant=TENANT_C)
    assert evt["data"]["tenant_count"] == 3, (
        "erased subject's tenant membership is retained by design "
        "(tombstone precedent)"
    )
    _assert_no_cross_tenant_leak(
        evt, [TENANT_A, TENANT_B, ra.json()["document_id"], rb.json()["document_id"]]
    )


def test_tenant_delete_removes_only_own_memberships(client, deps, monkeypatch, registry_on):
    """TenantDelete removes ONLY the departing tenant's own memberships;
    surviving members keep the parent fingerprint row alive (and their
    match protection with it); the parent row drops ONLY when its LAST
    membership leaves."""
    payload = make_png(seed=61)
    assert _extract(client, TENANT_A, payload).status_code == 200
    assert _extract(client, TENANT_B, payload).status_code == 200
    matches = _match_events(deps)
    _merged_tree_required(len(matches) == 1, "live match before TenantDelete")
    assert matches[0]["tenantid"] == TENANT_B
    assert len(deps.db.registry_keys) == 1, "one fingerprint row for the bytes"
    assert len(deps.db.registry_members) == 2, "A's and B's memberships"

    def tenant_deleted(tenant: str):
        return client.post(
            "/v1/ocr/events/tenant-deleted",
            json={
                "specversion": "1.0",
                "id": str(uuid.uuid4()),
                "source": "identity-service",
                "type": "com.opendesk.identity.TenantDeleted",
                "subject": tenant,
                "tenantid": tenant,
                "time": datetime.now(timezone.utc).isoformat(),
                "data": {
                    "tenant_id": tenant,
                    "tenant_slug": tenant,
                    "deleted_at": datetime.now(timezone.utc).isoformat(),
                    "actor": "w67-test",
                },
            },
        )

    resp = tenant_deleted(TENANT_A)
    assert resp.status_code == 200 and resp.json()["status"] == "SUCCESS"

    # Surviving-member protection (G7): ONLY A's own membership is removed;
    # B's membership — and therefore the parent fingerprint row — survives.
    assert len(deps.db.registry_members) == 1, (
        "TenantDelete removed ONLY A's own membership; B's survives"
    )
    assert {m["tenant_id"] for m in deps.db.registry_members.values()} == {
        TENANT_B
    }, "the surviving membership is B's, not the departed tenant's"
    assert len(deps.db.registry_keys) == 1, (
        "parent fingerprint row stays alive while ANY membership remains"
    )
    assert next(iter(deps.db.registry_keys.values()))["tenant_count"] == 1, (
        "tenant_count re-counted down to the single surviving member"
    )

    # B's protection is intact: a new tenant C still matches, with count 2
    # (B + C), NOT 3 (A must have been removed).
    assert _extract(client, TENANT_C, payload).status_code == 200
    matches = _match_events(deps)
    assert len(matches) == 2, "C must still match B's surviving membership"
    evt = matches[-1]
    _assert_match_event_contract(evt, later_tenant=TENANT_C)
    assert evt["data"]["tenant_count"] == 2, (
        "TenantDelete removed A's membership (count is B + C, not A + B + C)"
    )

    # Delete BOTH remaining members (B and C): only now, with the LAST
    # membership gone, does the parent registry row drop.
    for gone in (TENANT_B, TENANT_C):
        resp = tenant_deleted(gone)
        assert resp.status_code == 200 and resp.json()["status"] == "SUCCESS"
    assert len(deps.db.registry_members) == 0, (
        "B's and C's own TenantDelete events removed their memberships"
    )
    assert len(deps.db.registry_keys) == 0, (
        "parent fingerprint row dropped at zero memberships"
    )

    # Post-zero: no prior member remains, so a later upload of the same
    # bytes matches NOTHING.
    assert _extract(client, TENANT_D, payload).status_code == 200
    matches = _match_events(deps)
    assert len(matches) == 2, (
        "parent registry row dropped at zero memberships — a later upload "
        "of the same bytes must NOT match (no new event beyond B's and C's)"
    )
    assert all(e["tenantid"] != TENANT_D for e in matches)


# ---------------------------------------------------------------------------
# 6) Backfill endpoint: auth (503/401), dry_run, idempotent re-run (A's
#    endpoint; C codes against the pinned contract only)
# ---------------------------------------------------------------------------
def _seed_extracted(
    deps,
    tenant: str,
    *,
    sha256: str,
    created_at: datetime,
    kind: str = "receipt",
    doc_id: uuid.UUID | None = None,
) -> uuid.UUID:
    """Insert one status='extracted' ocr_documents row directly (store-level
    fixture; test_backfill.seed idiom) — bypasses the live registry hook so
    the backfill has work to do."""
    doc_id = doc_id or uuid.uuid4()
    deps.db.rows[str(doc_id)] = {
        "id": doc_id,
        "tenant_id": tenant,
        "kind": kind,
        "storage_bucket": "documents",
        "storage_key": f"{tenant}/{doc_id}.png",
        "mime": "image/png",
        "bytes": 128,
        "sha256": sha256,
        "phash": None,
        "page_count": 1,
        "model_version": "minicpm-ocr-v1+mock",
        "extracted_text": None,
        "extracted_fields": {},
        "status": "extracted",
        "error": None,
        "expires_at": None,
        "subject_phone": None,
        "created_at": created_at,
        "updated_at": created_at,
    }
    return doc_id


def _route_registered(client, path: str) -> bool:
    return any(getattr(r, "path", None) == path for r in client.app.router.routes)


def test_backfill_registry_auth_fail_closed(client, deps, monkeypatch):
    _merged_tree_required(
        _route_registered(client, BACKFILL_REGISTRY_ROUTE),
        f"route POST {BACKFILL_REGISTRY_ROUTE}",
    )
    # 503 when OCR_INTERNAL_TOKEN unset (fail-closed, erase-endpoint idiom).
    monkeypatch.setattr(settings, "internal_token", "")
    resp = client.post(
        BACKFILL_REGISTRY_ROUTE, json={}, headers={"X-Internal-Token": "x"}
    )
    assert resp.status_code == 503
    # 401 on wrong and on missing token.
    monkeypatch.setattr(settings, "internal_token", INTERNAL_TOKEN)
    assert (
        client.post(
            BACKFILL_REGISTRY_ROUTE,
            json={},
            headers={"X-Internal-Token": "wrong"},
        ).status_code
        == 401
    )
    assert client.post(BACKFILL_REGISTRY_ROUTE, json={}).status_code == 401


def test_backfill_registry_dry_run_then_emit_then_idempotent_rerun(
    client, deps, monkeypatch
, registry_on):
    _merged_tree_required(
        _route_registered(client, BACKFILL_REGISTRY_ROUTE),
        f"route POST {BACKFILL_REGISTRY_ROUTE}",
    )
    t0 = datetime(2026, 2, 1, 12, 0, 0, tzinfo=timezone.utc)
    sha_shared = "ab" * 32
    sha_solo = "cd" * 32
    doc_a = _seed_extracted(deps, TENANT_A, sha256=sha_shared, created_at=t0)
    doc_b = _seed_extracted(
        deps, TENANT_B, sha256=sha_shared, created_at=t0 + timedelta(minutes=5)
    )
    _seed_extracted(
        deps, TENANT_A, sha256=sha_solo, created_at=t0 + timedelta(minutes=10)
    )

    # dry_run: counts, writes NOTHING, emits NOTHING.
    resp = client.post(BACKFILL_REGISTRY_ROUTE, json={"dry_run": True}, headers=AUTH)
    assert resp.status_code == 200, resp.text
    page = resp.json()
    assert BACKFILL_RESPONSE_KEYS <= set(page), (
        f"backfill response keys {sorted(page)} miss pinned "
        f"{sorted(BACKFILL_RESPONSE_KEYS - set(page))}"
    )
    assert page["scanned"] == 3
    # dry_run writes NOTHING (the pinned part); `matches` may be reported
    # as a prospective sizing count, so only the write counters are pinned.
    assert page["upserted"] == 0 and page["memberships_added"] == 0
    assert _match_events(deps) == [], "dry_run must never emit events"

    # real run: first-use-wins chronology ((created_at, id) keyset) makes
    # TENANT_A the first user of the shared hash; TENANT_B (later) matches.
    resp = client.post(BACKFILL_REGISTRY_ROUTE, json={}, headers=AUTH)
    assert resp.status_code == 200, resp.text
    page = resp.json()
    assert page["scanned"] == 3 and page["memberships_added"] == 3
    assert page["matches"] == 1, page
    matches = _match_events(deps)
    assert len(matches) == 1
    evt = matches[0]
    _assert_match_event_contract(evt, later_tenant=TENANT_B)
    assert evt["data"]["document_id"] == str(doc_b)
    assert evt["data"]["sha256"] == sha_shared
    assert evt["data"]["tenant_count"] == 2
    _assert_no_cross_tenant_leak(evt, [TENANT_A, str(doc_a)])

    # re-run: same upsert fn as the live path => zero new memberships,
    # zero new matches, zero new events (one membership set, one alert).
    resp = client.post(BACKFILL_REGISTRY_ROUTE, json={}, headers=AUTH)
    assert resp.status_code == 200, resp.text
    page = resp.json()
    assert page["scanned"] == 3
    assert page["memberships_added"] == 0 and page["matches"] == 0
    assert len(_match_events(deps)) == 1, (
        "backfill re-run must not re-alert (alerted flag, at-most-one per "
        "(tenant, hash))"
    )


def test_backfill_after_live_registration_adds_nothing(client, deps, monkeypatch, registry_on):
    """Live-registered documents are invisible to the backfill's write side:
    a backfill over live-registered rows adds zero memberships/matches."""
    _merged_tree_required(
        _route_registered(client, BACKFILL_REGISTRY_ROUTE),
        f"route POST {BACKFILL_REGISTRY_ROUTE}",
    )
    payload = make_png(seed=71)
    assert _extract(client, TENANT_A, payload).status_code == 200
    assert _extract(client, TENANT_B, payload).status_code == 200
    _merged_tree_required(len(_match_events(deps)) == 1, "live match")
    resp = client.post(BACKFILL_REGISTRY_ROUTE, json={}, headers=AUTH)
    assert resp.status_code == 200, resp.text
    page = resp.json()
    assert page["memberships_added"] == 0 and page["matches"] == 0, page
    assert len(_match_events(deps)) == 1


# ---------------------------------------------------------------------------
# 7) Metrics (platform-side observability, G5)
# ---------------------------------------------------------------------------
def test_registry_metrics_exposed_after_match(client, deps, monkeypatch, registry_on):
    payload = make_png(seed=81)
    assert _extract(client, TENANT_A, payload).status_code == 200
    assert _extract(client, TENANT_B, payload).status_code == 200
    _merged_tree_required(len(_match_events(deps)) == 1, "live match")
    text = client.get("/metrics").text
    keys = re.search(r"^\w*registry_keys_total\s+([\d.]+)", text, re.M)
    matches = re.search(r"^\w*registry_matches_total\s+([\d.]+)", text, re.M)
    assert keys and float(keys.group(1)) >= 1, (
        "registry_keys_total metric missing on /metrics (G5 observability)"
    )
    assert matches and float(matches.group(1)) >= 1, (
        "registry_matches_total metric missing/stale on /metrics (G5)"
    )


# ---------------------------------------------------------------------------
# 8) analytics-pipeline tolerates the new event type (SPEC verification
#    gate 9, unit-level): the match event must NOT land in
#    bronze.receipt_documents. mapping.py is pure stdlib — loaded by path.
# ---------------------------------------------------------------------------
def test_analytics_pipeline_ignores_cross_tenant_match_event():
    mapping_path = (
        REPO_ROOT / "services/analytics-pipeline/analytics_pipeline/mapping.py"
    )
    _merged_tree_required(mapping_path.exists(), "analytics-pipeline mapping.py")
    import importlib.util

    spec = importlib.util.spec_from_file_location(
        "w67_analytics_mapping", mapping_path
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    event = {
        "specversion": "1.0",
        "id": str(uuid.uuid4()),
        "source": "ocr-service",
        "type": MATCH_EVENT_TYPE,
        "subject": str(uuid.uuid4()),
        "tenantid": TENANT_B,
        "time": datetime.now(timezone.utc).isoformat(),
        "data": {
            "document_id": str(uuid.uuid4()),
            "sha256": "ef" * 32,
            "kind": "receipt",  # worst case: receipt-kind match event
            "tenant_count": 2,
        },
    }
    row = mod.map_ocr_document(event)
    assert row is None, (
        "gate 9: analytics-pipeline must IGNORE com.opendesk.ocr."
        "CrossTenantDocumentMatch — it must NOT land in "
        "bronze.receipt_documents (map_ocr_document currently keys on "
        "data.kind only, never on the envelope type)"
    )


# ---------------------------------------------------------------------------
# 9) DB-gated: schema allowlist via information_schema + FORCE RLS + the
#    GUC-less read sees zero rows (skips cleanly without a test Postgres;
#    same gate as test_rls.py / test_store_ddl.py)
# ---------------------------------------------------------------------------
@pytest.mark.skipif(
    not os.environ.get("OCR_TEST_DATABASE_URL"),
    reason="OCR_TEST_DATABASE_URL unset — no test Postgres available",
)
async def test_registry_schema_allowlist_information_schema_and_rls():
    from app.store import Database

    db = Database(os.environ["OCR_TEST_DATABASE_URL"])
    try:
        await db.connect()
        await db.ensure_ocr_documents_table()
        async with db._pool.acquire() as conn:
            rows = await conn.fetch(
                """
                SELECT table_name, column_name
                  FROM information_schema.columns
                 WHERE table_schema = 'public'
                   AND table_name IN ('ocr_doc_registry',
                                      'ocr_doc_registry_tenants')
                """
            )
            found: dict[str, set[str]] = {}
            for r in rows:
                found.setdefault(r["table_name"], set()).add(r["column_name"])
            assert found == REGISTRY_TABLE_COLUMNS, (
                f"information_schema columns {found} != pinned allowlist "
                f"{REGISTRY_TABLE_COLUMNS}"
            )
            rels = await conn.fetch(
                """
                SELECT relname, relrowsecurity, relforcerowsecurity
                  FROM pg_class
                 WHERE relname = ANY($1)
                """,
                list(REGISTRY_TABLE_COLUMNS),
            )
            assert len(rels) == 2
            for r in rels:
                assert r["relrowsecurity"] and r["relforcerowsecurity"], (
                    f"{r['relname']} must have ENABLE + FORCE RLS (G4)"
                )
            # Without the GUC a non-superuser app role reads ZERO rows.
            is_super = await conn.fetchval("SELECT current_setting('is_superuser')")
            if is_super == "off":
                for table in REGISTRY_TABLE_COLUMNS:
                    n = await conn.fetchval(f"SELECT count(*) FROM {table}")
                    assert n == 0, (
                        f"{table}: read without the {GUC} GUC must return "
                        "zero rows (FORCE RLS)"
                    )
    finally:
        await db.aclose()
