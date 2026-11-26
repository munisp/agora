/**
 * 2026-11-26: pure helpers for sortable dashboard tables (components/ui/table.tsx). A cell's sort key comes from its
 * text as shown: dates (as formatDateTime / formatDate / toLocaleString print them, or ISO), numbers (money,
 * percentages, counts) and otherwise text. No DOM, no React -- unit-tested via tsc -> node --test
 * (scripts/test-table-sort.sh).
 */

export type SortKey = { kind: "num"; value: number } | { kind: "text"; value: string } | { kind: "empty" };
export type SortDir = "asc" | "desc";

const MONTHS: Record<string, number> = {
  jan: 0, feb: 1, mar: 2, apr: 3, may: 4, jun: 5, jul: 6, aug: 7, sep: 8, sept: 8, oct: 9, nov: 10, dec: 11,
};

function clock(h: string | undefined, m: string | undefined, ampm: string | undefined): [number, number] {
  if (h === undefined) return [0, 0];
  let hour = Number(h);
  const p = (ampm || "").toLowerCase();
  if (p === "pm" && hour < 12) hour += 12;
  if (p === "am" && hour === 12) hour = 0;
  return [hour, Number(m || 0)];
}

const TIME = String.raw`(?:,?\s+(?:at\s+)?(\d{1,2}):(\d{2})(?::\d{2})?\s*([AaPp][Mm])?)?`;
const MON_D_Y = new RegExp(String.raw`^([A-Za-z]{3,9})\.?\s+(\d{1,2}),?\s+(\d{4})` + TIME + "$");
const D_MON_Y = new RegExp(String.raw`^(\d{1,2})\s+([A-Za-z]{3,9})\.?,?\s+(\d{4})` + TIME + "$");
const M_D_Y = new RegExp(String.raw`^(\d{1,2})/(\d{1,2})/(\d{4})` + TIME + "$");
const ISO = /^\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?)?$/;
const TIME_ONLY = /^(\d{1,2}):(\d{2})\s*([AaPp][Mm])?$/;

/** Whether this browser prints numeric dates day first (en-GB, en-NG: 26/11/2026) -- toLocaleString() follows it. */
function localeDayFirst(): boolean {
  try {
    const parts = new Intl.DateTimeFormat().formatToParts(new Date(2026, 10, 26));
    const order = parts.filter((p) => p.type === "day" || p.type === "month").map((p) => p.type);
    return order[0] === "day";
  } catch {
    return false;
  }
}
const DAY_FIRST = localeDayFirst();

/** Milliseconds for a date as the dashboard prints it, else null. Local time; only the order matters. */
export function parseShownDate(text: string): number | null {
  const t = text.trim();
  if (ISO.test(t)) {
    const v = Date.parse(t.length === 10 ? `${t}T00:00:00` : t.replace(" ", "T"));
    return Number.isNaN(v) ? null : v;
  }
  let m = MON_D_Y.exec(t);
  if (m) {
    const mon = MONTHS[m[1].slice(0, m[1].toLowerCase().startsWith("sept") ? 4 : 3).toLowerCase()];
    if (mon === undefined) return null;
    const [h, mi] = clock(m[4], m[5], m[6]);
    return new Date(Number(m[3]), mon, Number(m[2]), h, mi).getTime();
  }
  m = D_MON_Y.exec(t);
  if (m) {
    const mon = MONTHS[m[2].slice(0, 3).toLowerCase()];
    if (mon === undefined) return null;
    const [h, mi] = clock(m[4], m[5], m[6]);
    return new Date(Number(m[3]), mon, Number(m[1]), h, mi).getTime();
  }
  m = M_D_Y.exec(t);
  if (m) {
    const [h, mi] = clock(m[4], m[5], m[6]);
    const [a, b] = [Number(m[1]), Number(m[2])];
    // A part over 12 can only be the day; otherwise the browser's own order (toLocaleString prints in it).
    const dayFirst = a > 12 ? true : b > 12 ? false : DAY_FIRST;
    const [day, month] = dayFirst ? [a, b] : [b, a];
    return new Date(Number(m[3]), month - 1, day, h, mi).getTime();
  }
  m = TIME_ONLY.exec(t);
  if (m) {
    const [h, mi] = clock(m[1], m[2], m[3]);
    return h * 60 + mi;
  }
  return null;
}

/** A number as shown: "₦12,500.00", "45%", "-3", "1.2k" is left as text. */
export function parseShownNumber(text: string): number | null {
  const t = text.trim();
  const m = /^(?:[A-Z]{3}\s?|[^\d\s.,-]{1,3}\s?)?(-?\d[\d,]*(?:\.\d+)?)\s*%?$/.exec(t);
  if (!m) return null;
  const v = Number(m[1].replace(/,/g, ""));
  return Number.isFinite(v) ? v : null;
}

const EMPTY = new Set(["", "—", "-", "–", "n/a", "none"]);

export function sortKeyOf(value: unknown): SortKey {
  if (value === null || value === undefined) return { kind: "empty" };
  if (typeof value === "number") return Number.isFinite(value) ? { kind: "num", value } : { kind: "empty" };
  if (value instanceof Date) return { kind: "num", value: value.getTime() };
  const text = String(value).replace(/\s+/g, " ").trim();
  if (EMPTY.has(text.toLowerCase())) return { kind: "empty" };
  const d = parseShownDate(text);
  if (d !== null) return { kind: "num", value: d };
  const n = parseShownNumber(text);
  if (n !== null) return { kind: "num", value: n };
  return { kind: "text", value: text.toLowerCase() };
}

/** Order two keys; empties always last whatever the direction. */
export function compareKeys(a: SortKey, b: SortKey, dir: SortDir): number {
  if (a.kind === "empty" || b.kind === "empty") {
    return a.kind === b.kind ? 0 : a.kind === "empty" ? 1 : -1;
  }
  let c: number;
  if (a.kind === "num" && b.kind === "num") c = a.value - b.value;
  else if (a.kind === "text" && b.kind === "text") c = a.value.localeCompare(b.value, undefined, { numeric: true });
  else c = a.kind === "num" ? -1 : 1; // numbers before text
  return dir === "asc" ? c : -c;
}

/** Stable sort of row indexes by key. */
export function sortedOrder(keys: SortKey[], dir: SortDir): number[] {
  return keys
    .map((k, i) => i)
    .sort((i, j) => compareKeys(keys[i], keys[j], dir) || i - j);
}

/** Header names that mean "when" -- the column a table starts sorted on, latest first. */
const WHEN_HEADER = new RegExp(
  String.raw`\b(when|date|dated|time|at|created|updated|modified|last|received|sent|submitted|opened|joined|added|` +
    String.raw`started|filed|issued|occurred|scheduled|requested|posted|logged|seen|captured|reported|booked|paid|` +
    String.raw`generated|resolved|closed|completed|uploaded|shared|delivered|published|signed|registered|enrolled|` +
    String.raw`applied|approved|called|renewed|expires|expiry|due)\b`,
  "i",
);

export function isWhenHeader(label: string): boolean {
  return WHEN_HEADER.test(label.trim());
}

/** True when most non-empty keys of a column are dates/numbers (so "latest first" means something). */
export function mostlyDates(texts: string[]): boolean {
  const shown = texts.map((t) => t.trim()).filter((t) => t && !EMPTY.has(t.toLowerCase()));
  if (shown.length === 0) return false;
  const dated = shown.filter((t) => parseShownDate(t) !== null).length;
  return dated / shown.length >= 0.8;
}
