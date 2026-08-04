"use client";

/**
 * Lending portfolio tiles (SPEC-W20 Agent C): total outstanding, book
 * counts (active / repaid / defaulted) and PAR30 — the outstanding of
 * loans >30 days past due over total outstanding. PAR30 renders "—" when
 * the book has no outstanding (the backend's honest null, not 0%).
 *
 * Data: GET /api/bookings/v1/lending/portfolio
 */
import { Card, CardContent } from "@/components/ui/card";
import {
  formatKobo,
  formatPar30,
  type Portfolio,
} from "@/components/apps/lending/types";

export function PortfolioTiles({
  portfolio,
  loading,
}: {
  portfolio: Portfolio | null;
  loading: boolean;
}) {
  if (loading) {
    return (
      <p className="text-sm text-muted-foreground">Loading portfolio…</p>
    );
  }
  if (!portfolio) {
    return (
      <p className="text-sm text-muted-foreground">
        No portfolio data yet — the tiles fill in once the first loan is
        disbursed.
      </p>
    );
  }
  const tiles: Array<[string, string, string?]> = [
    [
      "Total outstanding",
      formatKobo(portfolio.total_outstanding_kobo),
      `${portfolio.active_count} active loan${portfolio.active_count === 1 ? "" : "s"}`,
    ],
    ["Active", String(portfolio.active_count)],
    ["Repaid", String(portfolio.repaid_count)],
    ["Defaulted", String(portfolio.defaulted_count)],
    [
      "PAR30",
      formatPar30(portfolio.par30),
      portfolio.par30 === null
        ? "no outstanding — nothing at risk"
        : `${formatKobo(portfolio.par30_outstanding_kobo)} >30d past due`,
    ],
  ];
  return (
    <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
      {tiles.map(([label, value, hint]) => (
        <Card key={label}>
          <CardContent className="py-4">
            <p className="text-xs text-muted-foreground">{label}</p>
            <p className="mt-1 text-xl font-semibold">{value}</p>
            {hint ? (
              <p className="mt-1 text-xs text-muted-foreground">{hint}</p>
            ) : null}
          </CardContent>
        </Card>
      ))}
    </div>
  );
}
