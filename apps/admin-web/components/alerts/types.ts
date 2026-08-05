/**
 * SPEC-W30 WS-D: shared types + helpers for the fraud & trust alert queue.
 *
 * Backend contract (graph-service alerts router, reached through the
 * existing tenant-scoped /api/graph/* gateway mount):
 *
 *   GET  /api/graph/v1/graph/alerts?status=&type=&severity=   list (JWT,
 *                                                               tenant-scoped)
 *   GET  /api/graph/v1/graph/alerts/{id}                      detail
 *   POST /api/graph/v1/graph/alerts/{id}/resolve              {decision:
 *          "confirmed"|"dismissed", reason} — reason mandatory, min 10
 *          chars (mirrored client-side; server enforces with 422).
 *
 * Alert node (SPEC-W30 §2): {alert_id, tenant_id, type, severity, status,
 * person_id?, agent_id?, evidence, created_at, resolved_at?, resolved_by?,
 * resolve_reason?}. `evidence` is a JSON string of the matched pattern so
 * auditors can replay exactly why the detector fired — the UI parses it
 * once here and renders it readably per detector type.
 */
import { BRAND, GRAPH_API } from "@/components/segments/types";

export const ALERTS_API = `${GRAPH_API}/alerts`;

/** Detector taxonomy (SPEC-W30 §0/§3). Unknown future types degrade to the raw string. */
export const ALERT_TYPES = [
  { value: "referral_cycle", label: "Referral ring" },
  { value: "sybil_cluster", label: "Sybil / duplicates" },
  { value: "capture_velocity", label: "Capture velocity" },
  { value: "geo_impossibility", label: "Geo impossibility" },
  { value: "consent_backdating", label: "Consent backdating" },
  { value: "ghost_booking", label: "Ghost bookings" },
  { value: "gnn_anomaly", label: "Anomalous actor" },
] as const;

export const ALERT_STATUSES = [
  { value: "open", label: "Open" },
  { value: "confirmed", label: "Confirmed" },
  { value: "dismissed", label: "Dismissed" },
] as const;

export type AlertSeverity = "low" | "medium" | "high";

/** Severity ramp: sage → amber → terracotta (brand rule: never red/blue). */
export const SEVERITY_META: Record<
  AlertSeverity,
  { label: string; fg: string; bg: string; border: string }
> = {
  low: {
    label: "Low",
    fg: BRAND.sage,
    bg: `${BRAND.sage}1f`,
    border: `${BRAND.sage}55`,
  },
  medium: {
    label: "Medium",
    fg: "#a8762f",
    bg: `${BRAND.amber}26`,
    border: `${BRAND.amber}66`,
  },
  high: {
    label: "High",
    fg: BRAND.terracotta,
    bg: `${BRAND.terracotta}1a`,
    border: `${BRAND.terracotta}59`,
  },
};

export function severityMeta(severity: string) {
  return SEVERITY_META[severity as AlertSeverity] ?? SEVERITY_META.medium;
}

export function alertTypeLabel(type: string): string {
  return ALERT_TYPES.find((t) => t.value === type)?.label ?? type.replace(/_/g, " ");
}

export interface FraudAlert {
  alert_id: string;
  type: string;
  severity: string;
  status: string;
  person_id?: string;
  agent_id?: string;
  /** Parsed evidence object (empty object when absent/unparseable). */
  evidence: Record<string, unknown>;
  created_at?: string;
  resolved_at?: string;
  resolved_by?: string;
  resolve_reason?: string;
}

/** Evidence arrives as a JSON string (SPEC-W30 §2); tolerate pre-parsed too. */
export function parseEvidence(raw: unknown): Record<string, unknown> {
  if (typeof raw === "string") {
    try {
      const parsed: unknown = JSON.parse(raw);
      if (typeof parsed === "object" && parsed !== null && !Array.isArray(parsed)) {
        return parsed as Record<string, unknown>;
      }
      return { value: parsed };
    } catch {
      return { raw };
    }
  }
  if (typeof raw === "object" && raw !== null && !Array.isArray(raw)) {
    return raw as Record<string, unknown>;
  }
  return {};
}

/** Tolerant alert row decode (id/status/severity key tolerance). */
export function normalizeAlert(raw: Record<string, unknown>): FraudAlert {
  const str = (k: string) =>
    raw[k] === null || raw[k] === undefined ? undefined : String(raw[k]);
  return {
    alert_id: str("alert_id") ?? str("id") ?? "",
    type: str("type") ?? "unknown",
    severity: str("severity") ?? "medium",
    status: str("status") ?? "open",
    person_id: str("person_id"),
    agent_id: str("agent_id"),
    evidence: parseEvidence(raw.evidence),
    created_at: str("created_at"),
    resolved_at: str("resolved_at"),
    resolved_by: str("resolved_by"),
    resolve_reason: str("resolve_reason"),
  };
}

/** Resolve contract (SPEC-W30 §4 WS-C): reason mandatory, min 10 chars. */
export const RESOLVE_REASON_MIN = 10;

export type ResolveDecision = "confirmed" | "dismissed";

export function validateResolveReason(reason: string): string | null {
  const trimmed = reason.trim();
  if (trimmed.length < RESOLVE_REASON_MIN) {
    return `Give a reason of at least ${RESOLVE_REASON_MIN} characters — it becomes part of the audit trail.`;
  }
  return null;
}
