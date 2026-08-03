"use client";

/**
 * Referrer leaderboard (SPEC-W14 Agent C): top referrers ranked by
 * verified+converted referral counts, with paid totals summed from paid
 * payouts (contract §4). Presentational — rows are built by the pure
 * buildLeaderboard helper in ./types.
 */
import { Trophy } from "lucide-react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { titleCase } from "@/lib/utils";
import {
  formatCount,
  formatNgn,
  type LeaderboardRow,
} from "@/components/growth/types";

export function ReferrerLeaderboard({
  rows,
  loading,
  payoutsAvailable,
}: {
  rows: LeaderboardRow[];
  loading: boolean;
  /**
   * false when the payouts read was unavailable — the paid-total column
   * renders "—" and a hint explains why (no fabricated zeros).
   */
  payoutsAvailable: boolean;
}) {
  const top = rows.slice(0, 10);
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Trophy className="h-4 w-4 text-muted-foreground" />
          Referrer leaderboard
        </CardTitle>
        <CardDescription>
          Top referrers by verified + converted referrals, with the total
          commission paid out to each.
          {payoutsAvailable
            ? ""
            : " Payout totals are unavailable right now — counts are still accurate."}
        </CardDescription>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-10">#</TableHead>
              <TableHead>Referrer</TableHead>
              <TableHead className="text-right">Verified</TableHead>
              <TableHead className="text-right">Converted</TableHead>
              <TableHead className="text-right">Paid referrals</TableHead>
              <TableHead className="text-right">Total paid</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading ? (
              <TableEmpty colSpan={6}>Loading leaderboard…</TableEmpty>
            ) : top.length === 0 ? (
              <TableEmpty colSpan={6}>
                No verified or converted referrals yet — the leaderboard fills
                in as referrals progress.
              </TableEmpty>
            ) : (
              top.map((row, i) => (
                <TableRow key={row.key}>
                  <TableCell className="text-muted-foreground">
                    {i + 1}
                  </TableCell>
                  <TableCell className="font-medium">
                    <span className="text-muted-foreground">
                      {titleCase(row.referrer_type)} ·{" "}
                    </span>
                    {row.referrer_id}
                  </TableCell>
                  <TableCell className="text-right">
                    {formatCount(row.verified)}
                  </TableCell>
                  <TableCell className="text-right">
                    {formatCount(row.converted)}
                  </TableCell>
                  <TableCell className="text-right">
                    {formatCount(row.paidCount)}
                  </TableCell>
                  <TableCell className="text-right">
                    {formatNgn(row.paidTotalKobo)}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}
