/**
 * SPEC-W119 §4: pure helpers for the bookings/contacts/leads entity-search
 * panels (OpenSearch via booking-service /v1/{entity}/search). No DOM, no
 * fetch — unit-tested via tsc -> node --test (scripts/test-entity-search.sh,
 * W114 idiom; mirrors lib/advanced-search.ts semantics).
 */

export interface EntitySelectOption {
  value: string;
  label: string;
}

export interface EntitySelect {
  key: string; // query param name (e.g. "status", "source", "channel")
  label: string;
  options: EntitySelectOption[];
}

export interface EntityDescriptor {
  entity: "bookings" | "contacts" | "leads";
  qPlaceholder: string;
  selects: EntitySelect[];
  /** Date-range filter field; null when the entity has no timestamps
   *  (contacts) — date inputs are then hidden and dates never sent. */
  dateField: string | null;
}

export interface EntitySearchState {
  q: string;
  filters: Record<string, string>;
  fromTs: string; // yyyy-mm-dd (date input) or ""
  toTs: string;
  limit: number;
  offset: number;
}

export const ENTITY_PAGE_SIZE = 25;
export const ENTITY_MAX_LIMIT = 100;
export const ENTITY_MAX_WINDOW = 10000;

export function emptyEntitySearchState(): EntitySearchState {
  return { q: "", filters: {}, fromTs: "", toTs: "", limit: ENTITY_PAGE_SIZE, offset: 0 };
}

export function clampEntityLimit(limit: number): number {
  if (!Number.isFinite(limit) || limit < 1) return 1;
  return Math.min(Math.floor(limit), ENTITY_MAX_LIMIT);
}

/** Builds the query string for /v1/{entity}/search. The descriptor is an
 *  allow-list: filter keys not declared in descriptor.selects are ignored,
 *  and dates are only emitted when descriptor.dateField is set. Dates
 *  become inclusive ISO day bounds (UTC). */
export function buildEntitySearchParams(
  state: EntitySearchState,
  descriptor: EntityDescriptor,
): URLSearchParams {
  const params = new URLSearchParams();
  const q = state.q.trim();
  if (q) params.set("q", q);
  for (const sel of descriptor.selects) {
    const value = state.filters[sel.key];
    if (value) params.set(sel.key, value);
  }
  if (descriptor.dateField) {
    if (state.fromTs) params.set("from_ts", `${state.fromTs}T00:00:00Z`);
    if (state.toTs) params.set("to_ts", `${state.toTs}T23:59:59Z`);
  }
  params.set("limit", String(clampEntityLimit(state.limit)));
  params.set("offset", String(Math.max(0, Math.floor(state.offset))));
  return params;
}

/** Returns an error string, or null when the range is valid. Descriptors
 *  without a date field never produce an error. */
export function validateEntityDateRange(
  descriptor: EntityDescriptor,
  fromTs: string,
  toTs: string,
): string | null {
  if (!descriptor.dateField) return null;
  if (fromTs && !/^\d{4}-\d{2}-\d{2}$/.test(fromTs)) {
    return "Start date must be yyyy-mm-dd.";
  }
  if (toTs && !/^\d{4}-\d{2}-\d{2}$/.test(toTs)) {
    return "End date must be yyyy-mm-dd.";
  }
  if (fromTs && toTs && fromTs > toTs) {
    return "Start date must not be after the end date.";
  }
  return null;
}

export interface EntityChip {
  key: string; // "q" | select key | "fromTs" | "toTs"
  label: string;
}

/** Human-readable active-filter chips for the bar. */
export function describeEntityChips(
  state: EntitySearchState,
  descriptor: EntityDescriptor,
): EntityChip[] {
  const chips: EntityChip[] = [];
  if (state.q.trim()) chips.push({ key: "q", label: `"${state.q.trim()}"` });
  for (const sel of descriptor.selects) {
    const value = state.filters[sel.key];
    if (value) chips.push({ key: sel.key, label: `${sel.label}: ${value}` });
  }
  if (descriptor.dateField) {
    if (state.fromTs) chips.push({ key: "fromTs", label: `From: ${state.fromTs}` });
    if (state.toTs) chips.push({ key: "toTs", label: `To: ${state.toTs}` });
  }
  return chips;
}

/** Returns the next state with one chip cleared. */
export function clearEntityChip(
  state: EntitySearchState,
  descriptor: EntityDescriptor,
  key: string,
): EntitySearchState {
  if (key === "q") return { ...state, q: "" };
  if (key === "fromTs" && descriptor.dateField) return { ...state, fromTs: "" };
  if (key === "toTs" && descriptor.dateField) return { ...state, toTs: "" };
  if (descriptor.selects.some((s) => s.key === key)) {
    return { ...state, filters: { ...state.filters, [key]: "" } };
  }
  return state;
}

/** Next/prev page offsets, clamped at 0 and the result window. */
export function nextEntityOffset(state: EntitySearchState, total: number): number | null {
  const next = state.offset + clampEntityLimit(state.limit);
  if (next >= total || next >= ENTITY_MAX_WINDOW) return null;
  return next;
}

export function prevEntityOffset(state: EntitySearchState): number | null {
  if (state.offset <= 0) return null;
  return Math.max(0, state.offset - clampEntityLimit(state.limit));
}

// ---------------------------------------------------------------------------
// Entity descriptors (option lists mirror the backend CHECK constraints /
// known value sets; unknown values would simply match nothing).
// ---------------------------------------------------------------------------

export const BOOKING_STATUS_OPTIONS = [
  "pending",
  "confirmed",
  "rescheduled",
  "cancelled",
  "no_show",
  "completed",
] as const;

export const BOOKING_SOURCE_OPTIONS = ["voice", "chat", "web", "api"] as const;

export const LEAD_STATUS_OPTIONS = [
  "new",
  "contacted",
  "qualified",
  "converted",
  "lost",
] as const;

export const LEAD_CHANNEL_OPTIONS = [
  "voice",
  "whatsapp",
  "telegram",
  "web",
  "sms",
  "webhook",
  "ussd",
  "qr",
  "promo",
  "field",
] as const;

export const CONTACT_SOURCE_OPTIONS = ["manual", "booking", "crm", "twenty", "import"] as const;

function toOptions(values: readonly string[]): EntitySelectOption[] {
  return values.map((v) => ({ value: v, label: v.replace(/_/g, " ") }));
}

export const BOOKINGS_DESCRIPTOR: EntityDescriptor = {
  entity: "bookings",
  qPlaceholder: "Contact name or booking reference…",
  selects: [
    { key: "status", label: "Status", options: toOptions(BOOKING_STATUS_OPTIONS) },
    { key: "source", label: "Source", options: toOptions(BOOKING_SOURCE_OPTIONS) },
  ],
  dateField: "starts_at",
};

export const CONTACTS_DESCRIPTOR: EntityDescriptor = {
  entity: "contacts",
  qPlaceholder: "Name, email or notes…",
  selects: [{ key: "source", label: "Source", options: toOptions(CONTACT_SOURCE_OPTIONS) }],
  dateField: null, // the contacts table has no timestamp columns
};

export const LEADS_DESCRIPTOR: EntityDescriptor = {
  entity: "leads",
  qPlaceholder: "Phone number or promo code…",
  selects: [
    { key: "status", label: "Status", options: toOptions(LEAD_STATUS_OPTIONS) },
    { key: "channel", label: "Channel", options: toOptions(LEAD_CHANNEL_OPTIONS) },
  ],
  dateField: "created_at",
};
