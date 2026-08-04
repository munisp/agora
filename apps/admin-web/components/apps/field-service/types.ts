/**
 * Field-service (work orders & dispatch) shared types + pure helpers
 * (SPEC-W19 Agent B). Nothing here touches the network — safe to import
 * anywhere. Mirrors the backend contract in
 * booking-service internal/workorders (docs/apps/field-service.md).
 */

/** Work order statuses — the state machine
 *  created → assigned → en_route → on_site → completed, any → cancelled. */
export const WO_STATUSES = [
  "created",
  "assigned",
  "en_route",
  "on_site",
  "completed",
  "cancelled",
] as const;
export type WorkOrderStatus = (typeof WO_STATUSES)[number];

export interface ChecklistItem {
  label: string;
  done: boolean;
}

export interface Proof {
  notes?: string;
  photos?: string[];
}

export interface WorkOrder {
  id: string;
  tenant_id: string;
  contact_id: string | null;
  booking_id: string | null;
  title: string;
  description: string;
  status: WorkOrderStatus;
  assignee_id: string | null;
  scheduled_start: string | null;
  scheduled_end: string | null;
  gps_lat: number | null;
  gps_lng: number | null;
  gps_accuracy: number | null;
  checklist: ChecklistItem[];
  proof: Proof;
  field_capture_id: string | null;
  created_at: string;
  updated_at: string;
  completed_at: string | null;
}

/** GET /board row: the work order plus the resolved assignee name. */
export interface BoardItem extends WorkOrder {
  assignee_name: string;
}

/** GET /board response: one lane per status (always all six keys). */
export type Board = Record<WorkOrderStatus, BoardItem[]>;

/** Legal next statuses from a given status (mirrors the backend matrix;
 *  assigned → assigned is the re-dispatch edge, handled by the dispatch
 *  control rather than a lane button). */
export const NEXT_STATUS: Record<WorkOrderStatus, WorkOrderStatus[]> = {
  created: ["assigned", "cancelled"],
  assigned: ["en_route", "cancelled"],
  en_route: ["on_site", "cancelled"],
  on_site: ["completed", "cancelled"],
  completed: [],
  cancelled: [],
};

/** Human labels for lanes/buttons. */
export const STATUS_LABEL: Record<WorkOrderStatus, string> = {
  created: "Created",
  assigned: "Assigned",
  en_route: "En route",
  on_site: "On site",
  completed: "Completed",
  cancelled: "Cancelled",
};

/** Badge variant per status (warm token set from components/ui/badge). */
export function statusVariant(
  s: WorkOrderStatus,
): "secondary" | "info" | "warning" | "success" | "destructive" | "outline" {
  switch (s) {
    case "created":
      return "secondary";
    case "assigned":
      return "info";
    case "en_route":
      return "warning";
    case "on_site":
      return "warning";
    case "completed":
      return "success";
    case "cancelled":
      return "destructive";
    default:
      return "outline";
  }
}

/** Client-side completion-gate hint (the server is authoritative):
 *  every checklist item done + non-empty proof.notes. */
export function completionBlockers(wo: WorkOrder): string[] {
  const blockers: string[] = [];
  const open = wo.checklist.filter((i) => !i.done);
  if (open.length > 0) {
    blockers.push(`${open.length} checklist item${open.length > 1 ? "s" : ""} not done`);
  }
  if (!wo.proof?.notes || wo.proof.notes.trim() === "") {
    blockers.push("proof notes required");
  }
  return blockers;
}

export function shortId(id: string): string {
  return id.slice(0, 8);
}
