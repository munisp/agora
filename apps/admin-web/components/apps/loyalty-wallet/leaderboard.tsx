"use client";

/**
 * Loyalty leaderboard (SPEC-W19 Agent C): wallet ranking by lifetime
 * earned (default), balance or lifetime redeemed. Pure presentational.
 */
import * as React from "react";
import { Trophy } from "lucide-react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Label, Select } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatPoints, tierLabel, type LeaderboardEntry } from "./types";

export const LEADERBOARD_METRICS = [
  { value: "lifetime_earned", label: "Lifetime earned" },
  { value: "balance", label: "Current balance" },
  { value: "lifetime_redeemed", label: "Lifetime redeemed" },
] as const;

export function Leaderboard({
  entries,
  loading,
  metric,
  onMetricChange,
}: {
  entries: LeaderboardEntry[];
  loading: boolean;
  metric: string;
  onMetricChange: (metric: string) => void;
}) {
  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between space-y-0">
        <div>
          <CardTitle className="flex items-center gap-2 text-base">
            <Trophy className="h-4 w-4 text-primary" /> Leaderboard
          </CardTitle>
          <CardDescription>
            Top wallets by {LEADERBOARD_METRICS.find((m) => m.value === metric)?.label ?? metric}.
          </CardDescription>
        </div>
        <div className="w-48 space-y-1.5">
          <Label htmlFor="lb-metric">Rank by</Label>
          <Select
            id="lb-metric"
            value={metric}
            onChange={(e) => onMetricChange(e.target.value)}
          >
            {LEADERBOARD_METRICS.map((m) => (
              <option key={m.value} value={m.value}>
                {m.label}
              </option>
            ))}
          </Select>
        </div>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-12">#</TableHead>
              <TableHead>Contact</TableHead>
              <TableHead>Tier</TableHead>
              <TableHead className="text-right">Balance</TableHead>
              <TableHead className="text-right">Earned</TableHead>
              <TableHead className="text-right">Redeemed</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {entries.map((e) => (
              <TableRow key={e.contact_id}>
                <TableCell className="font-medium">{e.rank}</TableCell>
                <TableCell className="font-mono text-xs">
                  {e.contact_id}
                </TableCell>
                <TableCell>
                  <Badge variant={e.tier ? "info" : "secondary"}>
                    {tierLabel(e.tier)}
                  </Badge>
                </TableCell>
                <TableCell className="text-right">
                  {formatPoints(e.balance)}
                </TableCell>
                <TableCell className="text-right">
                  {formatPoints(e.lifetime_earned)}
                </TableCell>
                <TableCell className="text-right">
                  {formatPoints(e.lifetime_redeemed)}
                </TableCell>
              </TableRow>
            ))}
            {entries.length === 0 ? (
              <TableEmpty colSpan={6}>
                {loading
                  ? "Loading leaderboard…"
                  : "No wallets yet — points appear here after the first accrual."}
              </TableEmpty>
            ) : null}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}
