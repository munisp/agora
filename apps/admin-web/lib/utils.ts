import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

/** Agora is Nigeria-first: amounts default to naira and times to Lagos. A fixed zone also keeps the server render and
 * the browser render identical (an unset zone is UTC on the server but local time in the browser: React #418). */
export const DEFAULT_CURRENCY = "NGN";
export const DEFAULT_TIME_ZONE = "Africa/Lagos";

/** Format cents as a localised currency string. */
export function formatMoney(cents: number, currency = DEFAULT_CURRENCY, locale = "en-NG") {
  return new Intl.NumberFormat(locale, {
    style: "currency",
    currency,
  }).format(cents / 100);
}

export function formatDateTime(iso: string, locale = "en-US", timeZone: string = DEFAULT_TIME_ZONE) {
  return new Intl.DateTimeFormat(locale, {
    dateStyle: "medium",
    timeStyle: "short",
    timeZone,
  }).format(new Date(iso));
}

export function formatTime(iso: string, locale = "en-US", timeZone: string = DEFAULT_TIME_ZONE) {
  return new Intl.DateTimeFormat(locale, {
    hour: "numeric",
    minute: "2-digit",
    timeZone,
  }).format(new Date(iso));
}

export function formatDate(iso: string, locale = "en-US", timeZone: string = DEFAULT_TIME_ZONE) {
  return new Intl.DateTimeFormat(locale, {
    dateStyle: "full",
    timeZone,
  }).format(new Date(iso));
}

/** minutes-after-midnight -> "9:30 AM" */
export function minutesToTime(min: number) {
  const h = Math.floor(min / 60);
  const m = min % 60;
  const ampm = h >= 12 ? "PM" : "AM";
  const hr = h % 12 === 0 ? 12 : h % 12;
  return `${hr}:${m.toString().padStart(2, "0")} ${ampm}`;
}

/** "9:30" | "09:30" | "9:30 AM" -> minutes after midnight, or null */
export function timeToMinutes(value: string): number | null {
  const m = value.trim().match(/^(\d{1,2}):(\d{2})\s*(AM|PM)?$/i);
  if (!m) return null;
  let h = parseInt(m[1], 10);
  const min = parseInt(m[2], 10);
  if (min > 59 || h > 24) return null;
  const ampm = m[3]?.toUpperCase();
  if (ampm === "PM" && h < 12) h += 12;
  if (ampm === "AM" && h === 12) h = 0;
  if (!ampm && h === 24) h = 0;
  return h * 60 + min;
}

export const WEEKDAYS = [
  "Sunday",
  "Monday",
  "Tuesday",
  "Wednesday",
  "Thursday",
  "Friday",
  "Saturday",
] as const;

/** YYYY-MM-DD in local time (for availability date params). */
export function toISODate(d: Date) {
  const y = d.getFullYear();
  const m = (d.getMonth() + 1).toString().padStart(2, "0");
  const day = d.getDate().toString().padStart(2, "0");
  return `${y}-${m}-${day}`;
}

export function addDays(d: Date, n: number) {
  const c = new Date(d);
  c.setDate(c.getDate() + n);
  return c;
}

export function titleCase(s: string) {
  return s.replace(/_/g, " ").replace(/\b\w/g, (c) => c.toUpperCase());
}

/**
 * The list in an API response: a bare array, `{items: [...]}`, or one named list such as booking-service's
 * `{offerings: [...]}` / `{bookings: [...]}` (null or missing lists give []).
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any -- callers type the result through their state
export function listFrom<T = any>(data: unknown): T[] {
  if (Array.isArray(data)) return data as T[];
  if (typeof data !== "object" || data === null) return [];
  const items = (data as { items?: unknown }).items;
  if (Array.isArray(items)) return items as T[];
  const lists = Object.values(data as Record<string, unknown>).filter((v) => Array.isArray(v));
  return lists.length === 1 ? (lists[0] as T[]) : [];
}
