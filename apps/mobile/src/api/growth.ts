/**
 * Growth (referrals + leaderboard) pure helpers — a line-for-line mirror of
 * apps/admin-web/components/growth/types.ts (SPEC-W14 Agent C). Keep the two
 * in sync: the leaderboard must rank identically on web and mobile.
 *
 * Money: integer kobo in, formatted NGN out (divide by 100 — same
 * convention as the billing page's formatMoney).
 */
import type { Payout, Referral } from "./types";

export interface LeaderboardRow {
  key: string;
  referrer_type: string;
  referrer_id: string;
  verified: number;
  converted: number;
  paidCount: number;
  /** null when the payouts read was unavailable (render "—", never 0). */
  paidTotalKobo: number | null;
}

/**
 * Top referrers ranked by verified+converted counts, paid totals summed
 * from paid payouts (contract §4). Mirrors admin-web buildLeaderboard.
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
        paidTotalKobo: payouts ? paidByBeneficiary.get(r.referrer_id) ?? 0 : null,
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

/** Integer-kobo → "₦1,234.00" (NGN, Nigerian locale). */
export function formatNgn(kobo: number | null | undefined): string {
  if (kobo === null || kobo === undefined || !Number.isFinite(kobo)) return "—";
  return new Intl.NumberFormat("en-NG", {
    style: "currency",
    currency: "NGN",
  }).format(kobo / 100);
}

export function formatCount(n: number, locale = "en-NG"): string {
  return new Intl.NumberFormat(locale).format(n);
}

export function shortId(id: string | null | undefined): string {
  if (!id) return "—";
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}
