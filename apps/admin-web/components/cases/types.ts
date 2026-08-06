/**
 * SPEC-W32 WS-C: shared types, API paths and tolerant normalizers for the
 * civic-reporting operator console (cases queue, case detail drawer,
 * category & routing config).
 *
 * Backend contract (Agent A's booking-service civic module, reached through
 * the APISIX gateway mount /api/civic/* — OIDC-authed, tenant resolved from
 * the X-Tenant-Slug header that the BFF attaches):
 *
 *   GET    /api/civic/cases?status=&category=&ward=&sla_breach=&q=
 *   GET    /api/civic/cases/{id}                 detail incl. reporter
 *                                                (masked unless owner/admin)
 *   POST   /api/civic/cases/{id}/triage          {category_id?, ward?, mda_queue?}
 *   POST   /api/civic/cases/{id}/assign          {assignee}
 *   POST   /api/civic/cases/{id}/status          {status, note?}
 *   POST   /api/civic/cases/{id}/merge           {canonical_id}
 *   GET    /api/civic/cases/{id}/duplicates      geo ≤500m + same category
 *                                                + ±72h candidates
 *   GET/POST   /api/civic/categories             category list / create
 *   PATCH  /api/civic/categories/{id}            category update (active, SLAs)
 *   GET/POST   /api/civic/routing-rules          ward-override routing rules
 *   PATCH/DELETE /api/civic/routing-rules/{id}
 *
 * The module ships in parallel with this UI (SPEC §3 WS-A) — every read
 * tolerates a 404 with a clean empty state, and unknown/missing fields
 * degrade gracefully instead of throwing.
 */
import { BRAND } from "@/components/segments/types";

export { BRAND };

export const CIVIC_API = "/api/civic";
export const CIVIC_PUBLIC_API = "/api/civic/public/tenants";

/* ------------------------------------------------------------------ enums */

export const CASE_STATUSES = [
  { value: "new", label: "New" },
  { value: "triaged", label: "Triaged" },
  { value: "assigned", label: "Assigned" },
  { value: "in_progress", label: "In progress" },
  { value: "resolved", label: "Resolved" },
  { value: "closed", label: "Closed" },
] as const;

export type CaseStatus = (typeof CASE_STATUSES)[number]["value"];

/** Statuses the operator status action may move a case into (SPEC §3 WS-A). */
export const ACTIONABLE_STATUSES: CaseStatus[] = [
  "in_progress",
  "resolved",
  "closed",
];

const STATUS_ORDER: CaseStatus[] = [
  "new",
  "triaged",
  "assigned",
  "in_progress",
  "resolved",
  "closed",
];

export function statusLabel(status: string): string {
  return (
    CASE_STATUSES.find((s) => s.value === status)?.label ??
    status.replace(/_/g, " ")
  );
}

export function statusIndex(status: string): number {
  const i = STATUS_ORDER.indexOf(status as CaseStatus);
  return i === -1 ? 0 : i;
}

/** SLA-breach filter options for the queue toolbar. */
export const SLA_BREACH_FILTERS = [
  { value: "any", label: "Any breach" },
  { value: "ack", label: "Ack breach" },
  { value: "resolve", label: "Resolve breach" },
] as const;

export const CASE_CHANNELS = [
  { value: "web", label: "Web portal" },
  { value: "pwa", label: "Field PWA" },
  { value: "whatsapp", label: "WhatsApp" },
] as const;

export function channelLabel(channel: string): string {
  return (
    CASE_CHANNELS.find((c) => c.value === channel)?.label ?? channel ?? "—"
  );
}

/* ----------------------------------------------------------------- models */

export interface CivicCategory {
  id: string;
  name: string;
  slug: string;
  mda_queue: string;
  ack_sla_hours: number | null;
  resolve_sla_hours: number | null;
  active: boolean;
}

export interface CivicRoutingRule {
  id: string;
  ward: string | null;
  category_id: string;
  mda_queue: string;
}

export interface CivicCase {
  id: string;
  ref: string;
  category_id: string | null;
  category_slug: string | null;
  category_name: string | null;
  status: string;
  description: string;
  ward: string | null;
  lga: string | null;
  lat: number | null;
  lon: number | null;
  location_text: string | null;
  reporter_phone_e164: string | null;
  reporter_name: string | null;
  anonymous: boolean;
  photo_url: string | null;
  channel: string | null;
  mda_queue: string | null;
  assigned_to: string | null;
  ack_due_at: string | null;
  resolve_due_at: string | null;
  acked_at: string | null;
  resolved_at: string | null;
  closed_at: string | null;
  merged_into: string | null;
  sla_breach_ack: boolean;
  sla_breach_resolve: boolean;
  wants_updates: boolean;
  created_at: string | null;
  updated_at: string | null;
}

/** One entry in the case event timeline (operator detail only). */
export interface CivicCaseEvent {
  type: string;
  at: string | null;
  note: string | null;
  actor: string | null;
}

/* ------------------------------------------------------------- normalize */

function str(v: unknown): string | null {
  return typeof v === "string" && v !== "" ? v : null;
}

function bool(v: unknown): boolean {
  return v === true;
}

function num(v: unknown): number | null {
  const n = typeof v === "string" ? Number(v) : v;
  return typeof n === "number" && Number.isFinite(n) ? n : null;
}

/** Tolerant list unwrap — first own-property array wins (CAC page idiom). */
export function unwrapList<T>(data: unknown): T[] {
  if (Array.isArray(data)) return data as T[];
  if (typeof data === "object" && data !== null) {
    for (const value of Object.values(data)) {
      if (Array.isArray(value)) return value as T[];
    }
  }
  return [];
}

export function normalizeCategory(raw: unknown): CivicCategory {
  const o = (typeof raw === "object" && raw !== null ? raw : {}) as Record<
    string,
    unknown
  >;
  return {
    id: str(o.id) ?? str(o.category_id) ?? "",
    name: str(o.name) ?? str(o.slug) ?? "Category",
    slug: str(o.slug) ?? "",
    mda_queue: str(o.mda_queue) ?? "",
    ack_sla_hours: num(o.ack_sla_hours),
    resolve_sla_hours: num(o.resolve_sla_hours),
    active: o.active === undefined ? true : bool(o.active),
  };
}

export function normalizeRoutingRule(raw: unknown): CivicRoutingRule {
  const o = (typeof raw === "object" && raw !== null ? raw : {}) as Record<
    string,
    unknown
  >;
  return {
    id: str(o.id) ?? str(o.rule_id) ?? "",
    ward: str(o.ward),
    category_id: str(o.category_id) ?? "",
    mda_queue: str(o.mda_queue) ?? "",
  };
}

export function normalizeCase(raw: unknown): CivicCase {
  const o = (typeof raw === "object" && raw !== null ? raw : {}) as Record<
    string,
    unknown
  >;
  const cat = o.category;
  const catObj = (
    typeof cat === "object" && cat !== null ? cat : {}
  ) as Record<string, unknown>;
  return {
    id: str(o.id) ?? str(o.case_id) ?? "",
    ref: str(o.ref) ?? str(o.reference) ?? "",
    category_id: str(o.category_id) ?? str(catObj.id),
    category_slug: str(o.category_slug) ?? str(catObj.slug),
    category_name: str(o.category_name) ?? str(catObj.name),
    status: str(o.status) ?? "new",
    description: str(o.description) ?? "",
    ward: str(o.ward),
    lga: str(o.lga),
    lat: num(o.lat),
    lon: num(o.lon),
    location_text: str(o.location_text),
    reporter_phone_e164:
      str(o.reporter_phone_e164) ?? str(o.reporter_phone) ?? str(o.phone),
    reporter_name: str(o.reporter_name),
    anonymous: bool(o.anonymous),
    photo_url: str(o.photo_url),
    channel: str(o.channel),
    mda_queue: str(o.mda_queue),
    assigned_to: str(o.assigned_to),
    ack_due_at: str(o.ack_due_at),
    resolve_due_at: str(o.resolve_due_at),
    acked_at: str(o.acked_at),
    resolved_at: str(o.resolved_at),
    closed_at: str(o.closed_at),
    merged_into: str(o.merged_into),
    sla_breach_ack: bool(o.sla_breach_ack),
    sla_breach_resolve: bool(o.sla_breach_resolve),
    wants_updates: bool(o.wants_updates),
    created_at: str(o.created_at),
    updated_at: str(o.updated_at),
  };
}

export function normalizeEvent(raw: unknown): CivicCaseEvent {
  const o = (typeof raw === "object" && raw !== null ? raw : {}) as Record<
    string,
    unknown
  >;
  return {
    type: str(o.type) ?? str(o.event) ?? str(o.status) ?? "event",
    at: str(o.at) ?? str(o.created_at) ?? str(o.timestamp),
    note: str(o.note) ?? str(o.message),
    actor: str(o.actor) ?? str(o.actor_id),
  };
}

/**
 * Events may arrive on the detail payload under `events`, `timeline` or
 * `history`; each entry may itself be a bare status string.
 */
export function extractEvents(detail: unknown): CivicCaseEvent[] {
  if (typeof detail !== "object" || detail === null) return [];
  const o = detail as Record<string, unknown>;
  for (const key of ["events", "timeline", "history"]) {
    const v = o[key];
    if (Array.isArray(v)) {
      return v.map((e) =>
        typeof e === "string"
          ? { type: e, at: null, note: null, actor: null }
          : normalizeEvent(e),
      );
    }
  }
  return [];
}

/* ------------------------------------------------------------ SLA helpers */

export type SlaTone = "ok" | "soon" | "breach";

/**
 * SLA ramp (brand rule: sage → amber → terracotta, never red/blue).
 * ok = comfortably inside the window; soon = under 4h remaining;
 * breach = past due or flagged breached by the backend.
 */
export const SLA_TONE_META: Record<
  SlaTone,
  { fg: string; bg: string; border: string }
> = {
  ok: { fg: BRAND.sage, bg: `${BRAND.sage}1f`, border: `${BRAND.sage}55` },
  soon: { fg: "#a8762f", bg: `${BRAND.amber}26`, border: `${BRAND.amber}66` },
  breach: {
    fg: BRAND.terracotta,
    bg: `${BRAND.terracotta}1f`,
    border: `${BRAND.terracotta}55`,
  },
};

export interface SlaCountdown {
  tone: SlaTone;
  /** Human label, e.g. "3h 12m left" or "2h 5m over". */
  label: string;
  breached: boolean;
}

const SOON_MS = 4 * 60 * 60 * 1000;

/** Compute a countdown against a due timestamp; null when nothing is due. */
export function slaCountdown(
  dueIso: string | null,
  flaggedBreach: boolean,
  doneAt: string | null,
  now = Date.now(),
): SlaCountdown | null {
  if (!dueIso) return null;
  const due = Date.parse(dueIso);
  if (Number.isNaN(due)) return null;
  // SLA satisfied (acked/resolved in time) — no countdown chip needed.
  if (doneAt) {
    const done = Date.parse(doneAt);
    if (!Number.isNaN(done) && done <= due && !flaggedBreach) return null;
  }
  const delta = due - now;
  const breached = flaggedBreach || delta < 0;
  const abs = Math.abs(delta);
  const hours = Math.floor(abs / 3600000);
  const mins = Math.floor((abs % 3600000) / 60000);
  const days = Math.floor(hours / 24);
  const span =
    days >= 1
      ? `${days}d ${hours % 24}h`
      : hours >= 1
        ? `${hours}h ${mins}m`
        : `${mins}m`;
  if (breached) return { tone: "breach", label: `${span} over`, breached: true };
  return {
    tone: abs <= SOON_MS ? "soon" : "ok",
    label: `${span} left`,
    breached: false,
  };
}

/** Mask a phone for list views / non-privileged roles (SPEC §2 anonymous rule). */
export function maskPhone(phone: string | null): string {
  if (!phone) return "—";
  const digits = phone.replace(/\D/g, "");
  if (digits.length <= 4) return "••••";
  return `••• ••• ${digits.slice(-4)}`;
}

/** Reporter display: anonymous reporters are masked unless owner/admin. */
export function reporterDisplay(
  c: Pick<CivicCase, "reporter_phone_e164" | "reporter_name" | "anonymous">,
  canReveal: boolean,
): { name: string; phone: string } {
  if (c.anonymous && !canReveal) {
    return { name: "Anonymous reporter", phone: maskPhone(c.reporter_phone_e164) };
  }
  return {
    name: c.reporter_name ?? (c.anonymous ? "Anonymous reporter" : "—"),
    phone: c.reporter_phone_e164 ?? "—",
  };
}
