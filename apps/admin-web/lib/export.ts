/**
 * Client-side export helpers (SPEC-W82 B1.5): CSV generation with correct
 * escaping and WhatsApp share URLs. The pure functions (csvEscape, toCSV,
 * waShareUrl) touch nothing browser-specific so they stay unit-testable;
 * only downloadCSV needs a DOM.
 */

/** One CSV column: header label + how to read the cell out of a row. */
export interface CsvColumn<T> {
  header: string;
  value: (row: T) => string | number | null | undefined;
}

/**
 * Escape one CSV cell per RFC 4180: cells containing a comma, double quote
 * or newline are wrapped in double quotes; embedded quotes are doubled.
 */
export function csvEscape(cell: string | number | null | undefined): string {
  const s = cell === null || cell === undefined ? "" : String(cell);
  if (/[",\r\n]/.test(s)) return `"${s.replace(/"/g, '""')}"`;
  return s;
}

/**
 * Build a CSV document (header row + one row per record, CRLF separated)
 * from already-fetched client state — no endpoint involved.
 */
export function toCSV<T>(rows: T[], columns: CsvColumn<T>[]): string {
  const header = columns.map((c) => csvEscape(c.header)).join(",");
  const body = rows.map((row) =>
    columns.map((c) => csvEscape(c.value(row))).join(","),
  );
  return [header, ...body].join("\r\n");
}

/** WhatsApp click-to-chat share URL with the message pre-filled. */
export function waShareUrl(text: string): string {
  return `https://wa.me/?text=${encodeURIComponent(text)}`;
}

/**
 * Trigger a browser download of a CSV string via Blob + anchor click.
 * No-op outside the browser (SSR safety).
 */
export function downloadCSV(filename: string, csv: string): void {
  if (typeof window === "undefined" || typeof document === "undefined") return;
  const blob = new Blob([csv], { type: "text/csv;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}
