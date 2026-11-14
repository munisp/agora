"""D9 document_abuse (SPEC-W55 Scope B).

S1 exact_duplicate: >= D9_DUP_MIN_COUNT (2) same-sha256 documents in the
    lookback window => medium; high ONLY at >= D9_DUP_HIGH_COUNT (3).
S2 near_duplicate:  phash hamming <= D9_PHASH_HAMMING_MAX (6), different
    sha256 => medium per pair.
S3 tamper_suspect:  phash hamming == 0 AND different sha256 => medium per
    pair (pinned downgrade from high — hash-only posture).
D9 NEVER auto-quarantines and every finding carries person_id=None.
"""

from __future__ import annotations

import json
from datetime import timedelta

from fraud_engine.detectors import ALL_DETECTORS, DetectionRunner
from fraud_engine.detectors.d9_document import DocumentAbuseDetector
from fraud_engine.quarantine import AUTO_QUARANTINE_TYPES

from conftest import NOW, OTHER_TENANT, TENANT, ts

SHA_A = "a" * 64
SHA_B = "b" * 64
SHA_C = "c" * 64

# 64-bit dHash phashes (16-char hex). Distance from PHASH_BASE = popcount.
PHASH_BASE = "0000000000000000"
PHASH_DIST_6 = "000000000000003f"  # hamming 6 == D9_PHASH_HAMMING_MAX
PHASH_DIST_7 = "000000000000007f"  # hamming 7 == threshold + 1


def add_document(
    g,
    tenant,
    document_id,
    *,
    sha256=SHA_A,
    phash=None,  # phash absent unless a test wires a pair explicitly
    kind="receipt",
    status="extracted",
    minutes_ago=60,
):
    """Project one hash-only (:Document) stub (SPEC-W55 pinned field set)."""
    return g.add_document(
        tenant,
        document_id,
        sha256=sha256,
        phash=phash,
        kind=kind,
        status=status,
        created_at=ts(minutes_ago),
    )


def dup_fixture(g, tenant, count, digest=SHA_A, prefix="dup"):
    for i in range(count):
        add_document(g, tenant, f"{prefix}-{tenant}-{i}", sha256=digest)


# -- S1: exact sha256 duplicates ----------------------------------------------


def test_exact_duplicate_fires_high_at_three(client, graph, settings):
    dup_fixture(graph, TENANT, 3)
    findings = DocumentAbuseDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    f = findings[0]
    assert f.type == "document_abuse" and f.severity == "high"
    assert f.person_id is None, "cross-document finding — no single subject"
    assert f.dedup_key == f"sha256:{SHA_A}"
    ev = f.evidence
    assert ev["detector"] == "d9_document_abuse"
    assert ev["signal"] == "exact_duplicate"
    assert ev["count"] == 3 and ev["sha256"] == SHA_A and ev["kind"] == "receipt"
    assert ev["document_ids"] == sorted(ev["document_ids"])
    assert ev["threshold"] == {"dup_min_count": 2, "dup_high_count": 3}
    assert "severity_rule" in ev


def test_exact_duplicate_medium_at_min_threshold(client, graph, settings):
    dup_fixture(graph, TENANT, 2)  # count >= D9_DUP_MIN_COUNT, < DUP_HIGH
    findings = DocumentAbuseDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    f = findings[0]
    assert f.severity == "medium"
    assert f.evidence["signal"] == "exact_duplicate"
    assert f.evidence["count"] == 2


def test_exact_duplicate_silent_below_threshold(client, graph, settings):
    add_document(graph, TENANT, "solo-1", sha256=SHA_A)
    add_document(graph, TENANT, "solo-2", sha256=SHA_B)  # distinct digests
    assert DocumentAbuseDetector().detect(client, TENANT, settings, NOW) == []


def test_silent_when_documents_outside_lookback(client, graph, settings):
    for i in range(3):  # 3 same-sha docs but 40 days old (lookback is 30)
        add_document(
            graph, TENANT, f"old-{i}", sha256=SHA_A, minutes_ago=40 * 24 * 60
        )
    assert DocumentAbuseDetector().detect(client, TENANT, settings, NOW) == []


# -- S2: phash near-duplicates --------------------------------------------------


def test_near_duplicate_fires_at_hamming_threshold(client, graph, settings):
    add_document(graph, TENANT, "doc-a", sha256=SHA_A, phash=PHASH_BASE)
    add_document(graph, TENANT, "doc-b", sha256=SHA_B, phash=PHASH_DIST_6)
    findings = DocumentAbuseDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    f = findings[0]
    assert f.type == "document_abuse" and f.severity == "medium"
    assert f.person_id is None
    assert f.dedup_key == "phash:doc-a:doc-b"  # sorted document-id pair
    ev = f.evidence
    assert ev["signal"] == "near_duplicate"
    assert ev["hamming_distance"] == 6
    assert ev["document_ids"] == ["doc-a", "doc-b"]
    assert ev["phashes"] == [PHASH_BASE, PHASH_DIST_6]
    assert ev["threshold"] == {"phash_hamming_max": 6}
    assert "byte-grid" in ev["caveat"] and "mime" in ev["caveat"]
    assert "severity_rule" in ev


def test_near_duplicate_silent_above_threshold(client, graph, settings):
    add_document(graph, TENANT, "doc-a", sha256=SHA_A, phash=PHASH_BASE)
    add_document(graph, TENANT, "doc-b", sha256=SHA_B, phash=PHASH_DIST_7)
    assert DocumentAbuseDetector().detect(client, TENANT, settings, NOW) == []


def test_near_duplicate_requires_different_sha256(client, graph, settings):
    # Same digest pairs are S1's domain, not S2's (single finding total).
    dup_fixture(graph, TENANT, 2, digest=SHA_A)
    findings = DocumentAbuseDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1 and findings[0].evidence["signal"] == "exact_duplicate"


def test_invalid_phash_pair_skipped(client, graph, settings):
    add_document(graph, TENANT, "doc-a", sha256=SHA_A, phash="not-valid-hex!!")
    add_document(graph, TENANT, "doc-b", sha256=SHA_B, phash=PHASH_BASE)
    add_document(graph, TENANT, "doc-c", sha256=SHA_C, phash=None)
    assert DocumentAbuseDetector().detect(client, TENANT, settings, NOW) == []


# -- S3: tamper suspects (distance 0, different sha256) -------------------------


def test_tamper_suspect_distance_zero_medium(client, graph, settings):
    add_document(graph, TENANT, "doc-a", sha256=SHA_A, phash=PHASH_BASE)
    add_document(graph, TENANT, "doc-b", sha256=SHA_B, phash=PHASH_BASE)
    findings = DocumentAbuseDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    f = findings[0]
    assert f.severity == "medium"  # PINNED downgrade from high (SPEC-W55)
    assert f.dedup_key == "tamper:doc-a:doc-b"
    ev = f.evidence
    assert ev["signal"] == "tamper_suspect"
    assert ev["hamming_distance"] == 0
    assert ev["document_ids"] == ["doc-a", "doc-b"]
    # The severity rule states the downgrade rationale (hash-only posture).
    assert "downgrade" in ev["severity_rule"]
    assert "hash-only" in ev["severity_rule"]


# -- cross-cutting: tenancy, dedup, quarantine posture --------------------------


def test_tenant_isolation(client, graph, settings):
    dup_fixture(graph, OTHER_TENANT, 3)
    add_document(graph, OTHER_TENANT, "other-a", sha256=SHA_B, phash=PHASH_BASE)
    add_document(graph, OTHER_TENANT, "other-b", sha256=SHA_C, phash=PHASH_BASE)
    # OTHER_TENANT documents are invisible to TENANT's sweep.
    assert DocumentAbuseDetector().detect(client, TENANT, settings, NOW) == []
    # ...and the tripped tenant does fire (the fixture is real).
    assert len(DocumentAbuseDetector().detect(client, OTHER_TENANT, settings, NOW)) == 2


def test_runner_dedup_second_sweep_creates_no_new_alerts(
    client, graph, publisher, settings
):
    dup_fixture(graph, TENANT, 3)  # S1 high
    add_document(graph, TENANT, "doc-a", sha256=SHA_B, phash=PHASH_BASE)
    add_document(graph, TENANT, "doc-b", sha256=SHA_C, phash=PHASH_BASE)  # S3
    runner = DetectionRunner(
        client, publisher, settings, detectors=[DocumentAbuseDetector()]
    )
    first = runner.run(tenant_id=TENANT, now=NOW)
    assert first.alerts_created == 2 and first.alerts_deduped == 0
    second = runner.run(tenant_id=TENANT, now=NOW + timedelta(minutes=15))
    assert second.alerts_created == 0 and second.alerts_deduped == 2
    doc_alerts = [a for a in graph.alerts.values() if a["type"] == "document_abuse"]
    assert len(doc_alerts) == 2
    assert {a["severity"] for a in doc_alerts} == {"high", "medium"}
    assert all(a["tenant_id"] == TENANT for a in doc_alerts)
    assert all(a["person_id"] is None for a in doc_alerts)


def test_document_abuse_never_auto_quarantines(client, graph, publisher, settings):
    assert "document_abuse" not in AUTO_QUARANTINE_TYPES
    dup_fixture(graph, TENANT, 5)  # high-severity finding
    report = DetectionRunner(
        client, publisher, settings, detectors=[DocumentAbuseDetector()]
    ).run(tenant_id=TENANT, now=NOW)
    assert report.alerts_created == 1
    assert report.quarantined == []


def test_registered_in_detector_registry():
    names = [d.name for d in ALL_DETECTORS]
    assert "d9_document_abuse" in names
    det = next(d for d in ALL_DETECTORS if d.name == "d9_document_abuse")
    assert det.alert_type == "document_abuse"


def test_kafka_trigger_routes_ocr_events():
    from fraud_engine.main import detectors_for_event

    assert detectors_for_event("com.opendesk.ocr.DocumentExtracted") == [
        "d9_document_abuse"
    ]
    assert detectors_for_event("com.opendesk.booking.BookingCreated") == []


# -- S4: cross-tenant exact duplicate (SPEC-W67 G6) ----------------------------
#
# graph-sync projects the ocr registry's pinned CrossTenantDocumentMatch
# event onto the LATER tenant's OWN (:Document) stub as the additive
# seen_elsewhere_count (MAX semantics). D9's tenant-bound sweep turns
# count >= D9_XTENANT_MIN_COUNT (2) into ONE high finding per sha256 with
# zero-disclosure evidence (own document ids + count ONLY) and dedup_key
# xtenant:{sha256} => alert_id document_abuse:{tenant}:none:xtenant:{sha256}.

S4_EVIDENCE_KEYS = {"detector", "signal", "document_ids", "seen_elsewhere_count"}


def add_xtenant_document(g, tenant, document_id, count, *, sha256=SHA_A):
    """Project a stub the way graph-sync leaves it after a W67 match."""
    return g.add_document(
        tenant,
        document_id,
        sha256=sha256,
        kind="receipt",
        status="extracted",
        created_at=ts(60),
        seen_elsewhere_count=count,
    )


def test_cross_tenant_fires_high_at_threshold(client, graph, settings):
    add_xtenant_document(graph, TENANT, "own-1", 2)
    findings = DocumentAbuseDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    f = findings[0]
    assert f.type == "document_abuse" and f.severity == "high"
    assert f.person_id is None, "zero cross-tenant disclosure — no subject"
    assert f.dedup_key == f"xtenant:{SHA_A}"
    assert f.alert_id(TENANT) == f"document_abuse:{TENANT}:none:xtenant:{SHA_A}"
    ev = f.evidence
    assert set(ev) == S4_EVIDENCE_KEYS, "zero-disclosure evidence allowlist"
    assert ev["signal"] == "cross_tenant_exact_duplicate"
    assert ev["detector"] == "d9_document_abuse"
    assert ev["document_ids"] == ["own-1"]
    assert ev["seen_elsewhere_count"] == 2


def test_cross_tenant_groups_own_documents_per_sha256(client, graph, settings):
    add_xtenant_document(graph, TENANT, "own-1", 2)
    add_xtenant_document(graph, TENANT, "own-2", 4)  # same digest, higher count
    add_xtenant_document(graph, TENANT, "own-3", 3, sha256=SHA_B)
    findings = DocumentAbuseDetector().detect(client, TENANT, settings, NOW)
    s4 = [f for f in findings if f.evidence["signal"] == "cross_tenant_exact_duplicate"]
    assert len(s4) == 2, "one S4 finding per sha256"
    by_digest = {f.dedup_key: f for f in s4}
    fa = by_digest[f"xtenant:{SHA_A}"]
    assert fa.evidence["document_ids"] == ["own-1", "own-2"]
    assert fa.evidence["seen_elsewhere_count"] == 4, "max count across own stubs"
    assert by_digest[f"xtenant:{SHA_B}"].evidence["document_ids"] == ["own-3"]
    # S1 also fires on the two same-sha256 OWN documents — an orthogonal,
    # additional signal (same-tenant duplicate), not a conflict.
    assert any(f.evidence["signal"] == "exact_duplicate" for f in findings)


def test_cross_tenant_silent_below_threshold_and_when_absent(
    client, graph, settings
):
    add_xtenant_document(graph, TENANT, "own-1", 1)  # only ever seen locally
    add_document(graph, TENANT, "own-2", sha256=SHA_B)  # property absent entirely
    add_xtenant_document(graph, TENANT, "own-3", 0, sha256=SHA_C)
    assert DocumentAbuseDetector().detect(client, TENANT, settings, NOW) == []


def test_cross_tenant_evidence_discloses_nothing_cross_tenant(
    client, graph, settings
):
    add_xtenant_document(graph, TENANT, "own-1", 3)
    # Another tenant holding the same hash MUST be invisible to this sweep
    # and MUST NOT appear anywhere in the evidence.
    add_xtenant_document(graph, OTHER_TENANT, "other-9", 2)
    findings = DocumentAbuseDetector().detect(client, TENANT, settings, NOW)
    assert len(findings) == 1
    blob = json.dumps(findings[0].evidence)
    assert OTHER_TENANT not in blob and "other-9" not in blob
    assert findings[0].evidence["document_ids"] == ["own-1"]


def test_cross_tenant_tenant_isolation(client, graph, settings):
    add_xtenant_document(graph, OTHER_TENANT, "other-1", 5)
    # OTHER_TENANT's seen_elsewhere stubs are invisible to TENANT's sweep.
    assert DocumentAbuseDetector().detect(client, TENANT, settings, NOW) == []
    assert len(DocumentAbuseDetector().detect(client, OTHER_TENANT, settings, NOW)) == 1


def test_cross_tenant_runner_dedup_second_sweep(client, graph, publisher, settings):
    add_xtenant_document(graph, TENANT, "own-1", 3)
    runner = DetectionRunner(
        client, publisher, settings, detectors=[DocumentAbuseDetector()]
    )
    first = runner.run(tenant_id=TENANT, now=NOW)
    assert first.alerts_created == 1 and first.alerts_deduped == 0
    second = runner.run(tenant_id=TENANT, now=NOW + timedelta(minutes=15))
    assert second.alerts_created == 0 and second.alerts_deduped == 1
    alert = graph.alerts[f"document_abuse:{TENANT}:none:xtenant:{SHA_A}"]
    assert alert["type"] == "document_abuse" and alert["severity"] == "high"
    assert alert["tenant_id"] == TENANT and alert["person_id"] is None
    evidence = json.loads(alert["evidence"])
    assert set(evidence) == S4_EVIDENCE_KEYS
    assert evidence["signal"] == "cross_tenant_exact_duplicate"


def test_cross_tenant_refresh_only_while_open(client, graph, publisher, settings):
    add_xtenant_document(graph, TENANT, "own-1", 3)
    runner = DetectionRunner(
        client, publisher, settings, detectors=[DocumentAbuseDetector()]
    )
    runner.run(tenant_id=TENANT, now=NOW)
    alert_id = f"document_abuse:{TENANT}:none:xtenant:{SHA_A}"
    # Operator resolves the alert; a re-sweep must NOT reopen/refresh it
    # (alerts.py ON MATCH refreshes evidence only while status = open).
    graph.alerts[alert_id]["status"] = "dismissed"
    stale_evidence = graph.alerts[alert_id]["evidence"]
    add_xtenant_document(graph, TENANT, "own-2", 5)  # count grew
    report = runner.run(tenant_id=TENANT, now=NOW + timedelta(minutes=15))
    assert report.alerts_created == 1, "only the new orthogonal S1 alert is created"
    assert report.alerts_deduped == 1, "the S4 alert is deduped, not refreshed"
    assert graph.alerts[alert_id]["evidence"] == stale_evidence
    assert graph.alerts[alert_id]["status"] == "dismissed"


def test_cross_tenant_never_auto_quarantines(client, graph, publisher, settings):
    assert "document_abuse" not in AUTO_QUARANTINE_TYPES
    add_xtenant_document(graph, TENANT, "own-1", 7)  # high-severity S4
    report = DetectionRunner(
        client, publisher, settings, detectors=[DocumentAbuseDetector()]
    ).run(tenant_id=TENANT, now=NOW)
    assert report.alerts_created == 1
    assert report.quarantined == []


def test_kafka_trigger_routes_cross_tenant_match():
    from fraud_engine.main import detectors_for_event

    assert detectors_for_event("com.opendesk.ocr.CrossTenantDocumentMatch") == [
        "d9_document_abuse"
    ]

