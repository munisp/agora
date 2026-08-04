/**
 * Workforce (shifts, time, leave) shared types + pure helpers
 * (SPEC-W20 Agent D). Nothing here touches the network — safe to import
 * anywhere. Mirrors the backend contract in
 * booking-service internal/workforce (docs/apps/workforce.md).
 */

/** Shift statuses — scheduled → confirmed → completed|no_show|cancelled. */
export const SHIFT_STATUSES = [
  "scheduled",
  "confirmed",
  "completed",
  "no_show",
  "cancelled",
] as const;
export type ShiftStatus = (typeof SHIFT_STATUSES)[number];

export interface Shift {
  id: string;
  tenant_id: string;
  agent_id: string;
  starts_at: string;
  ends_at: string;
  role: string;
  status: ShiftStatus;
  created_at: string;
  updated_at: string;
}

/** GET /shifts/week row: the shift plus the resolved agent name. */
export interface ShiftView extends Shift {
  agent_name: string;
}

/** GET /shifts/week response. */
export interface WeekGrid {
  week_start: string;
  days: string[]; // 7 × YYYY-MM-DD
  shifts: ShiftView[];
}

export interface TeamMember {
  id: string;
  name: string;
  email: string;
}

export interface TimeEntry {
  id: string;
  tenant_id: string;
  agent_id: string;
  clock_in_at: string;
  clock_out_at: string | null;
  method: "web" | "field_pwa";
  gps_lat: number | null;
  gps_lng: number | null;
}

export const LEAVE_KINDS = ["annual", "sick", "unpaid"] as const;
export type LeaveKind = (typeof LEAVE_KINDS)[number];

export interface LeaveRequest {
  id: string;
  tenant_id: string;
  agent_id: string;
  kind: LeaveKind;
  starts_on: string;
  ends_on: string;
  status: "pending" | "approved" | "declined";
  reason: string;
  decided_by: string;
  decided_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface UtilizationRow {
  agent_id: string;
  agent_name: string;
  scheduled_hours: number;
  clocked_hours: number;
  utilization_pct: number | null;
  open_entries: number;
}

export interface CoverageDay {
  date: string; // YYYY-MM-DD
  agents_scheduled: number;
  bookings: number;
}

/** Legal next statuses from a given shift status (mirrors the backend
 *  matrix; same-status is a no-op, not offered). */
export const NEXT_SHIFT_STATUS: Record<ShiftStatus, ShiftStatus[]> = {
  scheduled: ["confirmed", "completed", "no_show", "cancelled"],
  confirmed: ["completed", "no_show", "cancelled"],
  completed: [],
  no_show: [],
  cancelled: [],
};

/** Human labels. */
export const SHIFT_LABEL: Record<ShiftStatus, string> = {
  scheduled: "Scheduled",
  confirmed: "Confirmed",
  completed: "Completed",
  no_show: "No show",
  cancelled: "Cancelled",
};

/** Badge variant per shift status (warm token set from components/ui/badge). */
export function shiftVariant(
  s: ShiftStatus,
): "secondary" | "info" | "warning" | "success" | "destructive" | "outline" {
  switch (s) {
    case "scheduled":
      return "secondary";
    case "confirmed":
      return "info";
    case "completed":
      return "success";
    case "no_show":
      return "warning";
    case "cancelled":
      return "destructive";
    default:
      return "outline";
  }
}

/** Badge variant per leave status. */
export function leaveVariant(
  s: LeaveRequest["status"],
): "secondary" | "warning" | "success" | "destructive" {
  switch (s) {
    case "pending":
      return "warning";
    case "approved":
      return "success";
    case "declined":
      return "destructive";
    default:
      return "secondary";
  }
}

/** YYYY-MM-DD of an ISO timestamp (UTC date — matches the backend's
 *  week/coverage day bucketing). */
export function dayOf(iso: string): string {
  return iso.slice(0, 10);
}

/** HH:mm of an ISO timestamp (UTC — honest, no silent local conversion). */
export function timeOf(iso: string): string {
  return iso.slice(11, 16);
}

export function shortId(id: string): string {
  return id.slice(0, 8);
}
