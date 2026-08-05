/**
 * SPEC-W28 WS-C: shared types + API paths for the Segment Builder, Ask box
 * and Person 360 views.
 *
 * Backend contracts (Agent B's graph-service, reached through the APISIX
 * gateway mount /api/graph/* — the BFF attaches the tenant headers):
 *
 *   POST   /api/graph/v1/graph/segments                 DSL → save
 *   GET    /api/graph/v1/graph/segments                 list
 *   GET    /api/graph/v1/graph/segments/{id}/count      consent-passing count
 *   POST   /api/graph/v1/graph/segments/count           DSL → preview count
 *                                                       (unsaved; dry-run
 *                                                       fallback: POST
 *                                                       /segments?dry_run=true)
 *   POST   /api/graph/v1/graph/ask                      NL → Cypher answer
 *   GET    /api/graph/v1/graph/persons/{id}             person 360
 *
 * SPEC-W29 WS-C (predictive layer):
 *
 *   POST   /api/graph/v1/graph/cypher                   template-allowlisted
 *          {template, params}                            read-only queries;
 *          W29 templates: next_best_services {person_id},
 *          similar_persons {person_id, k}
 *
 *   The segment DSL gains an optional `score_filters` block (SPEC-W29 §3
 *   WS-B): numeric filters over the per-person propensity/risk scores,
 *   compiled server-side to parameterized WHERE clauses. Unknown field or
 *   malformed value → 422; validateScoreFilterRows mirrors those rules
 *   client-side so the builder never submits a DSL the server will reject.
 *
 * Audience handoff (this workstream's notification-worker intake, reached
 * through the existing /api/notifications/* gateway mount):
 *
 *   POST   /api/notifications/v1/audiences              {segment_id,
 *          campaign_id, message, channel} + Idempotency-Key: campaign_id
 */

export const GRAPH_API = "/api/graph/v1/graph";
export const AUDIENCES_API = "/api/notifications/v1/audiences";

/**
 * Agora brand tokens (SPEC-W29/W30 WS-C brief): low-saturation warm palette.
 * The score/severity ramps run sage → amber → terracotta — never red/blue.
 * Shared with components/alerts (single source of truth).
 */
export const BRAND = {
  cream: "#FAF6F0",
  ink: "#2B2118",
  terracotta: "#C0562F",
  amber: "#D99A4E",
  sage: "#7A8B6F",
} as const;

/** Numeric Person score fields filterable in the segment DSL (SPEC-W29 §3 WS-B). */
export const SCORE_FILTER_FIELDS = [
  { value: "propensity_churn", label: "Churn propensity" },
  { value: "propensity_convert", label: "Convert propensity" },
  { value: "propensity_turnout", label: "Turnout propensity" },
  { value: "risk_score", label: "Fraud risk score" },
] as const;

export type ScoreFilterField = (typeof SCORE_FILTER_FIELDS)[number]["value"];

export type ScoreFilterOp = ">=" | "<=" | "between";

/**
 * One numeric score filter of the DSL block (SPEC-W29 §3 WS-B):
 * `{"field": ..., "op": ">="|"<="|"between", "value": float | [lo, hi]}`.
 * Compiled server-side to a parameterized WHERE clause ($sfN binding).
 */
export interface ScoreFilter {
  field: ScoreFilterField;
  op: ScoreFilterOp;
  value: number | [number, number];
}

/** Declarative segment DSL (SPEC-W28 §4 WS-B). Sent verbatim to graph-service. */
export interface SegmentDSL {
  /** Purpose of the required CONSENTED edge (SPEC-W12 purposes; "marketing" default). */
  has_consent: string;
  /** ISO date — only Persons whose last booking is before this date. */
  last_booking_before?: string;
  /** Lagos LGA of the capture Location. */
  lga?: string;
  /** Exclude Persons messaged within this many days (any campaign). */
  not_messaged_since_days?: number;
  /**
   * SPEC-W29: optional numeric score filters over Person.propensity_* /
   * Person.risk_score. Scores rank/filter *within* the already
   * consent-eligible population — they never widen it (SPEC-W29 §0.3).
   */
  score_filters?: ScoreFilter[];
  /**
   * FIXED false (SPEC-W28 §5 gate 4): quarantined (imported,
   * consent-unverified) Persons are query-visible but audience-ineligible.
   * The field exists in the DSL so the exclusion is explicit and auditable.
   */
  include_quarantined: false;
}

export interface Segment extends SegmentDSL {
  id: string;
  name: string;
  created_at?: string;
  /** Present when the list endpoint pre-computes it. */
  count?: number;
}

export interface AskAnswer {
  /** Answer rows (tolerant decode: first array property of the response). */
  rows: Record<string, unknown>[];
  /** Generated read-only Cypher shown in the collapsible view. */
  cypher: string;
  /** Optional NL summary sentence from the model. */
  summary?: string;
}

export interface PersonContact {
  lead_id?: string;
  channel_of_first_touch?: string;
  captured_at?: string;
  source?: string;
  geo?: string;
}

export interface PersonBooking {
  booking_id?: string;
  status?: string;
  offering?: string;
  offering_id?: string;
  created_at?: string;
  showed?: boolean;
}

export interface PersonConsent {
  consent_id?: string;
  purpose?: string;
  granted_at?: string;
  revoked_at?: string;
}

export interface PersonReferral {
  person_id?: string;
  name?: string;
  direction?: "out" | "in";
  program?: string;
  at?: string;
}

export interface PersonMessage {
  campaign_id?: string;
  at?: string;
  status?: string;
  channel?: string;
}

export interface Person360 {
  person_id: string;
  name?: string;
  phone_hash?: string;
  channels?: string[];
  consent_summary?: string;
  quarantine?: boolean;
  contacts: PersonContact[];
  bookings: PersonBooking[];
  consents: PersonConsent[];
  referrals: PersonReferral[];
  messages: PersonMessage[];
  /** SPEC-W29 §2: heuristic/GNN propensity scores, all 0..1, all optional
   * (absent until the first scoring sweep has run for the tenant). */
  propensity_churn?: number;
  propensity_convert?: number;
  propensity_turnout?: number;
  /** SPEC-W30 §2: fraud risk score + active detector flags. */
  risk_score?: number;
  risk_flags?: string[];
  /** Provenance of the scores above (SPEC-W29 §0.4). */
  model_version?: string;
  scored_at?: string;
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

/** Tolerant scalar count decode: {count} | {total} | {consenting_count} | number. */
export function unwrapCount(data: unknown): number | null {
  if (typeof data === "number" && Number.isFinite(data)) return data;
  if (typeof data === "object" && data !== null) {
    for (const key of ["count", "total", "consenting_count", "audience_size"]) {
      const v = (data as Record<string, unknown>)[key];
      if (typeof v === "number" && Number.isFinite(v)) return v;
    }
  }
  return null;
}

/** Build the DSL payload from the builder form state (empty fields omitted). */
export function buildDSL(input: {
  hasConsent: string;
  lastBookingBefore: string;
  lga: string;
  notMessagedSinceDays: string;
  /** Already-validated score filter rows (validateScoreFilterRows first). */
  scoreFilters?: ScoreFilter[];
}): SegmentDSL {
  const dsl: SegmentDSL = {
    has_consent: input.hasConsent,
    include_quarantined: false,
  };
  if (input.lastBookingBefore) dsl.last_booking_before = input.lastBookingBefore;
  const lga = input.lga.trim();
  if (lga) dsl.lga = lga;
  const days = Number(input.notMessagedSinceDays);
  if (input.notMessagedSinceDays.trim() !== "" && Number.isFinite(days) && days >= 0) {
    dsl.not_messaged_since_days = Math.floor(days);
  }
  if (input.scoreFilters && input.scoreFilters.length > 0) {
    dsl.score_filters = input.scoreFilters;
  }
  return dsl;
}

/* ------------------------------------------------------------------ *
 * SPEC-W29 WS-C: score-filter validation + predictive template types  *
 * ------------------------------------------------------------------ */

/** Editable score-filter row state of the builder (string inputs). */
export interface ScoreFilterRow {
  /** Local row id for React keys. */
  id: string;
  field: ScoreFilterField;
  op: ScoreFilterOp;
  /** Raw input for ">=" / "<=" ops. */
  value: string;
  /** Raw inputs for the "between" op. */
  valueLo: string;
  valueHi: string;
}

export function emptyScoreFilterRow(id: string): ScoreFilterRow {
  return { id, field: "propensity_churn", op: ">=", value: "", valueLo: "", valueHi: "" };
}

const SCORE_FIELDS: readonly string[] = SCORE_FILTER_FIELDS.map((f) => f.value);

function parseScore(raw: string): number | null {
  const n = Number(raw.trim());
  if (raw.trim() === "" || !Number.isFinite(n)) return null;
  return n;
}

/**
 * Client-side mirror of the graph-service 422 rules for the score_filters
 * DSL block (SPEC-W29 §3 WS-B): unknown field, unknown op, non-numeric or
 * out-of-range value, and malformed [lo, hi] ranges are rejected before the
 * DSL ever leaves the browser. Scores are defined on 0..1 (SPEC-W29 §2,
 * SPEC-W30 §2), so values outside that interval are rejected too.
 *
 * Returns `{ filters, errors }` — `errors` is keyed by row id; a row with an
 * error is excluded from `filters` so the preview count can still run.
 */
export function validateScoreFilterRows(rows: ScoreFilterRow[]): {
  filters: ScoreFilter[];
  errors: Record<string, string>;
} {
  const filters: ScoreFilter[] = [];
  const errors: Record<string, string> = {};
  for (const row of rows) {
    if (!SCORE_FIELDS.includes(row.field)) {
      errors[row.id] = "Unknown score field.";
      continue;
    }
    if (row.op === "between") {
      const lo = parseScore(row.valueLo);
      const hi = parseScore(row.valueHi);
      if (lo === null || hi === null) {
        errors[row.id] = "Between needs two numeric bounds.";
        continue;
      }
      if (lo < 0 || hi > 1) {
        errors[row.id] = "Bounds must lie between 0 and 1.";
        continue;
      }
      if (lo > hi) {
        errors[row.id] = "Lower bound must not exceed the upper bound.";
        continue;
      }
      filters.push({ field: row.field, op: "between", value: [lo, hi] });
    } else if (row.op === ">=" || row.op === "<=") {
      const v = parseScore(row.value);
      if (v === null) {
        errors[row.id] = "Enter a numeric score (0–1).";
        continue;
      }
      if (v < 0 || v > 1) {
        errors[row.id] = "Score must lie between 0 and 1.";
        continue;
      }
      filters.push({ field: row.field, op: row.op, value: v });
    } else {
      errors[row.id] = "Unknown operator.";
    }
  }
  return { filters, errors };
}

/** One ranked RECOMMENDED_FOR edge row of the next_best_services template. */
export interface ServiceRecommendation {
  offering_id: string;
  offering_name?: string;
  /** 0..1 — rendered as the score bar width. */
  score: number;
  rank: number;
  /** Machine-ish reason string from the scorer ("booked_cleaning_2x"). */
  reason?: string;
  model_version?: string;
  scored_at?: string;
}

/** One row of the similar_persons template (cosine over stored embeddings). */
export interface SimilarPerson {
  person_id: string;
  name?: string;
  similarity?: number;
}

function num(v: unknown): number | undefined {
  const n = typeof v === "string" ? Number(v) : v;
  return typeof n === "number" && Number.isFinite(n) ? n : undefined;
}

function str(v: unknown): string | undefined {
  return v === null || v === undefined ? undefined : String(v);
}

/** Tolerant row decode for the next_best_services template response. */
export function normalizeRecommendation(
  raw: Record<string, unknown>,
): ServiceRecommendation {
  return {
    offering_id:
      str(raw.offering_id) ?? str(raw.offering) ?? str(raw.id) ?? "",
    offering_name:
      str(raw.offering_name) ?? str(raw.name) ?? str(raw.title) ??
      // Some renders return the offering label in `offering` alongside an id.
      (raw.offering_id !== undefined ? str(raw.offering) : undefined),
    score: num(raw.score) ?? 0,
    rank: num(raw.rank) ?? 0,
    reason: str(raw.reason),
    model_version: str(raw.model_version),
    scored_at: str(raw.scored_at),
  };
}

/** Tolerant row decode for the similar_persons template response. */
export function normalizeSimilarPerson(
  raw: Record<string, unknown>,
): SimilarPerson {
  return {
    person_id: str(raw.person_id) ?? str(raw.id) ?? "",
    name: str(raw.name),
    similarity: num(raw.similarity) ?? num(raw.score),
  };
}

/** Humanize a scorer reason string ("booked_cleaning_2x" → "Booked cleaning 2x"). */
export function humanizeReason(reason: string): string {
  const text = reason.replace(/[_-]+/g, " ").replace(/\s+/g, " ").trim();
  return text ? text.charAt(0).toUpperCase() + text.slice(1) : text;
}

/** Normalize a raw saved-segment row into Segment (id/name key tolerance). */
export function normalizeSegment(raw: Record<string, unknown>): Segment {
  const { id, segment_id, name, created_at, count, ...dsl } = raw;
  const typed = dsl as unknown as SegmentDSL;
  if (typeof typed.has_consent !== "string" || typed.has_consent === "") {
    typed.has_consent = "marketing";
  }
  return {
    ...typed,
    id: String(id ?? segment_id ?? ""),
    name: String(name ?? "Untitled segment"),
    created_at: created_at ? String(created_at) : undefined,
    count: typeof count === "number" ? count : undefined,
    include_quarantined: false,
  };
}

/**
 * Normalize the person-360 payload.
 *
 * GET /v1/graph/persons/{id} returns an ENVELOPE (graph-service person_360
 * handler): identity lives in `body.person` (one row of the person_by_id
 * projection — person_id, name, quarantine, and per Agent C's W29/W30
 * extension propensity_* / risk_score / risk_flags / model_version /
 * scored_at), while the relation lists are top-level keys
 * (`body.contacts`, `body.bookings`, …). Identity/score reads therefore
 * prefer `body.person` and fall back to the top level (tolerance for a
 * flat response); relation lists prefer the top level and fall back to
 * `body.person`.
 */
export function normalizePerson360(data: unknown): Person360 {
  const envelope = (typeof data === "object" && data !== null ? data : {}) as Record<
    string,
    unknown
  >;
  const person =
    typeof envelope.person === "object" && envelope.person !== null
      ? (envelope.person as Record<string, unknown>)
      : envelope;
  // Scalar identity + score fields: person projection first, envelope fallback.
  const pick = (k: string) => (person[k] != null ? person[k] : envelope[k]);
  const str = (k: string) => (pick(k) == null ? undefined : String(pick(k)));
  const numProp = (k: string) => {
    const raw = pick(k);
    const n = typeof raw === "string" ? Number(raw) : raw;
    return typeof n === "number" && Number.isFinite(n) ? n : undefined;
  };
  // Relation lists: envelope first (their real location), person fallback.
  const list = <T>(k: string) =>
    unwrapList<T>(envelope[k] !== undefined ? envelope[k] : person[k]);
  const flags = pick("risk_flags");
  return {
    person_id: str("person_id") ?? str("id") ?? "",
    name: str("name"),
    phone_hash: str("phone_hash"),
    channels: Array.isArray(pick("channels"))
      ? (pick("channels") as unknown[]).map(String)
      : undefined,
    consent_summary: str("consent_summary"),
    quarantine: pick("quarantine") === true,
    contacts: list<PersonContact>("contacts"),
    bookings: list<PersonBooking>("bookings"),
    consents: list<PersonConsent>("consents"),
    referrals: list<PersonReferral>("referrals"),
    messages: list<PersonMessage>("messages"),
    // SPEC-W29/W30 score properties — absent until the scoring sweep runs.
    propensity_churn: numProp("propensity_churn"),
    propensity_convert: numProp("propensity_convert"),
    propensity_turnout: numProp("propensity_turnout"),
    risk_score: numProp("risk_score"),
    risk_flags: Array.isArray(flags) ? flags.map(String) : undefined,
    model_version: str("model_version"),
    scored_at: str("scored_at"),
  };
}
