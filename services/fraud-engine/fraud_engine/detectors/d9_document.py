"""D9 document_abuse (SPEC-W55 Scope B).

Same-tenant duplicate / near-duplicate / tamper-suspect detection over the
hash-only ``(:Document)`` stub nodes graph-sync projects from
``com.opendesk.ocr.DocumentExtracted`` on ``opendesk.ocr.events`` (Scope A;
SPEC-W55 pinned field set: ``tenant_id, document_id, kind, status, sha256,
phash, page_count, model_version, created_at`` — NEVER document text,
extracted field payloads, or storage locations; PII minimization).

ONE ``// detector:d9_documents`` Cypher (``$tenant_id``-bound) returns the
tenant's Document rows in the ``D9_LOOKBACK_DAYS`` window; ALL analysis is
pure Python (D8 analyze-in-Python precedent):

  S1 exact_duplicate — group by sha256; count >= D9_DUP_MIN_COUNT (2) =>
      medium, escalated to high ONLY at count >= D9_DUP_HIGH_COUNT (3)
      (count-based only — person linkage is a SPEC-W55 NO-GO).
  S2 near_duplicate  — per-pair, different sha256, both phashes valid
      16-char hex, hamming <= D9_PHASH_HAMMING_MAX (6) => medium.
      Caveat: for non-PNG inputs the phash is a byte-grid fallback over raw
      bytes (ocr validate.py:179-194), not perceptual, and the W53 event
      carries no ``mime`` field, so ONE conservative threshold is used.
  S3 tamper_suspect  — per-pair, hamming == 0 AND different sha256 (same
      pixels/grid, re-encoded or edited bytes) => medium. PINNED downgrade
      from high: the high call was gated on extracted-field divergence,
      which the hash-only posture excludes; benign re-encode is possible.
  S4 cross_tenant_exact_duplicate (SPEC-W67 G6) — the tenant's OWN stub
      carries ``seen_elsewhere_count >= D9_XTENANT_MIN_COUNT (2)``: the
      platform's ocr registry saw this exact sha256 on that many DISTINCT
      tenants (graph-sync projects the pinned CrossTenantDocumentMatch
      event; count, never identity). Exact-byte reuse across tenants is the
      strongest document-fraud signal the platform can produce => HIGH.
      Evidence is ZERO-DISCLOSURE: the tenant's own document ids + the
      count ONLY — no other tenant id, no other document id, no phash
      anywhere (earlier tenants are never alerted, and this tenant never
      learns who the others are).

D9 NEVER auto-quarantines (``document_abuse`` stays OUT of
quarantine.AUTO_QUARANTINE_TYPES) and every finding carries
``person_id=None`` (D8 coordinated precedent) — operator triage only.
MERGE dedup on ``type:tenant:subject:dedup_key`` (alerts.py) makes re-sweeps
idempotent.
"""

from __future__ import annotations

from datetime import datetime
from typing import Any

from ..config import Settings
from ..graph import GraphClient
from .base import Detector, Finding, days_ago

DOCUMENTS_CYPHER = """
// detector:d9_documents
MATCH (d:Document) WHERE d.tenant_id = $tenant_id AND d.created_at >= $since
RETURN d.document_id AS document_id, d.sha256 AS sha256, d.phash AS phash,
       d.kind AS kind, d.status AS status, d.created_at AS created_at,
       d.seen_elsewhere_count AS seen_elsewhere_count
"""

# SPEC-W55 pinned decision 1: non-PNG phashes are a byte-grid fallback and
# the W53 DocumentExtracted payload carries no mime field, so S2 uses ONE
# conservative threshold with this caveat surfaced in evidence.
BYTE_GRID_CAVEAT = (
    "phash is a 64-bit dHash; for non-PNG inputs it is a byte-grid fallback "
    "over raw bytes (ocr-service validate.py:179-194), not a perceptual hash "
    "— the W53 DocumentExtracted event carries no mime field, so one "
    "conservative hamming threshold is applied to all documents"
)

# SPEC-W55 pinned decision 2: S3 is MEDIUM (downgrade from RECON §3.3's
# high). The high-severity tamper call was gated on extracted-field
# divergence, which the hash-only PII posture excludes.
TAMPER_SEVERITY_RULE = (
    "medium (pinned downgrade from high, SPEC-W55): phash-identical with "
    "different sha256 indicates re-encoded or edited bytes, but "
    "fields-divergence corroboration is unavailable under the hash-only "
    "posture (extracted field payloads never enter the graph) and a benign "
    "re-encode produces the same signal — human triage decides"
)


def phash_int(phash: Any) -> int | None:
    """Parse a 64-bit phash (16-char lowercase hex, ocr validate.py:228).
    Invalid/missing -> None (caller skips the pair)."""
    s = str(phash or "").strip()
    if len(s) != 16:
        return None
    try:
        return int(s, 16)
    except ValueError:
        return None


def hamming64(a: int, b: int) -> int:
    """Hamming distance between two 64-bit phash integers."""
    return bin(a ^ b).count("1")


class DocumentAbuseDetector(Detector):
    name = "d9_document_abuse"
    alert_type = "document_abuse"

    def cypher(self, settings: Settings) -> str:
        return DOCUMENTS_CYPHER

    def detect(
        self, client: GraphClient, tenant_id: str, settings: Settings, now: datetime
    ) -> list[Finding]:
        since = days_ago(now, settings.d9_lookback_days)
        rows = self._run(
            client, DOCUMENTS_CYPHER, {"tenant_id": tenant_id, "since": since}
        )
        return (
            self._exact_duplicate_findings(rows, settings)
            + self._near_duplicate_findings(rows, settings)
            + self._cross_tenant_findings(rows, settings)
        )

    # -- signal S1: exact sha256 duplicate groups ---------------------------
    def _exact_duplicate_findings(
        self, rows: list[dict[str, Any]], settings: Settings
    ) -> list[Finding]:
        by_digest: dict[str, list[dict[str, Any]]] = {}
        for row in rows:
            digest = str(row.get("sha256") or "").strip()
            doc_id = str(row.get("document_id") or "").strip()
            if not digest or not doc_id:
                continue
            by_digest.setdefault(digest, []).append(row)

        findings: list[Finding] = []
        for digest, group in sorted(by_digest.items()):
            doc_ids = sorted({str(r["document_id"]) for r in group})
            count = len(doc_ids)
            if count < settings.d9_dup_min_count:
                continue
            high = count >= settings.d9_dup_high_count
            kinds = sorted({str(r.get("kind") or "") for r in group if r.get("kind")})
            findings.append(
                Finding(
                    type=self.alert_type,
                    severity="high" if high else "medium",
                    dedup_key=f"sha256:{digest}",
                    person_id=None,  # cross-document finding — no single subject
                    evidence={
                        "detector": self.name,
                        "signal": "exact_duplicate",
                        "document_ids": doc_ids,
                        "sha256": digest,
                        "kind": kinds[0] if len(kinds) == 1 else kinds,
                        "count": count,
                        "threshold": {
                            "dup_min_count": settings.d9_dup_min_count,
                            "dup_high_count": settings.d9_dup_high_count,
                        },
                        "severity_rule": (
                            f"high: count {count} >= D9_DUP_HIGH_COUNT "
                            f"({settings.d9_dup_high_count})"
                            if high
                            else f"medium: count {count} >= D9_DUP_MIN_COUNT "
                            f"({settings.d9_dup_min_count}); escalates to high "
                            f"ONLY at count >= D9_DUP_HIGH_COUNT "
                            f"({settings.d9_dup_high_count})"
                        ),
                    },
                )
            )
        return findings

    # -- signals S2 (near-duplicate) + S3 (tamper-suspect): phash pairs -----
    def _near_duplicate_findings(
        self, rows: list[dict[str, Any]], settings: Settings
    ) -> list[Finding]:
        # Cross-digest pairs only: same-sha256 pairs are already S1.
        docs: dict[str, dict[str, Any]] = {}
        for row in rows:
            doc_id = str(row.get("document_id") or "").strip()
            digest = str(row.get("sha256") or "").strip()
            if not doc_id or not digest:
                continue
            docs[doc_id] = {
                "sha256": digest,
                "phash": phash_int(row.get("phash")),
                "phash_raw": row.get("phash"),
            }

        findings: list[Finding] = []
        skipped_invalid_phash = 0
        ids = sorted(docs)
        for i, a_id in enumerate(ids):
            for b_id in ids[i + 1 :]:
                a, b = docs[a_id], docs[b_id]
                if a["sha256"] == b["sha256"]:
                    continue
                if a["phash"] is None or b["phash"] is None:
                    skipped_invalid_phash += 1
                    continue
                distance = hamming64(a["phash"], b["phash"])
                if distance > settings.d9_phash_hamming_max:
                    continue
                pair_ids = sorted([a_id, b_id])
                phashes = [str(docs[d]["phash_raw"] or "") for d in pair_ids]
                sha256s = [docs[d]["sha256"] for d in pair_ids]
                if distance == 0:
                    # S3: same pixels/grid, different bytes (SPEC-W55 pinned
                    # MEDIUM downgrade — see module docstring).
                    findings.append(
                        Finding(
                            type=self.alert_type,
                            severity="medium",
                            dedup_key=f"tamper:{pair_ids[0]}:{pair_ids[1]}",
                            person_id=None,
                            evidence={
                                "detector": self.name,
                                "signal": "tamper_suspect",
                                "document_ids": pair_ids,
                                "phashes": phashes,
                                "sha256s": sha256s,
                                "hamming_distance": 0,
                                "skipped_invalid_phash_pairs": skipped_invalid_phash,
                                "severity_rule": TAMPER_SEVERITY_RULE,
                            },
                        )
                    )
                else:
                    # S2: near-duplicate (different bytes, close grids).
                    findings.append(
                        Finding(
                            type=self.alert_type,
                            severity="medium",  # ALWAYS medium (SPEC-W55)
                            dedup_key=f"phash:{pair_ids[0]}:{pair_ids[1]}",
                            person_id=None,
                            evidence={
                                "detector": self.name,
                                "signal": "near_duplicate",
                                "document_ids": pair_ids,
                                "phashes": phashes,
                                "sha256s": sha256s,
                                "hamming_distance": distance,
                                "threshold": {
                                    "phash_hamming_max": settings.d9_phash_hamming_max
                                },
                                "skipped_invalid_phash_pairs": skipped_invalid_phash,
                                "caveat": BYTE_GRID_CAVEAT,
                                "severity_rule": "medium (always): phash hamming "
                                "distance at/below D9_PHASH_HAMMING_MAX is a "
                                "triage signal only — D9 never escalates S2 and "
                                "never auto-quarantines",
                            },
                        )
                    )
        return findings

    # -- signal S4: cross-tenant exact duplicate (SPEC-W67 G6) ----------------
    def _cross_tenant_findings(
        self, rows: list[dict[str, Any]], settings: Settings
    ) -> list[Finding]:
        """One HIGH finding per sha256 whose OWN stubs carry
        ``seen_elsewhere_count >= D9_XTENANT_MIN_COUNT``. The count is the
        ONLY cross-tenant datum the graph holds (graph-sync MAX-semantics
        projection of the pinned registry match event), so the evidence is
        zero-disclosure by construction: this tenant's own document ids +
        the count. ``dedup_key = xtenant:{sha256}`` =>
        ``alert_id = document_abuse:{tenant}:none:xtenant:{sha256}`` — the
        alerts.py MERGE makes replays/re-sweeps free (ON MATCH refreshes
        only while open). person_id=None; D9 never auto-quarantines."""
        by_digest: dict[str, dict[str, Any]] = {}
        for row in rows:
            raw_count = row.get("seen_elsewhere_count")
            try:
                count = int(raw_count)
            except (TypeError, ValueError):
                continue  # property absent (never matched elsewhere)
            if count < settings.d9_xtenant_min_count:
                continue
            digest = str(row.get("sha256") or "").strip()
            doc_id = str(row.get("document_id") or "").strip()
            if not digest or not doc_id:
                continue
            entry = by_digest.setdefault(digest, {"document_ids": set(), "count": 0})
            entry["document_ids"].add(doc_id)
            entry["count"] = max(entry["count"], count)

        findings: list[Finding] = []
        for digest in sorted(by_digest):
            entry = by_digest[digest]
            findings.append(
                Finding(
                    type=self.alert_type,
                    severity="high",  # exact-byte reuse across tenants (SPEC-W67)
                    dedup_key=f"xtenant:{digest}",
                    person_id=None,  # zero cross-tenant disclosure — no subject
                    evidence={
                        "detector": self.name,
                        "signal": "cross_tenant_exact_duplicate",
                        # OWN document ids + the count ONLY — no other
                        # tenant's ids/documents/timestamps, no phash.
                        "document_ids": sorted(entry["document_ids"]),
                        "seen_elsewhere_count": entry["count"],
                    },
                )
            )
        return findings

    def analyze(self, rows, settings, now):  # pragma: no cover - detect() overrides
        raise NotImplementedError("DocumentAbuseDetector.detect handles analysis")
