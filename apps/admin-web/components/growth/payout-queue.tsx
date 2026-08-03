"use client";

/**
 * Payout queue (SPEC-W14 Agent C, contract §4): every payout row with
 * status, provider and failure reason. Execution is a Temporal workflow
 * server-side (provider transfer, 3 attempts with backoff, then "failed") —
 * this table is the operator's read view.
 */
import { Badge } from "@/components/ui/badge";
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
import { formatDateTime, titleCase } from "@/lib/utils";
import {
  formatNgn,
  payoutStatusVariant,
  shortId,
  type Payout,
} from "@/components/growth/types";

export function PayoutQueue({
  rows,
  loading,
}: {
  rows: Payout[];
  loading: boolean;
}) {
  const sorted = [...rows].sort((a, b) =>
    (b.created_at ?? "").localeCompare(a.created_at ?? ""),
  );
  return (
    <Card>
      <CardHeader>
        <CardTitle>Payout queue</CardTitle>
        <CardDescription>
          Commission payouts executed via the payments provider. Failed rows
          show the provider&apos;s failure reason after the workflow exhausts
          its retries.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Payout</TableHead>
              <TableHead>Beneficiary</TableHead>
              <TableHead className="text-right">Amount</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Provider</TableHead>
              <TableHead>Provider ref</TableHead>
              <TableHead>Failure reason</TableHead>
              <TableHead>Created</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading ? (
              <TableEmpty colSpan={8}>Loading payouts…</TableEmpty>
            ) : sorted.length === 0 ? (
              <TableEmpty colSpan={8}>
                No payouts queued yet — payouts are created when commission
                balances are settled.
              </TableEmpty>
            ) : (
              sorted.map((p) => (
                <TableRow key={p.payout_id}>
                  <TableCell className="font-medium">
                    {shortId(p.payout_id)}
                  </TableCell>
                  <TableCell>{p.beneficiary_id}</TableCell>
                  <TableCell className="text-right">
                    {formatNgn(p.amount_ngn)}
                  </TableCell>
                  <TableCell>
                    <Badge variant={payoutStatusVariant(p.status)}>
                      {titleCase(p.status)}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {p.provider ? titleCase(p.provider) : "—"}
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {p.provider_ref ?? "—"}
                  </TableCell>
                  <TableCell className="max-w-48 truncate text-muted-foreground">
                    {p.failure_reason ?? "—"}
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {p.created_at ? formatDateTime(p.created_at) : "—"}
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
