"""Fake GraphClient for tests — an in-memory property graph plus
marker-dispatched evaluation of the exact Cyphers fraud-engine emits.

No live FalkorDB/Kafka anywhere in the suite (SPEC-W30 §4 WS-B test rule).

Dispatch key = the ``// detector:dX`` / ``// write:...`` comment marker every
statement in the package carries. The fake enforces the same tenant scoping
FalkorDB would get from the bound ``$tenant_id``: nodes of other tenants are
invisible, so tenant-isolation tests are real behavioral tests.
"""

from __future__ import annotations

import itertools
import re
from datetime import datetime
from typing import Any

from fraud_engine.detectors.base import parse_ts


class PropertyGraph:
    """Minimal property graph matching docs/graph.md schema v1 + W30 v3."""

    def __init__(self) -> None:
        self.nodes: dict[str, dict[str, Any]] = {}
        self.edges: list[dict[str, Any]] = []
        self.alerts: dict[str, dict[str, Any]] = {}
        self._ids = itertools.count(1)

    # -- node builders ----------------------------------------------------
    def add_node(self, labels: set[str], **props: Any) -> str:
        key = f"n{next(self._ids)}"
        self.nodes[key] = {"labels": set(labels), "props": dict(props)}
        return key

    def add_tenant(self, tenant_id: str) -> str:
        return self.add_node({"Tenant"}, tenant_id=tenant_id, slug=tenant_id)

    def add_person(self, tenant_id: str, person_id: str, **props: Any) -> str:
        return self.add_node(
            {"Person"}, tenant_id=tenant_id, person_id=person_id, **props
        )

    def add_contact(self, tenant_id: str, lead_id: str, **props: Any) -> str:
        return self.add_node({"Contact"}, tenant_id=tenant_id, lead_id=lead_id, **props)

    def add_location(self, tenant_id: str, **props: Any) -> str:
        return self.add_node({"Location"}, tenant_id=tenant_id, **props)

    def add_consent(self, tenant_id: str, purpose: str, **props: Any) -> str:
        return self.add_node(
            {"Consent"}, tenant_id=tenant_id, purpose=purpose, **props
        )

    def add_booking(self, tenant_id: str, booking_id: str, **props: Any) -> str:
        return self.add_node(
            {"Booking"}, tenant_id=tenant_id, booking_id=booking_id, **props
        )

    def add_case(self, tenant_id: str, ref: str, **props: Any) -> str:
        return self.add_node({"Case"}, tenant_id=tenant_id, case_id=ref, **props)

    def add_campaign(self, tenant_id: str, campaign_id: str) -> str:
        return self.add_node(
            {"Campaign"}, tenant_id=tenant_id, campaign_id=campaign_id, kind="outreach"
        )

    # -- edge builder ------------------------------------------------------
    def add_edge(self, src: str, etype: str, dst: str, **props: Any) -> None:
        self.edges.append({"src": src, "type": etype, "dst": dst, "props": dict(props)})

    # -- traversal helpers -------------------------------------------------
    def out_edges(self, key: str, etype: str | None = None) -> list[dict[str, Any]]:
        return [
            e for e in self.edges if e["src"] == key and (etype is None or e["type"] == etype)
        ]

    def in_edges(self, key: str, etype: str | None = None) -> list[dict[str, Any]]:
        return [
            e for e in self.edges if e["dst"] == key and (etype is None or e["type"] == etype)
        ]

    def persons(self, tenant_id: str) -> list[tuple[str, dict[str, Any]]]:
        return [
            (k, n["props"])
            for k, n in self.nodes.items()
            if "Person" in n["labels"] and n["props"].get("tenant_id") == tenant_id
        ]

    def person_key(self, tenant_id: str, person_id: str) -> str | None:
        for key, props in self.persons(tenant_id):
            if props.get("person_id") == person_id:
                return key
        return None


def _ge(a: Any, b: Any) -> bool:
    """Timezone-aware-ish >= for ISO strings / epochs."""
    ta, tb = parse_ts(a), parse_ts(b)
    if ta is not None and tb is not None:
        return ta >= tb
    return str(a) >= str(b)


class FakeGraphClient:
    """Marker-dispatched GraphClient over a PropertyGraph."""

    def __init__(self, graph: PropertyGraph | None = None) -> None:
        self.g = graph or PropertyGraph()
        self.calls: list[tuple[str, dict[str, Any]]] = []

    # -- GraphClient protocol ---------------------------------------------
    def ping(self) -> bool:
        return True

    def close(self) -> None:
        pass

    def query(self, cypher: str, params: dict[str, Any]) -> list[dict[str, Any]]:
        self.calls.append((cypher, dict(params)))
        marker = ""
        for line in cypher.splitlines():
            line = line.strip()
            if line.startswith("//"):
                marker = line[2:].strip()
                break
        handler = getattr(self, f"_h_{marker.split(':')[0]}_{marker.split(':')[1].split()[0]}", None) if ":" in marker else None
        if handler is None:
            raise AssertionError(f"fake has no handler for marker {marker!r}")
        return handler(cypher, params)

    # -- handlers -----------------------------------------------------------
    def _h_sweep_tenants(self, cypher, params):
        ids = {
            n["props"]["tenant_id"]
            for n in self.g.nodes.values()
            if "Tenant" in n["labels"] and n["props"].get("tenant_id")
        }
        return [{"tenant_id": t} for t in sorted(ids)]

    def _h_detector_d1_referral_cycle(self, cypher, params):
        m = re.search(r"\*(\d+)\.\.(\d+)", cypher)
        lo, hi = int(m.group(1)), int(m.group(2))
        tenant = params["tenant_id"]
        referred = [
            e for e in self.g.edges
            if e["type"] == "REFERRED"
            and self.g.nodes[e["src"]]["props"].get("tenant_id") == tenant
            and self.g.nodes[e["dst"]]["props"].get("tenant_id") == tenant
        ]
        adj: dict[str, list[str]] = {}
        for e in referred:
            adj.setdefault(e["src"], []).append(e["dst"])

        rows = []
        for start in adj:
            # DFS cycles start -> ... -> start with hops in [lo, hi]
            stack = [(start, [start])]
            while stack:
                node, path = stack.pop()
                for nxt in adj.get(node, []):
                    if nxt == start and lo <= len(path) <= hi:
                        ids = [self.g.nodes[k]["props"]["person_id"] for k in path] + [
                            self.g.nodes[start]["props"]["person_id"]
                        ]
                        rows.append({"cycle": ids, "hops": len(path)})
                    elif nxt not in path and len(path) < hi:
                        stack.append((nxt, path + [nxt]))
        return rows

    def _h_detector_d1_conversions(self, cypher, params):
        tenant = params["tenant_id"]
        wanted = set(params["person_ids"])
        out = set()
        for key, props in self.g.persons(tenant):
            if props.get("person_id") not in wanted:
                continue
            for e in self.g.out_edges(key, "BOOKED"):
                booking = self.g.nodes[e["dst"]]["props"]
                if booking.get("status") != "cancelled":
                    out.add(props["person_id"])
        return [{"person_id": pid} for pid in sorted(out)]

    def _contact_rows(self, tenant: str, since: Any, need_location: bool):
        rows = []
        for key, node in self.g.nodes.items():
            if "Contact" not in node["labels"]:
                continue
            c = node["props"]
            if c.get("tenant_id") != tenant or not c.get("captured_by"):
                continue
            if since is not None and not _ge(c.get("captured_at"), since):
                continue
            person = None
            for e in self.g.in_edges(key, "HAS_CONTACT"):
                src = self.g.nodes[e["src"]]
                if "Person" in src["labels"]:
                    person = src["props"]
            loc = None
            for e in self.g.out_edges(key, "CAPTURED_AT"):
                loc = self.g.nodes[e["dst"]]["props"]
            if need_location and (loc is None or loc.get("lat") is None or loc.get("lon") is None):
                continue
            rows.append((c, person, loc))
        return rows

    def _h_detector_d2_sybil_cluster(self, cypher, params):
        rows = []
        for c, person, loc in self._contact_rows(params["tenant_id"], params.get("since"), False):
            if person is None or loc is None:
                continue
            rows.append(
                {
                    "person_id": person.get("person_id"),
                    "name": person.get("name"),
                    "embedding": person.get("name_embedding"),
                    "lead_id": c.get("lead_id"),
                    "agent": c.get("captured_by"),
                    "captured_at": c.get("captured_at"),
                    "lga": loc.get("lga"),
                    "ward": loc.get("ward"),
                    "lat": loc.get("lat"),
                    "lon": loc.get("lon"),
                }
            )
        return rows

    def _h_detector_d3_capture_velocity(self, cypher, params):
        return [
            {"agent": c.get("captured_by"), "lead_id": c.get("lead_id"), "captured_at": c.get("captured_at")}
            for c, _, _ in self._contact_rows(params["tenant_id"], params.get("since"), False)
        ]

    def _h_detector_d4_geo_impossibility(self, cypher, params):
        return [
            {
                "agent": c.get("captured_by"),
                "lead_id": c.get("lead_id"),
                "captured_at": c.get("captured_at"),
                "lat": loc.get("lat"),
                "lon": loc.get("lon"),
            }
            for c, _, loc in self._contact_rows(params["tenant_id"], params.get("since"), True)
        ]

    def _h_detector_d5_consent_backdating(self, cypher, params):
        tenant = params["tenant_id"]
        rows = []
        for key, props in self.g.persons(tenant):
            for ce in self.g.out_edges(key, "CONSENTED"):
                consent = self.g.nodes[ce["dst"]]["props"]
                granted = ce["props"].get("granted_at") or consent.get("granted_at")
                if granted is None:
                    continue
                purpose = consent.get("purpose")
                matched = []
                for me in self.g.out_edges(key, "MESSAGED"):
                    m = me["props"]
                    if m.get("at") is None:
                        continue
                    if m.get("purpose") is not None and m.get("purpose") != purpose:
                        continue
                    tm, tg = parse_ts(m.get("at")), parse_ts(granted)
                    if tm is not None and tg is not None and tm < tg:
                        matched.append(tm)
                if matched:
                    rows.append(
                        {
                            "person_id": props.get("person_id"),
                            "purpose": purpose,
                            "granted_at": str(granted),
                            "first_messaged_at": min(matched).isoformat(),
                            "messages_before_consent": len(matched),
                        }
                    )
        return rows

    def _h_detector_d6_ghost_booking(self, cypher, params):
        tenant = params["tenant_id"]
        since = params.get("since")
        rows = []
        for node in self.g.nodes.values():
            if "Booking" not in node["labels"]:
                continue
            b = node["props"]
            if (
                b.get("tenant_id") != tenant
                or not b.get("created_by")
                or b.get("status") != "cancelled"
                or not b.get("cancelled_at")
            ):
                continue
            if since is not None and not _ge(b.get("created_at"), since):
                continue
            rows.append(
                {
                    "staff": b.get("created_by"),
                    "booking_id": b.get("booking_id"),
                    "created_at": b.get("created_at"),
                    "cancelled_at": b.get("cancelled_at"),
                }
            )
        return rows

    def _h_detector_d7_gnn_anomaly(self, cypher, params):
        tenant = params["tenant_id"]
        threshold = params["threshold"]
        return [
            {"person_id": p.get("person_id"), "risk_score": p.get("risk_score")}
            for _, p in self.g.persons(tenant)
            if p.get("risk_score") is not None and float(p.get("risk_score")) >= threshold
        ]

    def _h_detector_d8_report_velocity(self, cypher, params):
        tenant = params["tenant_id"]
        since = params.get("since")
        rows = []
        for e in self.g.edges:
            if e["type"] != "REPORTED":
                continue
            person = self.g.nodes[e["src"]]
            case = self.g.nodes[e["dst"]]
            if "Person" not in person["labels"] or "Case" not in case["labels"]:
                continue
            if person["props"].get("tenant_id") != tenant or case["props"].get("tenant_id") != tenant:
                continue
            created = case["props"].get("created_at")
            if since is not None and not _ge(created, since):
                continue
            rows.append(
                {
                    "person_id": person["props"].get("person_id"),
                    "case_ref": case["props"].get("case_id"),
                    "created_at": created,
                }
            )
        return rows

    def _h_detector_d8_coordinated_spam(self, cypher, params):
        tenant = params["tenant_id"]
        since = params.get("since")
        rows = []
        for key, node in self.g.nodes.items():
            if "Case" not in node["labels"]:
                continue
            cs = node["props"]
            if cs.get("tenant_id") != tenant:
                continue
            if since is not None and not _ge(cs.get("created_at"), since):
                continue
            loc = None
            for e in self.g.out_edges(key, "AT"):
                loc = self.g.nodes[e["dst"]]["props"]
            if loc is None or loc.get("lat") is None or loc.get("lon") is None:
                continue
            reporter = None
            for e in self.g.in_edges(key, "REPORTED"):
                src = self.g.nodes[e["src"]]
                if "Person" in src["labels"] and src["props"].get("tenant_id") == tenant:
                    reporter = src["props"].get("person_id")
            rows.append(
                {
                    "case_ref": cs.get("case_id"),
                    "category": cs.get("category"),
                    "status": cs.get("status"),
                    "created_at": cs.get("created_at"),
                    "lat": loc.get("lat"),
                    "lon": loc.get("lon"),
                    "reporter_id": reporter,
                }
            )
        return rows

    # -- writers -------------------------------------------------------------
    def _h_write_alert_merge(self, cypher, params):
        alert_id = params["alert_id"]
        existing = self.g.alerts.get(alert_id)
        if existing is None:
            self.g.alerts[alert_id] = {
                "alert_id": alert_id,
                "tenant_id": params["tenant_id"],
                "type": params["type"],
                "severity": params["severity"],
                "status": "open",
                "person_id": params.get("person_id"),
                "agent_id": params.get("agent_id"),
                "evidence": params["evidence"],
                "created_at": params["created_at"],
            }
            return [{"created": True, "status": "open"}]
        # ON MATCH: refresh evidence/severity only while open (audit rule).
        if existing["status"] == "open":
            existing["evidence"] = params["evidence"]
            existing["severity"] = params["severity"]
        return [{"created": False, "status": existing["status"]}]

    def _h_write_alert_flag_person(self, cypher, params):
        key = self.g.person_key(params["tenant_id"], params["person_id"])
        if key is not None:
            alert_node = self.g.add_node({"Alert"}, alert_id=params["alert_id"], tenant_id=params["tenant_id"])
            self.g.add_edge(alert_node, "FLAGGED", key)
        return []

    def _h_write_person_risk_flag(self, cypher, params):
        key = self.g.person_key(params["tenant_id"], params["person_id"])
        if key is None:
            return []
        props = self.g.nodes[key]["props"]
        flags = list(props.get("risk_flags") or [])
        if params["flag"] not in flags:
            flags.append(params["flag"])
        props["risk_flags"] = flags
        return [{"person_id": params["person_id"]}]

    def _h_write_quarantine(self, cypher, params):
        key = self.g.person_key(params["tenant_id"], params["person_id"])
        if key is None:
            return []
        props = self.g.nodes[key]["props"]
        props["quarantine"] = True
        props["quarantined_at"] = params["at"]
        props["quarantine_reason"] = params["reason"]
        return [{"person_id": params["person_id"]}]
