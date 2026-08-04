/**
 * Surveys / VoC shared types + pure helpers (SPEC-W20 Agent B). Nothing
 * here touches the network — safe to import anywhere. Mirrors the backend
 * contract in booking-service internal/surveys (docs/apps/surveys-voc.md).
 */

/** Survey status machine: draft → active → paused ↔ active → archived. */
export const SURVEY_STATUSES = ["draft", "active", "paused", "archived"] as const;
export type SurveyStatus = (typeof SURVEY_STATUSES)[number];

export const SURVEY_KINDS = ["nps", "csat", "ces", "custom"] as const;
export type SurveyKind = (typeof SURVEY_KINDS)[number];

export const QUESTION_TYPES = ["rating", "text", "single", "multi"] as const;
export type QuestionType = (typeof QUESTION_TYPES)[number];

export const SURVEY_CHANNELS = ["sms", "push_marketing"] as const;
export type SurveyChannel = (typeof SURVEY_CHANNELS)[number];

export const TRIGGER_KINDS = ["manual", "ticket_resolved", "booking_completed"] as const;

export interface Question {
  id: string;
  type: QuestionType;
  label: string;
  options?: string[];
  required: boolean;
}

export interface Survey {
  id: string;
  tenant_id: string;
  name: string;
  status: SurveyStatus;
  kind: SurveyKind;
  questions: Question[];
  trigger_kind: string;
  channel: SurveyChannel;
  created_at: string;
  updated_at: string;
}

/** GET /surveys/{id} stats rollup. */
export interface SurveyStats {
  invites_queued: number;
  invites_sent: number;
  invites_answered: number;
  invites_expired: number;
  responses: number;
}

export interface Invite {
  id: string;
  contact_id: string;
  token: string;
  link: string;
  status: string;
}

export interface SkippedContact {
  contact_id: string;
  reason: string;
}

/** POST /surveys/{id}/send response. */
export interface SendResult {
  invites: Invite[];
  invites_created: number;
  sent: number;
  queued: number;
  skipped: SkippedContact[];
  sends_deferred: boolean;
}

export interface OptionCount {
  option: string;
  count: number;
}

export interface QuestionBreakdown {
  id: string;
  type: string;
  label: string;
  answer_count: number;
  options: OptionCount[];
}

/** GET /surveys/{id}/results block. */
export interface SurveyResults {
  survey_id: string;
  kind: SurveyKind;
  response_count: number;
  score_distribution: Record<string, number>;
  scored_count: number;
  nps: number | null;
  promoters: number;
  passives: number;
  detractors: number;
  mean_score: number | null;
  questions: QuestionBreakdown[];
}

/** GET /voc/themes row. */
export interface Theme {
  term: string;
  count: number;
}

export const KIND_LABEL: Record<SurveyKind, string> = {
  nps: "NPS",
  csat: "CSAT",
  ces: "CES",
  custom: "Custom",
};

export const STATUS_LABEL: Record<SurveyStatus, string> = {
  draft: "Draft",
  active: "Active",
  paused: "Paused",
  archived: "Archived",
};

/** Badge variant per status (warm token set from components/ui/badge). */
export function statusVariant(
  s: SurveyStatus,
): "secondary" | "success" | "warning" | "outline" {
  switch (s) {
    case "active":
      return "success";
    case "paused":
      return "warning";
    case "draft":
      return "secondary";
    default:
      return "outline";
  }
}

/** Legal next statuses (mirrors the backend machine; archived terminal). */
export const NEXT_STATUS: Record<SurveyStatus, SurveyStatus[]> = {
  draft: ["active", "archived"],
  active: ["paused", "archived"],
  paused: ["active", "archived"],
  archived: [],
};

/** NPS tile colour (warm tokens): >= 50 great, >= 0 ok, < 0 bad. */
export function npsTone(nps: number): string {
  if (nps >= 50) return "text-success";
  if (nps >= 0) return "text-warning";
  return "text-destructive";
}

/** Score distribution rows sorted by score ascending (0..10 keys). */
export function distributionRows(dist: Record<string, number>): [string, number][] {
  return Object.entries(dist).sort((a, b) => Number(a[0]) - Number(b[0]));
}

export function shortId(id: string): string {
  return id.slice(0, 8);
}
