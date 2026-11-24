/**
 * SPEC-W117 §4: pure helpers for OpenSearch-backed advanced search bars.
 * No DOM, no fetch — unit-tested via tsc -> node --test
 * (scripts/test-advanced-search.sh, W114 idiom).
 */

export interface SearchState {
  q: string;
  channel: string;
  role: string;
  fromTs: string; // yyyy-mm-dd (date input) or ""
  toTs: string;
  limit: number;
  offset: number;
}

export const SEARCH_CHANNELS = ["web", "whatsapp", "telegram", "voice", "ussd"] as const;
export const SEARCH_ROLES = ["user", "agent", "tool", "system"] as const;
export const SEARCH_PAGE_SIZE = 25;
export const SEARCH_MAX_LIMIT = 100;
export const SEARCH_MAX_WINDOW = 10000;

export const EMPTY_SEARCH_STATE: SearchState = {
  q: "",
  channel: "",
  role: "",
  fromTs: "",
  toTs: "",
  limit: SEARCH_PAGE_SIZE,
  offset: 0,
};

export function clampLimit(limit: number): number {
  if (!Number.isFinite(limit) || limit < 1) return 1;
  return Math.min(Math.floor(limit), SEARCH_MAX_LIMIT);
}

/** Omits empty filters; dates become inclusive ISO bounds (start of
 * from-day, end of to-day in UTC). */
export function buildSearchParams(state: SearchState): URLSearchParams {
  const params = new URLSearchParams();
  const q = state.q.trim();
  if (q) params.set("q", q);
  if (state.channel) params.set("channel", state.channel);
  if (state.role) params.set("role", state.role);
  if (state.fromTs) params.set("from_ts", `${state.fromTs}T00:00:00Z`);
  if (state.toTs) params.set("to_ts", `${state.toTs}T23:59:59Z`);
  params.set("limit", String(clampLimit(state.limit)));
  params.set("offset", String(Math.max(0, Math.floor(state.offset))));
  return params;
}

/** Returns an error string, or null when the range is valid. */
export function validateDateRange(fromTs: string, toTs: string): string | null {
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

export interface Chip {
  key: string;
  label: string;
}

/** Human-readable active-filter chips for the bar. */
export function describeChips(state: SearchState): Chip[] {
  const chips: Chip[] = [];
  if (state.q.trim()) chips.push({ key: "q", label: `"${state.q.trim()}"` });
  if (state.channel) chips.push({ key: "channel", label: `Channel: ${state.channel}` });
  if (state.role) chips.push({ key: "role", label: `Role: ${state.role}` });
  if (state.fromTs) chips.push({ key: "fromTs", label: `From: ${state.fromTs}` });
  if (state.toTs) chips.push({ key: "toTs", label: `To: ${state.toTs}` });
  return chips;
}

/** Next/prev page offsets, clamped at 0 and the result window. */
export function nextOffset(state: SearchState, total: number): number | null {
  const next = state.offset + clampLimit(state.limit);
  if (next >= total || next >= SEARCH_MAX_WINDOW) return null;
  return next;
}

export function prevOffset(state: SearchState): number | null {
  if (state.offset <= 0) return null;
  return Math.max(0, state.offset - clampLimit(state.limit));
}
