"use client";

/**
 * Commission ledger view (SPEC-W14 Agent C, contract §3): double-entry rows
 * grouped by journal_id. Every commission posting is a balanced pair
 * (debit 301 expense / credit 300 payable; payout: debit 300 / credit 302
 * agent-float or 303 house-clearing) — each journal group shows a balanced
 * indicator computed client-side from the row sums.
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
import { cn, formatDateTime } from "@/lib/utils";
import {
  accountLabel,
  formatNgn,
  groupByJournal,
  shortId,
  type LedgerEntry,
} from "@/components/growth/types";

export function LedgerTable({
  entries,
  loading,
  from,
  to,
}: {
  entries: LedgerEntry[];
  loading: boolean;
  from: string;
  to: string;
}) {
  const journals = groupByJournal(entries);
  return (
    <Card>
      <CardHeader>
        <CardTitle>Commission ledger</CardTitle>
        <CardDescription>
          Double-entry postings from {from} to {to}, grouped by journal. Each
          journal must balance: total debits equal total credits.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {loading ? (
          <p className="py-10 text-center text-sm text-muted-foreground">
            Loading ledger entries…
          </p>
        ) : journals.length === 0 ? (
          <p className="py-10 text-center text-sm text-muted-foreground">
            No ledger entries in this window — postings appear here when
            referrals verify and payouts settle.
          </p>
        ) : (
          journals.map((g) => (
            <div
              key={g.journal_id}
              className="rounded-md border border-border"
            >
              <div className="flex flex-wrap items-center gap-x-4 gap-y-1 border-b border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
                <span className="font-medium text-foreground">
                  Journal {shortId(g.journal_id)}
                </span>
                {g.ref_type ? (
                  <span>
                    ref {g.ref_type}
                    {g.ref_id ? ` · ${shortId(g.ref_id)}` : ""}
                  </span>
                ) : null}
                {g.created_at ? <span>{formatDateTime(g.created_at)}</span> : null}
                <Badge
                  variant={g.balanced ? "success" : "destructive"}
                  className="ml-auto"
                >
                  {g.balanced ? "Balanced" : "Unbalanced"}
                </Badge>
              </div>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Account</TableHead>
                    <TableHead>Entry</TableHead>
                    <TableHead className="text-right">Debit</TableHead>
                    <TableHead className="text-right">Credit</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {g.entries.length === 0 ? (
                    <TableEmpty colSpan={4}>No entries.</TableEmpty>
                  ) : (
                    <>
                      {g.entries.map((e) => (
                        <TableRow key={e.entry_id}>
                          <TableCell className="font-medium">
                            {accountLabel(e.account_code)}
                          </TableCell>
                          <TableCell className="text-muted-foreground">
                            {shortId(e.entry_id)}
                          </TableCell>
                          <TableCell
                            className={cn(
                              "text-right",
                              e.debit_ngn > 0
                                ? "text-foreground"
                                : "text-muted-foreground",
                            )}
                          >
                            {e.debit_ngn > 0 ? formatNgn(e.debit_ngn) : "—"}
                          </TableCell>
                          <TableCell
                            className={cn(
                              "text-right",
                              e.credit_ngn > 0
                                ? "text-foreground"
                                : "text-muted-foreground",
                            )}
                          >
                            {e.credit_ngn > 0 ? formatNgn(e.credit_ngn) : "—"}
                          </TableCell>
                        </TableRow>
                      ))}
                      <TableRow className="border-t border-border bg-muted/20 font-medium">
                        <TableCell colSpan={2} className="text-xs">
                          Journal total
                        </TableCell>
                        <TableCell className="text-right">
                          {formatNgn(g.totalDebit)}
                        </TableCell>
                        <TableCell className="text-right">
                          {formatNgn(g.totalCredit)}
                        </TableCell>
                      </TableRow>
                    </>
                  )}
                </TableBody>
              </Table>
            </div>
          ))
        )}
      </CardContent>
    </Card>
  );
}
