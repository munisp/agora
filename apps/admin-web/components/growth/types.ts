/**
 * Shared referral & commission ("Growth") dashboard types and pure helpers
 * (SPEC-W14 Agent C, contracts §1–§4). Nothing here touches the network —
 * safe to import anywhere.
 *
 * Money convention: every `*_ngn` field in the SPEC-W14 contracts is an
 * integer amount in KOBO (contract §2: "amount_ngn int (kobo)"). Formatting
 * reuses the billing page's `formatMoney` helper from lib/utils (which
 * divides by 100) — reused, not duplicated.
 */
import { formatMoney } from "@/lib/utils";

/** Contract §1: referral row as returned by GET /v1/referrals. */
export interface Referral {
  referral_id: string;
  tenant_id?: string;
  referrer_type: "contact" | "agent" | "staff" | string;
  referrer_id: string;
  referee_phone: string;
  campaign_id?: string | null;
  status: "pending" | "verified" | "converted" | "paid" | "rejected" | string;
  bounty_rule_id?: string | null;
  created_at?: string;
  verified_at?: string | null;
  paid_at?: string | null;
}

/** Contract §2: tenant-editable commission rule. */
export interface CommissionRule {
  rule_id: string;
  tenant_id?: string;
  name: string;
  trigger: "signup_verified" | "first_booking" | "first_txn" | "sale" | string;
  beneficiary: "referrer" | "agent" | "staff" | string;
  amount_type: "flat" | "percent" | string;
  /** flat rules: integer kobo. */
  amount_ngn?: number | null;
  /** percent rules: basis points (100 bps = 1%). */
  bps?: number | null;
  /** optional cap, integer kobo. */
  cap_ngn?: number | null;
  active: boolean;
  priority: number;
}

/** Contract §3: double-entry ledger row. */
export interface LedgerEntry {
  entry_id: string;
  tenant_id?: string;
  journal_id: string;
  account_code: number;
  debit_ngn: number;
  credit_ngn: number;
  ref_type?: string | null;
  ref_id?: string | null;
  created_at?: string;
}

/** Contract §3 account codes. */
export const ACCOUNT_CODES: Record<number, string> = {
  300: "Commission payable",
  301: "Commission expense",
  302: "Agent float",
  303: "House clearing",
};

export function accountLabel(code: number): string {
  const name = ACCOUNT_CODES[code];
  return name ? `${code} · ${name}` : String(code);
}

/** Contract §4: payout row as returned by GET /v1/payouts. */
export interface Payout {
  payout_id: string;
  tenant_id?: string;
  beneficiary_id: string;
  amount_ngn: number;
  status: "queued" | "processing" | "paid" | "failed" | string;
  provider?: "paystack" | "flutterwave" | string | null;
  provider_ref?: string | null;
  failure_reason?: string | null;
  created_at?: string;
}

/**
 * Tolerant list unwrap (same contract as the CAC pages): booking-service
 * list endpoints answer with keyed envelopes ({referrals:[...]},
 * {rules:[...]}, {entries:[...]}, {payouts:[...]}), other services use
 * {items:[...]} or a bare array — accept all of them by taking the first
 * own-property value that is an array; anything else yields [].
 */
export function unwrap<T>(data: unknown): T[] {
  if (Array.isArray(data)) return data as T[];
  if (typeof data === "object" && data !== null) {
    for (const value of Object.values(data)) {
      if (Array.isArray(value)) return value as T[];
    }
  }
  return [];
}

/**
 * Format an integer-kobo amount as NGN. Reuses the billing page's
 * formatMoney (cents → major unit) with the NGN currency and Nigerian
 * locale — no duplicated money math.
 */
export function formatNgn(kobo: number | null | undefined): string {
  if (kobo === null || kobo === undefined || !Number.isFinite(kobo)) return "—";
  return formatMoney(kobo, "NGN", "en-NG");
}

/** Compact number for counts (1,234). */
export function formatCount(n: number, locale = "en-NG"): string {
  return new Intl.NumberFormat(locale).format(n);
}

/** Short id for dense tables: first 8 chars of a uuid. */
export function shortId(id: string | null | undefined): string {
  if (!id) return "—";
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}

/** Badge variant per referral status (warm, low-saturation palette). */
export function referralStatusVariant(
  status: string,
): "success" | "warning" | "info" | "secondary" | "destructive" {
  switch (status) {
    case "converted":
    case "paid":
      return "success";
    case "verified":
      return "info";
    case "pending":
      return "warning";
    case "rejected":
      return "destructive";
    default:
      return "secondary";
  }
}

/** Badge variant per payout status. */
export function payoutStatusVariant(
  status: string,
): "success" | "warning" | "info" | "secondary" | "destructive" {
  switch (status) {
    case "paid":
      return "success";
    case "processing":
      return "info";
    case "queued":
      return "warning";
    case "failed":
      return "destructive";
    default:
      return "secondary";
  }
}

/** Human label for a commission-rule trigger. */
export function triggerLabel(trigger: string): string {
  switch (trigger) {
    case "signup_verified":
      return "Signup verified";
    case "first_booking":
      return "First booking";
    case "first_txn":
      return "First transaction";
    case "sale":
      return "Sale";
    default:
      return trigger;
  }
}

/**
 * Human description of a rule's amount: flat kobo → ₦ string, percent bps →
 * a percentage (bps/100, integer-source math only — no float money), with
 * the cap appended when present.
 */
export function ruleAmountLabel(rule: CommissionRule): string {
  const base =
    rule.amount_type === "percent"
      ? `${((rule.bps ?? 0) / 100).toFixed(2).replace(/\.?0+$/, "")}%`
      : formatNgn(rule.amount_ngn ?? 0);
  return rule.cap_ngn ? `${base} (cap ${formatNgn(rule.cap_ngn)})` : base;
}

/**
 * Extract a numeric kobo balance from the tolerant balance endpoint
 * (GET /v1/commissions/balance/{beneficiary}). The contract does not pin the
 * response envelope, so accept common shapes: a bare number, or an object
 * with balance_ngn / balance / amount_ngn / available_ngn.
 */
export function extractBalance(data: unknown): number | null {
  if (typeof data === "number" && Number.isFinite(data)) return data;
  if (typeof data === "object" && data !== null) {
    const obj = data as Record<string, unknown>;
    for (const key of [
      "balance_ngn",
      "balance",
      "amount_ngn",
      "available_ngn",
    ]) {
      const v = obj[key];
      if (typeof v === "number" && Number.isFinite(v)) return v;
    }
  }
  return null;
}

/** One leaderboard row: a referrer ranked by verified+converted referrals. */
export interface LeaderboardRow {
  key: string;
  referrer_type: string;
  referrer_id: string;
  verified: number;
  converted: number;
  paidCount: number;
  /** total paid out to this beneficiary (kobo), null when unknown */
  paidTotalKobo: number | null;
}

/**
 * Build the per-referrer leaderboard: rank by verified+converted (both
 * desc). `paidTotalKobo` is summed from PAID payouts keyed by beneficiary_id
 * (contract §4) — when the payouts read is unavailable, totals are null and
 * the column renders "—" instead of fabricated zeros.
 */
export function buildLeaderboard(
  referrals: Referral[],
  payouts: Payout[] | null,
): LeaderboardRow[] {
  const paidByBeneficiary = new Map<string, number>();
  if (payouts) {
    for (const p of payouts) {
      if (p.status !== "paid") continue;
      paidByBeneficiary.set(
        p.beneficiary_id,
        (paidByBeneficiary.get(p.beneficiary_id) ?? 0) + (p.amount_ngn ?? 0),
      );
    }
  }
  const rows = new Map<string, LeaderboardRow>();
  for (const r of referrals) {
    const key = `${r.referrer_type}:${r.referrer_id}`;
    let row = rows.get(key);
    if (!row) {
      row = {
        key,
        referrer_type: r.referrer_type,
        referrer_id: r.referrer_id,
        verified: 0,
        converted: 0,
        paidCount: 0,
        paidTotalKobo: payouts
          ? (paidByBeneficiary.get(r.referrer_id) ?? 0)
          : null,
      };
      rows.set(key, row);
    }
    if (r.status === "verified") row.verified += 1;
    if (r.status === "converted" || r.status === "paid") row.converted += 1;
    if (r.status === "paid") row.paidCount += 1;
  }
  return [...rows.values()]
    .filter((row) => row.verified + row.converted > 0)
    .sort(
      (a, b) =>
        b.verified + b.converted - (a.verified + a.converted) ||
        b.converted - a.converted,
    );
}

/** One journal group: the balanced pair(s) sharing a journal_id. */
export interface JournalGroup {
  journal_id: string;
  entries: LedgerEntry[];
  totalDebit: number;
  totalCredit: number;
  balanced: boolean;
  created_at?: string;
  ref_type?: string | null;
  ref_id?: string | null;
}

/** Group ledger rows by journal_id (contract §3), newest journal first. */
export function groupByJournal(entries: LedgerEntry[]): JournalGroup[] {
  const groups = new Map<string, JournalGroup>();
  for (const e of entries) {
    let g = groups.get(e.journal_id);
    if (!g) {
      g = {
        journal_id: e.journal_id,
        entries: [],
        totalDebit: 0,
        totalCredit: 0,
        balanced: false,
        created_at: e.created_at,
        ref_type: e.ref_type,
        ref_id: e.ref_id,
      };
      groups.set(e.journal_id, g);
    }
    g.entries.push(e);
    g.totalDebit += e.debit_ngn ?? 0;
    g.totalCredit += e.credit_ngn ?? 0;
    if (e.created_at && (!g.created_at || e.created_at > g.created_at)) {
      g.created_at = e.created_at;
    }
  }
  const out = [...groups.values()];
  for (const g of out) {
    g.entries.sort((a, b) => a.account_code - b.account_code);
    g.balanced = g.totalDebit === g.totalCredit;
  }
  out.sort((a, b) => (b.created_at ?? "").localeCompare(a.created_at ?? ""));
  return out;
}
