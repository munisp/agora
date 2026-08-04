/**
 * Helpdesk app types + display helpers (SPEC-W19 Agent A). Mirrors the
 * booking-service internal/helpdesk JSON shapes exactly (snake_case) — the
 * BFF path is /api/bookings/v1/helpdesk/*. List unwrap uses the shared
 * tolerant unwrap<T>() from components/apps/types.ts (READ-ONLY import).
 */

export type TicketPriority = "low" | "normal" | "high" | "urgent";
export type TicketStatus = "open" | "pending" | "resolved" | "closed";

export interface Ticket {
  id: string;
  tenant_id: string;
  contact_id: string | null;
  conversation_id: string | null;
  subject: string;
  channel: string;
  priority: TicketPriority;
  status: TicketStatus;
  assignee_id: string | null;
  sla_policy_id: string | null;
  due_first_response_at: string | null;
  due_resolve_at: string | null;
  first_response_at: string | null;
  resolved_at: string | null;
  csat_rating: number | null;
  csat_comment: string | null;
  csat_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface TicketEvent {
  id: string;
  tenant_id: string;
  ticket_id: string;
  kind:
    | "created"
    | "assigned"
    | "status_changed"
    | "note"
    | "first_response"
    | "resolved"
    | "reopened";
  actor: string;
  payload: Record<string, unknown>;
  ts: string;
}

export interface SLAPolicy {
  id: string;
  tenant_id: string;
  name: string;
  priority: TicketPriority;
  first_response_minutes: number;
  resolve_minutes: number;
  active: boolean;
  created_at: string;
  updated_at: string;
}

export interface HelpdeskStats {
  open_by_priority: Record<string, number>;
  open_count: number;
  breached_count: number;
  resolved_30d: number;
  avg_first_response_minutes_30d: number | null;
  avg_resolve_minutes_30d: number | null;
  avg_csat_30d: number | null;
}

export interface TeamMember {
  id: string;
  name: string;
  email: string;
}

export const PRIORITIES: TicketPriority[] = ["low", "normal", "high", "urgent"];
export const STATUSES: TicketStatus[] = ["open", "pending", "resolved", "closed"];

/** Priority pill variants (queue board + drawer). */
export const PRIORITY_META: Record<
  TicketPriority,
  { label: string; variant: "secondary" | "info" | "warning" | "destructive" }
> = {
  low: { label: "Low", variant: "secondary" },
  normal: { label: "Normal", variant: "info" },
  high: { label: "High", variant: "warning" },
  urgent: { label: "Urgent", variant: "destructive" },
};

export const STATUS_META: Record<TicketStatus, { label: string }> = {
  open: { label: "Open" },
  pending: { label: "Pending" },
  resolved: { label: "Resolved" },
  closed: { label: "Closed" },
};

/**
 * Client-side breach check for board badges (mirrors GET
 * /v1/helpdesk/breaches: now > due_*, status not in resolved|closed). The
 * stats tile count comes from the server; this is for per-card badges.
 */
export function ticketBreaches(t: Ticket, now = new Date()): {
  firstResponse: boolean;
  resolve: boolean;
} {
  if (t.status === "resolved" || t.status === "closed") {
    return { firstResponse: false, resolve: false };
  }
  return {
    firstResponse:
      !t.first_response_at &&
      !!t.due_first_response_at &&
      now > new Date(t.due_first_response_at),
    resolve: !!t.due_resolve_at && now > new Date(t.due_resolve_at),
  };
}

/** "1h 25m" / "3d 2h" style duration for SLA targets + averages. */
export function formatMinutes(min: number | null | undefined): string {
  if (min === null || min === undefined || Number.isNaN(min)) return "—";
  const m = Math.round(min);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 48) return m % 60 === 0 ? `${h}h` : `${h}h ${m % 60}m`;
  const d = Math.floor(h / 24);
  return h % 24 === 0 ? `${d}d` : `${d}d ${h % 24}h`;
}

/** Human label for a timeline event (drawer). */
export function eventLabel(e: TicketEvent): string {
  switch (e.kind) {
    case "created":
      return "Ticket created";
    case "assigned": {
      const to = e.payload.assignee_id;
      if (to === null || to === undefined) return "Unassigned";
      return e.payload.auto === true ? "Auto-assigned" : "Assigned";
    }
    case "status_changed":
      return `Status: ${String(e.payload.from ?? "?")} → ${String(
        e.payload.to ?? "?",
      )}`;
    case "note":
      return "Note";
    case "first_response":
      return "First response";
    case "resolved":
      return "Resolved";
    case "reopened":
      return "Reopened";
    default:
      return e.kind;
  }
}
