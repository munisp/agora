/**
 * Campaign Studio shared types + BFF path constants (SPEC-W19 Agent D).
 * Nothing here touches the network — safe to import anywhere. Lists are
 * read through the tolerant unwrap<T>() from components/apps/types.ts
 * (READ-ONLY import).
 */

/** BFF base for all Campaign Studio traffic (booking-service /v1/studio). */
export const STUDIO_API = "/api/bookings/v1/studio";

export type SegmentFilter = {
  field: string;
  op: "eq" | "neq" | "in" | "gte" | "lte" | "contains";
  value: unknown;
};

export type SegmentDefinition = { filters: SegmentFilter[] };

export type Segment = {
  id: string;
  tenant_id: string;
  name: string;
  definition: SegmentDefinition;
  approx_count: number;
  created_at: string;
  updated_at: string;
};

export type JourneyStatus = "draft" | "active" | "paused" | "archived";

export type JourneyStep = {
  type: "wait" | "send" | "branch";
  kind?: "sms" | "push_marketing" | "ussd";
  template?: string;
  wait_hours?: number;
  condition?: SegmentDefinition;
  ab_variant?: string;
};

export type Journey = {
  id: string;
  tenant_id: string;
  name: string;
  status: JourneyStatus;
  trigger_kind: "segment" | "manual" | "event";
  segment_id: string | null;
  steps: JourneyStep[];
  created_at: string;
  updated_at: string;
};

export type StepStat = {
  step_idx: number;
  type: string;
  active: number;
  passed: number;
  sent: number;
  suppressed: number;
  skipped: number;
  failed: number;
  exited: number;
};

export type JourneyStats = {
  enrolled: number;
  active: number;
  completed: number;
  exited: number;
  per_step: StepStat[];
};

export type StepRunResult = {
  journey_id: string;
  scanned: number;
  advanced: number;
  completed: number;
  exited: number;
  skipped: number;
  wait_not_due: number;
  sends_queued: number;
  sends_deferred: boolean;
  dispatch: string;
  workflow_id?: string;
};

/** Filter field options offered by the segment builder (backend whitelist). */
export const FILTER_FIELDS: { value: string; label: string }[] = [
  { value: "name", label: "Contact name" },
  { value: "phone", label: "Contact phone" },
  { value: "email", label: "Contact email" },
  { value: "source", label: "Contact source" },
  { value: "external_id", label: "Contact external id" },
  { value: "lead_status", label: "Lead status" },
  { value: "lead_channel", label: "Lead channel" },
  { value: "lead_campaign_id", label: "Lead campaign id" },
  { value: "lead_created_at", label: "Lead created at" },
];

export const FILTER_OPS: { value: SegmentFilter["op"]; label: string }[] = [
  { value: "eq", label: "equals" },
  { value: "neq", label: "not equals" },
  { value: "in", label: "is one of (comma-separated)" },
  { value: "gte", label: ">=" },
  { value: "lte", label: "<=" },
  { value: "contains", label: "contains" },
];

/** Status pill tone per journey status (warm tokens). */
export function statusTone(status: JourneyStatus): string {
  switch (status) {
    case "active":
      return "bg-emerald-100 text-emerald-800";
    case "paused":
      return "bg-amber-100 text-amber-800";
    case "archived":
      return "bg-stone-200 text-stone-600";
    default:
      return "bg-sky-100 text-sky-800"; // draft
  }
}
