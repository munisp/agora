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
