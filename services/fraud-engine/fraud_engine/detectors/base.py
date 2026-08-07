"""Detector framework (SPEC-W30 §3).

Each detector = parameterized Cypher (``$tenant_id`` always bound) +
pure-Python analysis (severity rule) + dedup via
``alert_id = type:tenant:person:dedup_key`` (MERGE in alerts.py).

Detection != punishment: detectors only produce ``Finding`` objects. Alert
nodes, quarantine and CloudEvents are applied by ``DetectionRunner``.
"""

from __future__ import annotations

import dataclasses
import logging
import uuid
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from typing import Any, Protocol, Sequence

from ..alerts import AlertRecord, upsert_alert
from ..config import Settings
from ..events import EventPublisher, alert_raised_event
from ..graph import GraphClient
from ..quarantine import apply_quarantine

SEVERITIES = ("low", "medium", "high")

log = logging.getLogger("fraud_engine.detectors")


# ---------------------------------------------------------------------------
# Timestamp helpers — graph-sync stores ISO-8601 strings; be liberal in what
# we accept (epoch seconds or ISO) so detectors work across producers.
# ---------------------------------------------------------------------------
def parse_ts(value: Any) -> datetime | None:
    if value is None:
        return None
    if isinstance(value, datetime):
        return value if value.tzinfo else value.replace(tzinfo=UTC)
    if isinstance(value, (int, float)):
        return datetime.fromtimestamp(value, tz=UTC)
    s = str(value).strip()
    if not s:
        return None
    try:
        if s.replace(".", "").replace("-", "").replace(":", "").isdigit():
            return datetime.fromtimestamp(float(s), tz=UTC)
    except (ValueError, OverflowError, OSError):
        pass
    try:
        dt = datetime.fromisoformat(s.replace("Z", "+00:00"))
        return dt if dt.tzinfo else dt.replace(tzinfo=UTC)
    except ValueError:
        return None


def iso(dt: datetime) -> str:
    return dt.astimezone(UTC).isoformat()


def hours_ago(now: datetime, hours: float) -> str:
    return iso(now - timedelta(hours=hours))


def days_ago(now: datetime, days: float) -> str:
    return iso(now - timedelta(days=days))


# ---------------------------------------------------------------------------
# Findings
# ---------------------------------------------------------------------------
@dataclass(frozen=True)
class Finding:
    """One tripped detector match. ``evidence`` must be replayable
    (SPEC-W30 §5 gate 3): node ids, cycle paths, distances, timestamps."""

    type: str  # alert type, e.g. "referral_cycle"
    severity: str  # low|medium|high
    dedup_key: str  # stable across re-runs for the same underlying pattern
    person_id: str | None = None
    agent_id: str | None = None  # staff id string (staff are not graph nodes)
    evidence: dict[str, Any] = field(default_factory=dict)

    def subject(self) -> str:
        return self.person_id or (f"agent:{self.agent_id}" if self.agent_id else "none")

    def alert_id(self, tenant_id: str) -> str:
        # SPEC-W30 §3: alert_id = type:tenant:person:dedup_key
        return f"{self.type}:{tenant_id}:{self.subject()}:{self.dedup_key}"


def assert_tenant_bound(cypher: str, params: dict[str, Any]) -> None:
    """SPEC-W30 §5 gate 1: refuse to emit any statement that does not bind
    $tenant_id. Called by ``Detector._run`` for EVERY query/write."""
    if "tenant_id" not in params or not params["tenant_id"]:
        raise ValueError("tenant_id must be bound on every fraud-engine Cypher statement")
    if "$tenant_id" not in cypher:
        raise ValueError("Cypher statement must reference $tenant_id: " + cypher[:120])


class Detector:
    """Base class. Subclasses define CYPHER (+ markers) and ``analyze``."""

    name: str = "base"
    alert_type: str = "base"

    def cypher(self, settings: Settings) -> str:  # pragma: no cover - abstract
        raise NotImplementedError

    def params(self, tenant_id: str, settings: Settings, now: datetime) -> dict[str, Any]:
        return {"tenant_id": tenant_id}

    def analyze(
        self, rows: list[dict[str, Any]], settings: Settings, now: datetime
    ) -> list[Finding]:  # pragma: no cover - abstract
        raise NotImplementedError

    def _run(self, client: GraphClient, cypher: str, params: dict[str, Any]) -> list[dict[str, Any]]:
        assert_tenant_bound(cypher, params)
        return client.query(cypher, params)

    def detect(
        self, client: GraphClient, tenant_id: str, settings: Settings, now: datetime
    ) -> list[Finding]:
        rows = self._run(client, self.cypher(settings), self.params(tenant_id, settings, now))
        return self.analyze(rows, settings, now)


# ---------------------------------------------------------------------------
# Runner — orchestrates detectors + alerts + quarantine + events.
# ---------------------------------------------------------------------------
TENANTS_CYPHER = "// sweep:tenants\nMATCH (t:Tenant) RETURN t.tenant_id AS tenant_id"


@dataclass
class RunReport:
    run_id: str
    started_at: str
    tenants: list[str] = field(default_factory=list)
    detectors: list[str] = field(default_factory=list)
    findings: int = 0
    alerts_created: int = 0
    alerts_deduped: int = 0
    quarantined: list[str] = field(default_factory=list)
    events_published: int = 0
    errors: list[str] = field(default_factory=list)

    def as_dict(self) -> dict[str, Any]:
        return {
            "run_id": self.run_id,
            "started_at": self.started_at,
            "tenants": self.tenants,
            "detectors": self.detectors,
            "findings": self.findings,
            "alerts_created": self.alerts_created,
            "alerts_deduped": self.alerts_deduped,
            "quarantined": self.quarantined,
            "events_published": self.events_published,
            "errors": self.errors,
        }


class DetectorRegistry(Protocol):
    def __iter__(self) -> Any: ...


# ---------------------------------------------------------------------------
# W33-B learned scorer projection (SPEC-W33 §3 B1 wiring).
#
# Optional, additive: when settings.ml_registry_dir is set and usable weights
# exist, each finding's person is scored by fraud-ae + fraud-clf on an fv1
# vector built from this tenant-scoped activity projection (captures with
# geo, bookings/cancellations, REFERRED degree — the subset of the A1 event
# schema the production graph actually carries; amount-bearing dims are 0 at
# runtime, documented in ml/features.py). The ML blend NEVER weakens a rule
# verdict (I1 UNION): it only appends an "ml_blend ae=<x> clf=<y>" reason to
# the alert evidence and may raise severity low -> medium; the high band —
# and therefore auto-quarantine — remains rule-only in W33-B.
# ---------------------------------------------------------------------------
ML_ACTIVITY_CYPHER = """
// detector:ml_person_activity
MATCH (p:Person {tenant_id:$tenant_id})
WHERE p.person_id IN $person_ids
OPTIONAL MATCH (p)-[:HAS_CONTACT]->(c:Contact)
OPTIONAL MATCH (c)-[:CAPTURED_AT]->(l:Location)
OPTIONAL MATCH (p)-[:BOOKED]->(b:Booking)
OPTIONAL MATCH (p)-[r:REFERRED]-(:Person)
RETURN p.person_id AS person_id,
       collect(DISTINCT {ts: c.captured_at, lat: l.lat, lon: l.lon,
                         reference_id: c.lead_id}) AS captures,
       collect(DISTINCT {created_at: b.created_at, status: b.status,
                         cancelled_at: b.cancelled_at,
                         reference_id: b.booking_id}) AS bookings,
       count(DISTINCT r) AS referral_degree
"""


def activity_row_to_events(
    row: dict[str, Any],
) -> tuple[list[dict[str, Any]], int]:
    """Project one ml_person_activity row into A1-shaped event dicts +
    referral degree for ``ml.features.build_feature_vector``."""
    events: list[dict[str, Any]] = []
    for cap in row.get("captures") or []:
        if not cap.get("ts"):
            continue
        events.append(
            {
                "event_type": "capture",
                "ts": cap.get("ts"),
                "amount_ngn": 0.0,
                "lat": cap.get("lat"),
                "lon": cap.get("lon"),
                "reference_id": cap.get("reference_id"),
                "counterparty": None,
            }
        )
    for bk in row.get("bookings") or []:
        if not bk.get("created_at"):
            continue
        events.append(
            {
                "event_type": "booking",
                "ts": bk.get("created_at"),
                "amount_ngn": 0.0,
                "lat": None,
                "lon": None,
                "reference_id": bk.get("reference_id"),
                "counterparty": None,
            }
        )
        if bk.get("status") == "cancelled" and bk.get("cancelled_at"):
            events.append(
                {
                    "event_type": "cancellation",
                    "ts": bk.get("cancelled_at"),
                    "amount_ngn": 0.0,
                    "lat": None,
                    "lon": None,
                    "reference_id": bk.get("reference_id"),
                    "counterparty": None,
                }
            )
    return events, int(row.get("referral_degree") or 0)


class DetectionRunner:
    def __init__(
        self,
        client: GraphClient,
        publisher: EventPublisher,
        settings: Settings,
        detectors: Sequence[Detector] | None = None,
        ml_scorer: Any | None = None,
    ) -> None:
        self.client = client
        self.publisher = publisher
        self.settings = settings
        self.detectors = list(detectors) if detectors is not None else list(ALL_DETECTORS)
        # W33-B: explicit injection (tests) wins; otherwise lazy per-tenant
        # load from settings.ml_registry_dir. None/absent => pure rules (I1).
        self._ml_scorer = ml_scorer
        self._ml_scorer_by_tenant: dict[str, Any] = {}
        self._ml_activity_by_tenant: dict[str, dict[str, tuple[list[dict[str, Any]], int]]] = {}

    # -- W33-B learned scorer (additive; inert when disabled) ---------------
    def _ml_scorer_for(self, tenant_id: str) -> Any | None:
        if self._ml_scorer is not None:
            return self._ml_scorer
        if tenant_id in self._ml_scorer_by_tenant:
            return self._ml_scorer_by_tenant[tenant_id]
        scorer = None
        registry_dir = getattr(self.settings, "ml_registry_dir", "") or ""
        if registry_dir:
            try:
                from ..ml.scorer import LearnedScorer

                scorer = LearnedScorer.load(registry_dir, tenant_id)
            except Exception:  # noqa: BLE001 - model failure -> rule fallback
                log.exception("ml scorer load failed for %s; pure rules apply", tenant_id)
                scorer = None
        self._ml_scorer_by_tenant[tenant_id] = scorer
        return scorer

    def _ml_activity(
        self, tenant_id: str, person_ids: list[str]
    ) -> dict[str, tuple[list[dict[str, Any]], int]]:
        if tenant_id in self._ml_activity_by_tenant:
            return self._ml_activity_by_tenant[tenant_id]
        params = {"tenant_id": tenant_id, "person_ids": person_ids}
        rows = self._run_ml_query(tenant_id, params)
        activity = {
            str(row.get("person_id")): activity_row_to_events(row)
            for row in rows
            if row.get("person_id")
        }
        self._ml_activity_by_tenant[tenant_id] = activity
        return activity

    def _run_ml_query(self, tenant_id: str, params: dict[str, Any]) -> list[dict[str, Any]]:
        assert_tenant_bound(ML_ACTIVITY_CYPHER, params)
        return self.client.query(ML_ACTIVITY_CYPHER, params)

    def _ml_enrich(
        self, tenant_id: str, findings: list[Finding], report: "RunReport"
    ) -> list[Finding]:
        """Score each finding's person with the learned scorer and stamp
        ``ml_blend ae=<x> clf=<y>`` (+ provenance) into the evidence.
        Severity may only move UP (low -> medium; high stays rule-only).
        Any failure leaves the findings untouched (I1)."""
        scorer = self._ml_scorer_for(tenant_id)
        if scorer is None:
            return findings
        person_ids = sorted({f.person_id for f in findings if f.person_id})
        if not person_ids:
            return findings
        try:
            activity = self._ml_activity(tenant_id, person_ids)
            out: list[Finding] = []
            for finding in findings:
                if not finding.person_id:
                    out.append(finding)
                    continue
                events, degree = activity.get(finding.person_id, ([], 0))
                try:
                    result = scorer.score_events(events, degree)
                except Exception as exc:  # noqa: BLE001 - SPEC-W34 GF9
                    # Non-finite / failed ML score for ONE person must not
                    # suppress the upgrade path for the others: this finding
                    # falls back to the pure rule verdict (I1), the rest of
                    # the batch still scores.
                    log.warning(
                        "ml scoring failed for %s/%s: %s (rule fallback)",
                        tenant_id, finding.person_id, exc,
                    )
                    report.errors.append(
                        f"{tenant_id}/ml_blend: {type(exc).__name__}: {exc}"
                    )
                    out.append(finding)
                    continue
                evidence = dict(finding.evidence)
                evidence["ml_blend"] = scorer.blend_reason(result)
                evidence["ml_score"] = round(result.score, 6)
                evidence["ml_model_version"] = result.model_version
                evidence["ml_feature_schema"] = result.feature_schema
                severity = finding.severity
                if (
                    result.score >= self.settings.ml_score_threshold
                    and severity == "low"
                ):
                    severity = "medium"
                    evidence["severity_rule"] = (
                        f"ml_blend score >= {self.settings.ml_score_threshold} "
                        "raised low -> medium (UNION: high band and "
                        "auto-quarantine stay rule-only)"
                    )
                out.append(
                    dataclasses.replace(finding, severity=severity, evidence=evidence)
                )
            return out
        except Exception as exc:  # noqa: BLE001 - model failure -> pure rules
            log.exception("ml enrichment failed for %s; findings unchanged", tenant_id)
            report.errors.append(f"{tenant_id}/ml_blend: {type(exc).__name__}: {exc}")
            return findings

    def detector_named(self, name: str) -> Detector | None:
        for det in self.detectors:
            if det.name == name or det.alert_type == name:
                return det
        return None

    def _tenants(self, tenant_id: str | None) -> list[str]:
        if tenant_id:
            return [tenant_id]
        rows = self.client.query(TENANTS_CYPHER, {})
        return sorted({r["tenant_id"] for r in rows if r.get("tenant_id")})

    def run(
        self,
        tenant_id: str | None = None,
        detector: str | None = None,
        now: datetime | None = None,
    ) -> RunReport:
        now = now or datetime.now(UTC)
        report = RunReport(
            run_id=str(uuid.uuid4()),
            started_at=iso(now),
        )
        dets = self.detectors
        if detector:
            det = self.detector_named(detector)
            if det is None:
                raise KeyError(f"unknown detector: {detector}")
            dets = [det]
        report.detectors = [d.name for d in dets]
        report.tenants = self._tenants(tenant_id)

        for tenant in report.tenants:
            for det in dets:
                try:
                    findings = det.detect(self.client, tenant, self.settings, now)
                except Exception as exc:  # noqa: BLE001 — one detector must not sink a sweep
                    report.errors.append(f"{tenant}/{det.name}: {type(exc).__name__}: {exc}")
                    continue
                # W33-B (additive): optional learned-scorer blend. No-op
                # unless settings.ml_registry_dir resolves to usable weights.
                findings = self._ml_enrich(tenant, findings, report)
                report.findings += len(findings)
                created_records: list[AlertRecord] = []
                for finding in findings:
                    record, created = upsert_alert(self.client, tenant, finding, now)
                    if created:
                        report.alerts_created += 1
                        created_records.append(record)
                    else:
                        report.alerts_deduped += 1
                # CloudEvents only for NEWLY created alerts (idempotent re-runs
                # must not re-spam the alerts topic).
                for record in created_records:
                    event = alert_raised_event(tenant, record)
                    self.publisher.publish(self.settings.alerts_topic, record.alert_id, event)
                    report.events_published += 1
                # Quarantine per SPEC-W30 §3: only F1/F2/F3-high, applied
                # idempotently on every run (even for deduped alerts).
                newly_quarantined = apply_quarantine(self.client, tenant, findings, now)
                for pid in newly_quarantined:
                    if pid not in report.quarantined:
                        report.quarantined.append(pid)
        return report


# Concrete detectors (imported last to avoid a circular import through
# alerts/events/quarantine).
from .d1_referral import ReferralCycleDetector  # noqa: E402
from .d2_sybil import SybilClusterDetector  # noqa: E402
from .d3_velocity import CaptureVelocityDetector  # noqa: E402
from .d4_geo import GeoImpossibilityDetector  # noqa: E402
from .d5_consent import ConsentBackdatingDetector  # noqa: E402
from .d6_ghost import GhostBookingDetector  # noqa: E402
from .d7_anomaly import AnomalyDetector  # noqa: E402
from .d8_civic import ReportSpamDetector  # noqa: E402

ALL_DETECTORS: tuple[Detector, ...] = (
    ReferralCycleDetector(),
    SybilClusterDetector(),
    CaptureVelocityDetector(),
    GeoImpossibilityDetector(),
    ConsentBackdatingDetector(),
    GhostBookingDetector(),
    AnomalyDetector(),
    # SPEC-W32 §3 WS-D: report_spam is medium-only and NEVER auto-quarantines
    # (citizens are never banned from reporting) — see d8_civic/quarantine.
    ReportSpamDetector(),
)

DETECTORS_BY_NAME: dict[str, Detector] = {d.name: d for d in ALL_DETECTORS}
