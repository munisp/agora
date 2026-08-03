"use client";

/**
 * CAC by-channel table (SPEC-W13 Agent D): spend, leads, conversions,
 * conversion rate and CAC per channel, with a trend column comparing CAC
 * against the immediately preceding period of equal length (fetched by the
 * parent from the same contract §5 endpoint — no extra backend surface).
 */
import { ArrowDownRight, ArrowUpRight, Minus } from "lucide-react";
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
import { cn, titleCase } from "@/lib/utils";
import {
  formatCount,
  formatNaira,
  pctChange,
  type CacChannelRow,
} from "@/components/cac/types";

export function CacChannelTable({
  rows,
  previousRows,
  loading,
  days,
}: {
  rows: CacChannelRow[];
  previousRows: CacChannelRow[];
  loading: boolean;
  days: number;
}) {
  const prevByChannel = new Map(previousRows.map((r) => [r.channel, r]));
  const sorted = [...rows].sort((a, b) => b.spend_ngn - a.spend_ngn);

  return (
    <Card>
      <CardHeader>
        <CardTitle>CAC by channel</CardTitle>
        <CardDescription>
          Spend, funnel counts and acquisition cost per first-touch channel,
          last {days} days. Trend compares CAC with the previous {days} days.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Channel</TableHead>
              <TableHead className="text-right">Spend</TableHead>
              <TableHead className="text-right">Leads</TableHead>
              <TableHead className="text-right">Conversions</TableHead>
              <TableHead className="text-right">Conv. rate</TableHead>
              <TableHead className="text-right">CAC</TableHead>
              <TableHead className="text-right">CAC trend</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading ? (
              <TableEmpty colSpan={7}>Loading channel metrics…</TableEmpty>
            ) : sorted.length === 0 ? (
              <TableEmpty colSpan={7}>
                No acquisition activity in this period — spend appears here
                once campaigns report it and leads start converting.
              </TableEmpty>
            ) : (
              sorted.map((row) => {
                const prev = prevByChannel.get(row.channel);
                const trend = prev ? pctChange(row.cac_ngn, prev.cac_ngn) : null;
                const convRate =
                  row.leads > 0 ? (row.conversions / row.leads) * 100 : null;
                return (
                  <TableRow key={row.channel}>
                    <TableCell className="font-medium">
                      {titleCase(row.channel)}
                    </TableCell>
                    <TableCell className="text-right">
                      {formatNaira(row.spend_ngn)}
                    </TableCell>
                    <TableCell className="text-right">
                      {formatCount(row.leads)}
                    </TableCell>
                    <TableCell className="text-right">
                      {formatCount(row.conversions)}
                    </TableCell>
                    <TableCell className="text-right">
                      {convRate === null ? "—" : `${convRate.toFixed(1)}%`}
                    </TableCell>
                    <TableCell className="text-right">
                      {formatNaira(row.cac_ngn)}
                    </TableCell>
                    <TableCell className="text-right">
                      <TrendBadge change={trend} />
                    </TableCell>
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}

/**
 * CAC trend badge: for acquisition cost, DOWN is good (green-ish/success)
 * and UP is bad (warning). Null renders a neutral dash — e.g. the channel
 * had no spend in the previous period.
 */
function TrendBadge({ change }: { change: number | null }) {
  if (change === null) {
    return (
      <span className="inline-flex items-center justify-end gap-1 text-xs text-muted-foreground">
        <Minus className="h-3.5 w-3.5" /> —
      </span>
    );
  }
  const improved = change < 0;
  const flat = Math.abs(change) < 0.5;
  return (
    <span
      className={cn(
        "inline-flex items-center justify-end gap-1 text-xs font-medium",
        flat
          ? "text-muted-foreground"
          : improved
            ? "text-success"
            : "text-warning",
      )}
    >
      {flat ? (
        <Minus className="h-3.5 w-3.5" />
      ) : improved ? (
        <ArrowDownRight className="h-3.5 w-3.5" />
      ) : (
        <ArrowUpRight className="h-3.5 w-3.5" />
      )}
      {change >= 0 ? "+" : ""}
      {change.toFixed(1)}%
    </span>
  );
}
