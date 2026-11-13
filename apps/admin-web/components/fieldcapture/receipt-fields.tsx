/**
 * SPEC-W53 Scope C: display-only surface for receipt/invoice OCR results.
 *
 * Scope B (booking-service fieldcapture) stores the ocr-service extraction on
 * a field capture as `receipt_fields jsonb` ({vendor, amount, currency, date,
 * items}) plus `receipt_document_id`. This module renders that payload
 * wherever a capture row is shown (today: the Person-360 "Lead captures"
 * surface).
 *
 * SPEC-W57: `currency` was always extracted by the ocr-service and stored
 * verbatim — only the parse/display sites dropped it. It is now parsed and
 * rendered next to the amount, defensively optional (absent on receipts
 * uploaded against older/mock OCR builds).
 *
 * Everything is defensively optional: older API responses carry neither
 * field, the extraction itself is tolerant (any sub-field may be null), and
 * unknown shapes degrade to a plain marker instead of breaking the page.
 *
 * There is deliberately NO upload affordance here — staff receipt upload is
 * the fieldcapture API surface this wave (POST /v1/field/captures/{id}/receipt);
 * an admin-web upload UI is a later-wave decision.
 */
import { ScanText } from "lucide-react";
import { Badge } from "@/components/ui/badge";

/** Loose view of the ocr-service receipt extraction contract. */
export interface ReceiptFieldsData {
  vendor?: string | null;
  amount?: number | string | null;
  /** ISO-ish currency code as read off the paper by OCR (e.g. "NGN"). */
  currency?: string | null;
  date?: string | null;
  items?: ReceiptItem[] | null;
}

export interface ReceiptItem {
  name?: string | null;
  quantity?: number | string | null;
  price?: number | string | null;
  amount?: number | string | null;
}

function asText(v: unknown): string | null {
  if (v === null || v === undefined) return null;
  if (typeof v === "number" && Number.isFinite(v)) return String(v);
  if (typeof v === "string" && v.trim() !== "") return v.trim();
  return null;
}

function normalizeItem(v: unknown): ReceiptItem | null {
  if (typeof v === "string" && v.trim() !== "") return { name: v.trim() };
  if (typeof v !== "object" || v === null) return null;
  const o = v as Record<string, unknown>;
  const item: ReceiptItem = {
    name: asText(o.name ?? o.description ?? o.item),
    quantity: asText(o.quantity ?? o.qty),
    price: asText(o.price ?? o.unit_price),
    amount: asText(o.amount ?? o.total ?? o.line_total),
  };
  return item.name || item.quantity || item.price || item.amount ? item : null;
}

/**
 * Tolerant decode of a `receipt_fields` payload. Returns null when nothing
 * renderable is present so callers can fall back to the plain marker/dash.
 */
export function parseReceiptFields(v: unknown): ReceiptFieldsData | null {
  if (typeof v !== "object" || v === null) return null;
  const o = v as Record<string, unknown>;
  const rawItems = Array.isArray(o.items) ? o.items : [];
  const items = rawItems
    .map(normalizeItem)
    .filter((i): i is ReceiptItem => i !== null);
  const data: ReceiptFieldsData = {
    vendor: asText(o.vendor ?? o.merchant ?? o.store),
    amount: asText(o.amount ?? o.total),
    currency: asText(o.currency),
    date: asText(o.date ?? o.receipt_date),
    items: items.length > 0 ? items : null,
  };
  return data.vendor || data.amount || data.currency || data.date || data.items
    ? data
    : null;
}

/** "Receipt OCR" marker — shown wherever a capture carries OCR results. */
export function ReceiptOcrBadge() {
  return (
    <Badge variant="info">
      <ScanText className="mr-1 h-3 w-3" /> Receipt OCR
    </Badge>
  );
}

/** One-line summary for dense table cells (vendor · amount CCY · date). */
export function ReceiptSummary({ fields }: { fields: ReceiptFieldsData }) {
  const amount = formatAmountWithCurrency(fields.amount, fields.currency);
  const parts = [fields.vendor, amount, fields.date].filter(
    (p): p is string => typeof p === "string" && p !== "",
  );
  if (parts.length === 0) return null;
  return (
    <span className="text-xs text-muted-foreground">{parts.join(" · ")}</span>
  );
}

/** Amount with the OCR-read currency code appended when known. */
function formatAmountWithCurrency(
  amount?: number | string | null,
  currency?: string | null,
): string | null {
  const a = asText(amount);
  if (!a) return null;
  const c = asText(currency);
  return c ? `${a} ${c}` : a;
}

/** Full panel: vendor/amount/date header plus the itemized lines. */
export function ReceiptFields({ fields }: { fields: ReceiptFieldsData }) {
  const amount = formatAmountWithCurrency(fields.amount, fields.currency);
  return (
    <div className="rounded-md border border-border bg-muted/40 p-3">
      <div className="flex flex-wrap items-center gap-2">
        <ReceiptOcrBadge />
        {fields.vendor ? (
          <span className="text-sm font-medium">{fields.vendor}</span>
        ) : null}
        {amount ? <span className="text-sm">{amount}</span> : null}
        {fields.date ? (
          <span className="text-xs text-muted-foreground">{fields.date}</span>
        ) : null}
      </div>
      {fields.items && fields.items.length > 0 ? (
        <ul className="mt-2 space-y-1 text-xs text-muted-foreground">
          {fields.items.map((item, i) => (
            <li key={i} className="flex flex-wrap gap-x-2">
              <span>{item.name ?? "Item"}</span>
              {item.quantity ? <span>×{item.quantity}</span> : null}
              {item.price ? <span>@ {item.price}</span> : null}
              {item.amount ? <span>= {item.amount}</span> : null}
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}
