"""SPEC-W67 S4 battery (RECON §5) — Coder C, CONTRACT-DRIVEN.

D9 S4 ``cross_tenant_exact_duplicate``: graph-sync projects
``seen_elsewhere_count`` onto the LATER tenant's OWN (:Document) node from
``com.opendesk.ocr.CrossTenantDocumentMatch``; D9's existing tenant-bound
sweep raises S4 for Document nodes with ``seen_elsewhere_count >= 2``:
severity HIGH, dedup_key ``xtenant:{sha256}``, alert_id
``document_abuse:{T}:none:xtenant:{sha256}``, person_id=None, never
auto-quarantine, ZERO-DISCLOSURE evidence (own document ids + count ONLY —
no other tenant id, no other document ids, no phash).

Written against the PINNED SPEC-W67 contracts, not Coder B's internals.
SELF-CONTAINED (conftest/fakes belong to B; we subclass the shared fake
in-file to project the pinned additive property). On the pristine tree the
S4 signal / graph-sync decode contract do not exist and the affected tests
FAIL LOUDLY with a "merged tree required" message — never pass vacuously.
"""

from __future__ import annotations

import json
import re
from datetime import timedelta
from pathlib import Path

import pytest

from fraud_engine.detectors import DetectionRunner
from fraud_engine.detectors.d9_document import DOCUMENTS_CYPHER, DocumentAbuseDetector
from fraud_engine.quarantine import AUTO_QUARANTINE_TYPES

from conftest import NOW, OTHER_TENANT, TENANT, ts
from fakes import FakeGraphClient, PropertyGraph

REPO_ROOT = Path(__file__).resolve().parents[3]
GS_EVENTS = REPO_ROOT / "services/graph-sync/internal/events/cloudevents.go"
GS_SYNCER = REPO_ROOT / "services/graph-sync/internal/consumer/syncer.go"
GS_QUERIES = REPO_ROOT / "services/graph-sync/internal/graph/queries.go"
FRAUD_ROOT = REPO_ROOT / "services/fraud-engine"

MATCH_EVENT_TYPE = "com.opendesk.ocr.CrossTenantDocumentMatch"
MATCH_PAYLOAD_JSON_TAGS = {"document_id", "sha256", "kind", "tenant_count"}
S4_SIGNAL = "cross_tenant_exact_duplicate"

SHA = "e" * 64
SHA2 = "f" * 64

#: Zero-disclosure evidence allowlist for S4 (own ids + count + metadata).
S4_EVIDENCE_ALLOWLIST = {
    "detector",
    "signal",
    "document_id",
    "document_ids",
    "sha256",
    "seen_elsewhere_count",
    "count",
    "tenant_count",
    "kind",
    "severity_rule",
    "threshold",
}


class W67RegistryFakeClient(FakeGraphClient):
    """Shared fake + the pinned W67 additive Document property
    ``seen_elsewhere_count`` on the d9 detector rows. Self-contained on
    purpose: Coder C must not modify conftest.py/fakes.py (B's surface);
    this subclass mirrors the merged-tree behavior where the d9 Cypher also
    returns d.seen_elsewhere_count."""

    def _h_detector_d9_documents(self, cypher, params):
        rows = super()._h_detector_d9_documents(cypher, params)
        counts = {}
        for node in self.g.nodes.values():
            if "Document" not in node["labels"]:
                continue
            props = node["props"]
            if props.get("tenant_id") == params["tenant_id"]:
                counts[props.get("document_id")] = props.get("seen_elsewhere_count")
        for row in rows:
            row["seen_elsewhere_count"] = counts.get(row["document_id"])
        return rows


@pytest.fixture
def w67_graph() -> PropertyGraph:
    return PropertyGraph()


@pytest.fixture
def w67_client(w67_graph) -> W67RegistryFakeClient:
    return W67RegistryFakeClient(w67_graph)


def _add_doc(
    g: PropertyGraph,
    tenant: str,
    document_id: str,
    *,
    sha256: str = SHA,
    seen_elsewhere_count: int | None = None,
    kind: str = "receipt",
    minutes_ago: float = 60,
):
    """Project one hash-only (:Document) stub (SPEC-W55 field set) plus the
    pinned W67 additive property seen_elsewhere_count (graph-sync output)."""
    props = dict(
        sha256=sha256,
        phash=None,
        kind=kind,
        status="extracted",
        created_at=ts(minutes_ago),
    )
    if seen_elsewhere_count is not None:
        props["seen_elsewhere_count"] = seen_elsewhere_count
    return g.add_document(tenant, document_id, **props)


def _s4(findings):
    return [f for f in findings if f.evidence.get("signal") == S4_SIGNAL]


def _merged_tree_required(condition: bool, what: str) -> None:
    if not condition:
        pytest.fail(
            f"SPEC-W67 merged tree required: {what} is missing at this "
            "restore point (Coder B's graph-sync/D9 S4 core). Expected to "
            "FAIL on the pristine tree; must be GREEN in the merged tree."
        )


# ---------------------------------------------------------------------------
# 1) S4 firing contract
# ---------------------------------------------------------------------------
def test_s4_fires_high_at_seen_elsewhere_count_two(w67_client, w67_graph, settings):
    _add_doc(w67_graph, TENANT, "doc-later", sha256=SHA, seen_elsewhere_count=2)
    findings = DocumentAbuseDetector().detect(w67_client, TENANT, settings, NOW)
    s4 = _s4(findings)
    _merged_tree_required(bool(s4), f"D9 S4 signal {S4_SIGNAL!r} (seen_elsewhere_count >= 2)")
    assert len(s4) == 1
    f = s4[0]
    assert f.type == "document_abuse"
    assert f.severity == "high", (
        "exact-byte reuse across tenants is the strongest document-fraud "
        "signal — S4 is HIGH (pinned)"
    )
    assert f.dedup_key == f"xtenant:{SHA}"
    assert f.person_id is None, "cross-document finding — no single subject"
    assert f.evidence["detector"] == "d9_document_abuse"


def test_s4_silent_below_two_and_when_absent(w67_client, w67_graph, settings):
    _add_doc(w67_graph, TENANT, "doc-solo", sha256=SHA)  # property absent
    _add_doc(w67_graph, TENANT, "doc-one", sha256=SHA2, seen_elsewhere_count=1)
    findings = DocumentAbuseDetector().detect(w67_client, TENANT, settings, NOW)
    assert _s4(findings) == [], (
        "seen_elsewhere_count < 2 (or unset) must not raise S4 — the later "
        "tenant only learns 'seen on >= 2 tenants'"
    )


def test_s4_does_not_leak_into_s1(w67_client, w67_graph, settings):
    """A single cross-tenant-matched document is NOT a same-tenant exact
    duplicate: S1 stays silent for one local copy."""
    _add_doc(w67_graph, TENANT, "doc-later", sha256=SHA, seen_elsewhere_count=2)
    findings = DocumentAbuseDetector().detect(w67_client, TENANT, settings, NOW)
    assert [f for f in findings if f.evidence.get("signal") == "exact_duplicate"] == []
    _merged_tree_required(bool(_s4(findings)), f"D9 S4 signal {S4_SIGNAL!r}")


# ---------------------------------------------------------------------------
# 2) Zero-disclosure evidence schema
# ---------------------------------------------------------------------------
def test_s4_evidence_zero_disclosure(w67_client, w67_graph, settings):
    """Evidence carries OWN document ids + count ONLY: no other tenant id,
    no other document id, no phash, keys inside the pinned allowlist."""
    _add_doc(w67_graph, OTHER_TENANT, "doc-earlier", sha256=SHA)
    _add_doc(w67_graph, TENANT, "doc-later", sha256=SHA, seen_elsewhere_count=3)
    findings = DocumentAbuseDetector().detect(w67_client, TENANT, settings, NOW)
    s4 = _s4(findings)
    _merged_tree_required(bool(s4), f"D9 S4 signal {S4_SIGNAL!r}")
    ev = s4[0].evidence
    extra = set(ev) - S4_EVIDENCE_ALLOWLIST
    assert not extra, f"S4 evidence keys outside the zero-disclosure allowlist: {sorted(extra)}"
    blob = json.dumps(ev, default=str)
    assert OTHER_TENANT not in blob, "other tenant id leaked into S4 evidence"
    assert "doc-earlier" not in blob, "other tenant's document id leaked into S4 evidence"
    assert '"phash"' not in blob and "phashes" not in blob, (
        "phash is EXCLUDED from every cross-tenant artifact incl. S4 evidence (G2)"
    )
    own_ids = ev.get("document_ids")
    if own_ids is None and "document_id" in ev:
        own_ids = [ev["document_id"]]
    assert own_ids and all(
        str(d).startswith("doc-later") or d == "doc-later" for d in own_ids
    ), f"S4 evidence must carry ONLY the tenant's own document ids: {own_ids}"
    count = ev.get("seen_elsewhere_count", ev.get("count", ev.get("tenant_count")))
    assert isinstance(count, int) and count >= 2, (
        "the count is the only cross-tenant-derived datum in evidence"
    )


# ---------------------------------------------------------------------------
# 3) Alert write path: pinned alert_id, MERGE dedup, no auto-quarantine
# ---------------------------------------------------------------------------
def test_s4_runner_alert_id_and_mergededup(w67_client, w67_graph, settings, publisher):
    _add_doc(w67_graph, TENANT, "doc-later", sha256=SHA, seen_elsewhere_count=2)
    runner = DetectionRunner(
        w67_client, publisher, settings, detectors=[DocumentAbuseDetector()]
    )
    first = runner.run(tenant_id=TENANT, now=NOW)
    _merged_tree_required(first.alerts_created == 1, "S4 alert creation")
    alert_id = f"document_abuse:{TENANT}:none:xtenant:{SHA}"
    assert alert_id in w67_graph.alerts, (
        f"pinned alert_id {alert_id!r}; got {sorted(w67_graph.alerts)}"
    )
    alert = w67_graph.alerts[alert_id]
    assert alert["tenant_id"] == TENANT and alert["type"] == "document_abuse"
    assert alert["severity"] == "high" and alert["person_id"] is None

    second = runner.run(tenant_id=TENANT, now=NOW + timedelta(minutes=15))
    assert second.alerts_created == 0 and second.alerts_deduped >= 1, (
        "event redelivery / re-sweep => MERGE dedup: one alert per (tenant, hash)"
    )
    xtenant_alerts = [
        a for a in w67_graph.alerts.values() if ":xtenant:" in a["alert_id"]
    ]
    assert len(xtenant_alerts) == 1


def test_s4_never_auto_quarantines(w67_client, w67_graph, settings, publisher):
    assert "document_abuse" not in AUTO_QUARANTINE_TYPES
    _add_doc(w67_graph, TENANT, "doc-later", sha256=SHA, seen_elsewhere_count=4)
    report = DetectionRunner(
        w67_client, publisher, settings, detectors=[DocumentAbuseDetector()]
    ).run(tenant_id=TENANT, now=NOW)
    _merged_tree_required(report.alerts_created == 1, "S4 alert creation")
    assert report.quarantined == []


def test_s4_earlier_tenant_untouched(w67_client, w67_graph, settings, publisher):
    """The earlier tenant's Document node is unchanged and it receives NO
    alert (first-use-wins: earlier tenants never learn anything)."""
    _add_doc(w67_graph, OTHER_TENANT, "doc-earlier", sha256=SHA)
    _add_doc(w67_graph, TENANT, "doc-later", sha256=SHA, seen_elsewhere_count=2)
    before = {
        k: dict(n["props"])
        for k, n in w67_graph.nodes.items()
        if "Document" in n["labels"] and n["props"].get("tenant_id") == OTHER_TENANT
    }
    runner = DetectionRunner(
        w67_client, publisher, settings, detectors=[DocumentAbuseDetector()]
    )
    report = runner.run(tenant_id=TENANT, now=NOW)
    _merged_tree_required(report.alerts_created == 1, "S4 alert for the later tenant")
    # Earlier tenant's node props unchanged by the later tenant's sweep.
    after = {
        k: dict(n["props"])
        for k, n in w67_graph.nodes.items()
        if "Document" in n["labels"] and n["props"].get("tenant_id") == OTHER_TENANT
    }
    assert before == after, "D9 must never write to (:Document) nodes"
    # No alert stamped with the earlier tenant; nothing references it.
    assert all(a["tenant_id"] == TENANT for a in w67_graph.alerts.values())
    for a in w67_graph.alerts.values():
        blob = json.dumps(a, default=str)
        assert OTHER_TENANT not in blob and "doc-earlier" not in blob
    # The earlier tenant's own sweep produces NO S4 alert (it has no
    # seen_elsewhere_count projection — it was the first user).
    report2 = runner.run(tenant_id=OTHER_TENANT, now=NOW)
    assert report2.alerts_created == 0
    assert all(a["tenant_id"] == TENANT for a in w67_graph.alerts.values())


# ---------------------------------------------------------------------------
# 4) Source gates: D9 Cypher projection + tenant binding
# ---------------------------------------------------------------------------
def test_d9_cypher_projects_seen_elsewhere_count_and_stays_tenant_bound():
    assert "$tenant_id" in DOCUMENTS_CYPHER, "assert_tenant_bound stands (G4)"
    _merged_tree_required(
        "seen_elsewhere_count" in DOCUMENTS_CYPHER,
        "d9_documents Cypher projection of seen_elsewhere_count",
    )


# ---------------------------------------------------------------------------
# 5) graph-sync consumer contract (payload allowlist, own-tenant projection)
# ---------------------------------------------------------------------------
def test_graphsync_declares_match_event_with_pinned_payload_struct():
    _merged_tree_required(GS_EVENTS.exists(), "graph-sync cloudevents.go")
    src = GS_EVENTS.read_text()
    _merged_tree_required(
        MATCH_EVENT_TYPE in src,
        f"graph-sync decode of {MATCH_EVENT_TYPE}",
    )
    structs = re.findall(r"type\s+(\w+)\s+struct\s*\{(.*?)\n\}", src, re.DOTALL)
    candidates = [(n, b) for n, b in structs if '"tenant_count"' in b]
    assert len(candidates) == 1, (
        f"expected exactly one match-payload struct with tenant_count, got "
        f"{[n for n, _ in candidates]}"
    )
    name, body = candidates[0]
    tags = set(re.findall(r'json:"(\w+)[,"]', body))
    assert tags == MATCH_PAYLOAD_JSON_TAGS, (
        f"{name} json tags {sorted(tags)} != pinned payload allowlist "
        f"{sorted(MATCH_PAYLOAD_JSON_TAGS)}"
    )
    assert "phash" not in body.lower(), (
        "the cross-tenant match payload must never declare phash (G2)"
    )


def _go_func_block(src: str, name: str) -> str | None:
    """Extract the full source block of Go `func name(...)` (with or without
    a receiver) up to its closing brace at column 0. Returns None if the
    function is absent (pristine tree). Precise extraction — doc comments of
    NEIGHBORING functions are never included, unlike naive `\\nfunc `
    chunking."""
    m = re.search(rf"^func\s+(?:\([^)]*\)\s*)?{re.escape(name)}\(", src, re.MULTILINE)
    if not m:
        return None
    end = re.search(r"^\}", src[m.start():], re.MULTILINE)
    assert end, f"func {name} body has no closing brace at column 0"
    return src[m.start():m.start() + end.end()]


def _go_backtick_strings(block: str) -> list[str]:
    """Backtick (raw) string literals inside a Go source block."""
    return re.findall(r"`([^`]*)`", block)


def _go_call_args(src: str, name: str) -> list[str] | None:
    """Paren-balanced argument list of the first `name(...)` call in src
    (top-level comma split, so nested calls like TrimSpace(...) stay whole).
    Returns None if the call is absent."""
    idx = src.find(name + "(")
    if idx < 0:
        return None
    start = idx + len(name)
    depth = 0
    end = -1
    for i in range(start, len(src)):
        if src[i] == "(":
            depth += 1
        elif src[i] == ")":
            depth -= 1
            if depth == 0:
                end = i
                break
    assert end > start, f"unbalanced parens in {name}(...) call"
    inner = src[start + 1:end]
    args, buf, d = [], [], 0
    for ch in inner:
        if ch == "(":
            d += 1
        elif ch == ")":
            d -= 1
        if ch == "," and d == 0:
            args.append("".join(buf).strip())
            buf = []
        else:
            buf.append(ch)
    if "".join(buf).strip():
        args.append("".join(buf).strip())
    return args


def test_graphsync_projection_scoped_to_later_tenants_own_node():
    """The seen_elsewhere_count SET must be bound to the event's OWN
    {tenant_id, document_id} Document node — never another tenant's."""
    _merged_tree_required(GS_QUERIES.exists(), "graph-sync queries.go")
    src = GS_QUERIES.read_text()
    _merged_tree_required(
        "seen_elsewhere_count" in src, "seen_elsewhere_count projection query"
    )
    # PRECISION FIX (W67 verifier false positive): the previous version
    # chunked the file on "\nfunc " and asserted per chunk. The DOC COMMENT
    # of setDocumentSeenElsewhereQuery mentions seen_elsewhere_count and so
    # landed inside the chunk of the PRECEDING upsertDocumentQuery, whose
    # body legitimately contains "doc.phash = $phash" (the pre-existing
    # WITHIN-tenant upsert) -> false positive. Per lead ruling (option a),
    # extract the EXACT func block of the W67 projection and assert on its
    # backtick Cypher literal + params ONLY — the assertion itself is NOT
    # relaxed.
    block = _go_func_block(src, "setDocumentSeenElsewhereQuery")
    _merged_tree_required(
        block is not None, "func setDocumentSeenElsewhereQuery (W67 projection)"
    )
    texts = _go_backtick_strings(block)
    assert len(texts) == 1, (
        "setDocumentSeenElsewhereQuery must build exactly one Cypher text "
        f"literal, got {len(texts)}"
    )
    query = texts[0]
    assert "seen_elsewhere_count" in query, (
        "the W67 projection must SET seen_elsewhere_count"
    )
    assert "$tenant_id" in query and "$document_id" in query, (
        "seen_elsewhere_count SET must bind BOTH $tenant_id and "
        "$document_id (the later tenant's OWN node — MERGE idempotent)"
    )
    assert re.search(
        r"MERGE \(doc:Document \{tenant_id: \$tenant_id, document_id: \$document_id\}\)",
        query,
    ), "projection MERGE key must be exactly (tenant_id, document_id)"
    assert "phash" not in query.lower(), "no phash in the projection (G2)"
    assert "$tenant_count" in query, (
        "tenant_count is the ONLY cross-tenant-derived datum the projection may write"
    )
    assert re.search(r"\$tenant_count > doc\.seen_elsewhere_count", query), (
        "MAX semantics: a stale LOWER tenant_count must never regress the property"
    )
    # The projection takes exactly (tenant_id, document_id, tenant_count) —
    # no hash, no other-tenant identifier can even be passed.
    sig = re.search(
        r"func setDocumentSeenElsewhereQuery\(([^)]*)\)", block
    ).group(1)
    assert "tenantID" in sig and "documentID" in sig and "tenantCount" in sig
    assert "phash" not in sig.lower() and "sha256" not in sig.lower(), (
        "no hash material may enter the cross-tenant projection signature (G2)"
    )

    # Separate guard (kept per lead ruling): the ONLY phash-bearing Document
    # query remains the PRE-EXISTING within-tenant upsertDocumentQuery — the
    # test still proves no NEW phash usage appeared in the W67 projection.
    upsert_block = _go_func_block(src, "upsertDocumentQuery")
    assert upsert_block is not None, "pre-existing within-tenant document upsert not found"
    upsert_texts = _go_backtick_strings(upsert_block)
    assert len(upsert_texts) == 1
    upsert_query = upsert_texts[0]
    assert "doc.phash = $phash" in upsert_query and "$tenant_id" in upsert_query, (
        "phash stays confined to the PRE-EXISTING tenant-bound "
        "upsertDocumentQuery (DocumentExtracted projection)"
    )
    assert "seen_elsewhere_count" not in upsert_query, (
        "the within-tenant upsert must never write the W67 cross-tenant property"
    )

    # The syncer must route the event type through the dead-letter-tolerant
    # consumer path (unknown/malformed => ack/DLQ posture, never crash).
    _merged_tree_required(GS_SYNCER.exists(), "graph-sync syncer.go")
    syncer = GS_SYNCER.read_text()
    assert "CrossTenantDocumentMatch" in syncer, (
        "syncer must consume the match event type"
    )
    # Consumer call site: the projection is invoked with ONLY
    # tenant_id / document_id / tenant_count (zero-disclosure end to end).
    args = _go_call_args(syncer, "s.Graph.SetDocumentSeenElsewhere")
    assert args is not None, "SetDocumentSeenElsewhere call site not found in syncer.go"
    assert args[0] == "ctx", "first arg is the context"
    call_args = args[1:]
    assert len(call_args) == 3, (
        "projection call must pass exactly tenant_id/document_id/tenant_count"
        f" (plus ctx), got {args}"
    )
    assert call_args[0] == "tenantID"
    assert "DocumentID" in call_args[1], "second arg is the event's OWN document_id"
    assert "TenantCount" in call_args[2], "third arg is the tenant_count integer"
    handler = _go_func_block(syncer, "crossTenantMatch")
    assert handler is not None, "crossTenantMatch handler not found"
    assert "phash" not in handler.lower() and "sha256" not in handler.lower(), (
        "the consumer handler must never touch hash material for the "
        "cross-tenant projection (zero-disclosure, G2)"
    )


# ---------------------------------------------------------------------------
# 6) G4 location ruling: fraud-engine must NOT get a Postgres DSN
# ---------------------------------------------------------------------------
def test_fraud_engine_never_touches_postgres_or_registry_guc():
    banned = ("async" + "pg", "psyco" + "pg", "app.ocr" + "_registry", "OCR_REGISTRY" + "_HMAC_SECRET")
    offenders = []
    for path in FRAUD_ROOT.rglob("*.py"):
        if "__pycache__" in path.parts:
            continue
        if path.name.startswith("test_w67_"):
            continue  # this battery names the tokens it bans
        text = path.read_text(errors="ignore")
        for token in banned:
            if token in text:
                offenders.append((path.relative_to(FRAUD_ROOT).as_posix(), token))
    assert not offenders, (
        "G4: fraud-engine is graph-query-only — it must never gain a "
        f"Postgres/registry dependency: {offenders}"
    )
