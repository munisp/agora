/**
 * Lending app types + display helpers (SPEC-W20 Agent C). Mirrors the
 * booking-service internal/lending JSON shapes exactly (snake_case) — the
 * BFF path is /api/bookings/v1/lending/*. List unwrap uses the shared
 * tolerant unwrap<T>() from components/apps/types.ts (READ-ONLY import).
 *
 * Money is kobo int64 everywhere (₦1 = 100 kobo); interest is basis
 * points (100 bps = 1%).
 */
import { unwrap } from "@/components/apps/types";
import { formatMoney, formatDateTime } from "@/lib/utils";

export { unwrap };

// ---------------------------------------------------------------------------
// Status enums (backend mirrors)
// ---------------------------------------------------------------------------

export const APPLICATION_STATUSES = [
  "draft",
  "submitted",
  "under_review",
  "approved",
  "declined",
  "disbursed",
  "repaid",
  "defaulted",
] as const;
export type ApplicationStatus = (typeof APPLICATION_STATUSES)[number];

export const LOAN_STATUSES = ["active", "repaid", "defaulted"] as const;
export type LoanStatus = (typeof LOAN_STATUSES)[number];

type BadgeVariant =
  | "default"
  | "secondary"
  | "outline"
  | "success"
  | "warning"
  | "destructive"
  | "info";

export const APPLICATION_STATUS_META: Record<
  string,
  { label: string; variant: BadgeVariant }
> = {
  draft: { label: "Draft", variant: "outline" },
  submitted: { label: "Submitted", variant: "info" },
  under_review: { label: "Under review", variant: "warning" },
  approved: { label: "Approved", variant: "success" },
  declined: { label: "Declined", variant: "destructive" },
  disbursed: { label: "Disbursed", variant: "default" },
  repaid: { label: "Repaid", variant: "secondary" },
  defaulted: { label: "Defaulted", variant: "destructive" },
};

export const LOAN_STATUS_META: Record<
  string,
  { label: string; variant: BadgeVariant }
> = {
  active: { label: "Active", variant: "success" },
  repaid: { label: "Repaid", variant: "secondary" },
  defaulted: { label: "Defaulted", variant: "destructive" },
};

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

/** Product row of GET /v1/lending/products. */
export interface Product {
  id: string;
  tenant_id?: string;
  name: string;
  active: boolean;
  principal_min_kobo: number;
  principal_max_kobo: number;
  term_days: number;
  interest_bps: number;
  fee_flat_kobo: number;
  created_at?: string;
  updated_at?: string;
}

/** Application row of GET /v1/lending/applications. */
export interface LoanApplication {
  id: string;
  tenant_id?: string;
  contact_id: string;
  product_id: string;
  principal_kobo: number;
  status: ApplicationStatus;
  /** Naive 0..100 score (computed on submit) — NOT a credit bureau score. */
  score: number | null;
  decline_reason: string | null;
  decided_by: string | null;
  decided_at: string | null;
  created_at?: string;
  updated_at?: string;
}

/** Loan account row of GET /v1/lending/loans. */
export interface LoanAccount {
  id: string;
  tenant_id?: string;
  application_id: string;
  contact_id: string;
  principal_kobo: number;
  interest_kobo: number;
  fee_kobo: number;
  outstanding_kobo: number;
  disbursed_at: string;
  due_at: string;
  status: LoanStatus;
}

/** Repayment row inside the loan view. */
export interface Repayment {
  id: string;
  loan_id: string;
  amount_kobo: number;
  ref_id: string;
  paid_at: string;
}

/** Envelope of GET /v1/lending/loans/{id}. */
export interface LoanView {
  loan: LoanAccount;
  application: LoanApplication | null;
  repayments: Repayment[];
  total_kobo: number;
  days_past_due: number;
}

/** Response of POST /v1/lending/applications/{id}/disburse. */
export interface DisburseResponse {
  loan: LoanAccount;
  application: LoanApplication;
  replayed: boolean;
}

/** Response of POST /v1/lending/loans/{id}/repay. */
export interface RepayResponse {
  repayment: Repayment;
  loan: LoanAccount;
  requested_kobo: number;
  clamped: boolean;
  replayed: boolean;
  loan_repaid: boolean;
}

/** Response of GET /v1/lending/portfolio. */
export interface Portfolio {
  total_outstanding_kobo: number;
  active_count: number;
  repaid_count: number;
  defaulted_count: number;
  /** 0..1, null when there is no outstanding (honest empty state). */
  par30: number | null;
  par30_outstanding_kobo: number;
  computed_at?: string;
}

// ---------------------------------------------------------------------------
// Display helpers
// ---------------------------------------------------------------------------

/** Kobo → ₦ formatting (kobo is the NGN cent unit). */
export function formatKobo(kobo: number): string {
  return formatMoney(kobo, "NGN", "en-NG");
}

/** Basis points → human percent (1500 → "15%"). */
export function formatBps(bps: number): string {
  const pct = bps / 100;
  return `${Number.isInteger(pct) ? pct : pct.toFixed(2)}%`;
}

export function formatTs(iso?: string | null): string {
  if (!iso) return "—";
  try {
    return formatDateTime(iso);
  } catch {
    return iso;
  }
}

export function formatDay(iso?: string | null): string {
  if (!iso) return "—";
  try {
    return new Intl.DateTimeFormat("en-NG", { dateStyle: "medium" }).format(
      new Date(iso),
    );
  } catch {
    return iso;
  }
}

/** PAR30 ratio → percent label ("—" for the honest null empty state). */
export function formatPar30(par30: number | null): string {
  if (par30 === null || par30 === undefined) return "—";
  return `${(par30 * 100).toFixed(1)}%`;
}

/** Score badge variant: higher is safer (naive score, not a bureau). */
export function scoreVariant(score: number | null): BadgeVariant {
  if (score === null || score === undefined) return "outline";
  if (score >= 60) return "success";
  if (score >= 30) return "warning";
  return "destructive";
}

/** Short id rendering for uuid columns. */
export function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}
