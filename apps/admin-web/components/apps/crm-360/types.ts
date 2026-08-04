/**
 * CRM-360 shared types (SPEC-W20 Agent A). Pure shapes + label maps —
 * nothing here touches the network. Mirrors the booking-service
 * /v1/crm JSON payloads (internal/crm360).
 */

/** Base contact record (booking.contacts + W12 reverse-sync columns). */
export interface Contact {
  id: string;
  tenant_id: string;
  name: string;
  phone: string;
  email: string;
  /** Legacy free-text column — distinct from crm_notes. */
  notes: string;
  source: string;
  external_id: string;
}

/** One /contacts/search row: the contact plus its current tags. */
export interface ContactSearchResult extends Contact {
  tags: string[];
}

export interface TicketSummary {
  id: string;
  subject: string;
  status: string;
  priority: string;
  created_at: string;
}

export interface BookingSummary {
  id: string;
  status: string;
  source: string;
  starts_at: string;
  ends_at: string;
  created_at: string;
}

export interface WorkOrderSummary {
  id: string;
  title: string;
  status: string;
  scheduled_start: string | null;
}

export interface WalletSummary {
  balance: number;
  tier: string;
}

/** GET /v1/crm/contacts/{id}/360 payload. */
export interface Profile360 {
  contact: Contact;
  tags: string[];
  open_ticket_count: number;
  /** Latest 5 tickets, any status. */
  tickets: TicketSummary[];
  /** Latest 5 bookings by scheduled start. */
  bookings: BookingSummary[];
  /** Active work orders only. */
  work_orders: WorkOrderSummary[];
  /** null when the contact has no loyalty wallet. */
  wallet: WalletSummary | null;
  /** null when consent is not resolvable from booking-service. */
  consent: string | null;
}

/** One merged-feed row: {ts, kind, summary, ref_id}. */
export interface TimelineItem {
  ts: string;
  kind: TimelineKind;
  summary: string;
  ref_id: string;
}

export type TimelineKind =
  | "booking"
  | "ticket_event"
  | "work_order"
  | "loyalty"
  | "note";

export const TIMELINE_KIND_LABELS: Record<TimelineKind, string> = {
  booking: "Booking",
  ticket_event: "Ticket",
  work_order: "Work order",
  loyalty: "Loyalty",
  note: "Note",
};

/** Badge variant per timeline kind (ui/badge variants). */
export const TIMELINE_KIND_VARIANTS: Record<
  TimelineKind,
  "default" | "secondary" | "outline" | "success" | "warning" | "info"
> = {
  booking: "info",
  ticket_event: "warning",
  work_order: "default",
  loyalty: "success",
  note: "secondary",
};

/** One crm_notes row. */
export interface CrmNote {
  id: string;
  tenant_id: string;
  contact_id: string;
  author: string;
  body: string;
  pinned: boolean;
  created_at: string;
  updated_at: string;
}

/** Ticket/work-order/booking status → badge variant. */
export function statusVariant(
  status: string,
): "default" | "secondary" | "outline" | "success" | "warning" | "destructive" | "info" {
  switch (status) {
    case "open":
    case "created":
    case "pending":
      return "warning";
    case "assigned":
    case "en_route":
    case "on_site":
    case "confirmed":
      return "info";
    case "resolved":
    case "completed":
      return "success";
    case "closed":
    case "cancelled":
    case "no_show":
      return "secondary";
    default:
      return "outline";
  }
}
