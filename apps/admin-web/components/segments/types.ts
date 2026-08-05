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
 * Audience handoff (this workstream's notification-worker intake, reached
 * through the existing /api/notifications/* gateway mount):
 *
 *   POST   /api/notifications/v1/audiences              {segment_id,
 *          campaign_id, message, channel} + Idempotency-Key: campaign_id
 */

export const GRAPH_API = "/api/graph/v1/graph";
export const AUDIENCES_API = "/api/notifications/v1/audiences";

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
  return dsl;
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

/** Normalize the person-360 payload (each relation list tolerant-unwrapped). */
export function normalizePerson360(data: unknown): Person360 {
  const obj = (typeof data === "object" && data !== null ? data : {}) as Record<
    string,
    unknown
  >;
  const str = (k: string) => (obj[k] == null ? undefined : String(obj[k]));
  return {
    person_id: str("person_id") ?? str("id") ?? "",
    name: str("name"),
    phone_hash: str("phone_hash"),
    channels: Array.isArray(obj.channels) ? (obj.channels as string[]) : undefined,
    consent_summary: str("consent_summary"),
    quarantine: obj.quarantine === true,
    contacts: unwrapList<PersonContact>(obj.contacts),
    bookings: unwrapList<PersonBooking>(obj.bookings),
    consents: unwrapList<PersonConsent>(obj.consents),
    referrals: unwrapList<PersonReferral>(obj.referrals),
    messages: unwrapList<PersonMessage>(obj.messages),
  };
}
