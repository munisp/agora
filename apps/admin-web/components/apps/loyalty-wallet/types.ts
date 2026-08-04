/**
 * Shared loyalty-wallet types and pure helpers (SPEC-W19 Agent C). Nothing
 * here touches the network — safe to import anywhere. The tolerant list
 * unwrap is imported READ-ONLY from the W18 apps-portal helpers
 * (components/apps/types.ts) — do not re-implement it here.
 *
 * Points are plain integers (no money conversion — unlike the W14 growth
 * ledger, loyalty ledger rows are points, not kobo).
 */
import { unwrap } from "@/components/apps/types";

export { unwrap };

/** Earn-rule events (backend enum — internal/loyalty). */
export const EARN_EVENTS = [
  "booking_completed",
  "first_txn",
  "referral_converted",
] as const;
export type EarnEvent = (typeof EARN_EVENTS)[number];

export const EVENT_LABELS: Record<string, string> = {
  booking_completed: "Booking completed",
  first_txn: "First transaction",
  referral_converted: "Referral converted",
};

export interface EarnRule {
  event: EarnEvent | string;
  points: number;
}

export interface Tier {
  name: string;
  min_points: number;
  benefits?: string;
}

/** Program row as returned by GET /v1/loyalty/programs. */
export interface Program {
  program_id: string;
  tenant_id?: string;
  name: string;
  active: boolean;
  earn_rules: EarnRule[];
  tiers: Tier[];
  /** 0 = uncapped; over-cap accruals are clamped, not rejected. */
  cap_per_day: number;
  created_at?: string;
  updated_at?: string;
}

/** Wallet row as returned inside GET /v1/loyalty/wallets/{contact_id}. */
export interface Wallet {
  tenant_id?: string;
  contact_id: string;
  balance: number;
  lifetime_earned: number;
  lifetime_redeemed: number;
  tier: string;
  updated_at?: string;
}

/** Points double-entry ledger row (codes 400/401). */
export interface LedgerEntry {
  entry_id: string;
  tenant_id?: string;
  journal_id: string;
  account_code: number;
  beneficiary_id: string;
  debit_points: number;
  credit_points: number;
  ref_type?: string | null;
  ref_id?: string | null;
  created_at?: string;
}

export const ACCOUNT_CODES: Record<number, string> = {
  400: "Points issued",
  401: "Points redeemed",
};

/** Envelope of GET /v1/loyalty/wallets/{contact_id}. */
export interface WalletView {
  wallet: Wallet;
  entries: LedgerEntry[];
  ledger_balance: number;
}

/** Leaderboard row of GET /v1/loyalty/leaderboard. */
export interface LeaderboardEntry {
  rank: number;
  contact_id: string;
  balance: number;
  lifetime_earned: number;
  lifetime_redeemed: number;
  tier: string;
}

/** Response of POST /v1/loyalty/accrue. */
export interface AccrueResponse {
  wallet: Wallet;
  awarded: number;
  applied: boolean;
  capped: boolean;
}

/** Response of POST /v1/loyalty/redeem. */
export interface RedeemResponse {
  wallet: Wallet;
  redeemed: number;
  applied: boolean;
  ref_id: string;
}

export function formatPoints(n: number): string {
  return new Intl.NumberFormat("en-US").format(n);
}

export function tierLabel(tier: string): string {
  return tier === "" ? "—" : tier;
}
