"use client";

/**
 * Lending loan book + loan detail (SPEC-W20 Agent C): the loans table
 * (status filter, days-past-due hint), the schedule card
 * (principal/interest/fee/outstanding/due), the repayments history and the
 * repay form (amount + ref_id idempotency key; overpay is clamped
 * server-side and noted in the response).
 *
 * Data: GET /api/bookings/v1/lending/loans?status=
 *       GET /api/bookings/v1/lending/loans/{id}
 *       POST /api/bookings/v1/lending/loans/{id}/repay
 */
import * as React from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input, Label, Select } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  LOAN_STATUSES,
  LOAN_STATUS_META,
  formatDay,
  formatKobo,
  formatTs,
  shortId,
  type LoanAccount,
  type LoanView,
} from "@/components/apps/lending/types";

export function LoansTable({
  loans,
  statusFilter,
  onStatusFilter,
  onOpen,
  loading,
}: {
  loans: LoanAccount[];
  statusFilter: string;
  onStatusFilter: (s: string) => void;
  onOpen: (loan: LoanAccount) => void;
  loading: boolean;
}) {
  return (
    <div>
      <div className="mb-3 flex items-center gap-2">
        <Select
          className="w-44"
          value={statusFilter}
          onChange={(e) => onStatusFilter(e.target.value)}
        >
          <option value="">All statuses</option>
          {LOAN_STATUSES.map((s) => (
            <option key={s} value={s}>
              {LOAN_STATUS_META[s]?.label ?? s}
            </option>
          ))}
        </Select>
      </div>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Loan</TableHead>
            <TableHead>Principal</TableHead>
            <TableHead>Outstanding</TableHead>
            <TableHead>Due</TableHead>
            <TableHead>Status</TableHead>
            <TableHead className="text-right">Open</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {loans.map((l) => {
            const pastDue =
              l.status === "active" && new Date(l.due_at).getTime() < Date.now();
            return (
              <TableRow key={l.id}>
                <TableCell>
                  <span className="font-mono text-xs" title={l.id}>
                    {shortId(l.id)}
                  </span>
                </TableCell>
                <TableCell>{formatKobo(l.principal_kobo)}</TableCell>
                <TableCell>{formatKobo(l.outstanding_kobo)}</TableCell>
                <TableCell>
                  {formatDay(l.due_at)}
                  {pastDue ? (
                    <Badge variant="warning" className="ml-2">
                      past due
                    </Badge>
                  ) : null}
                </TableCell>
                <TableCell>
                  <Badge variant={LOAN_STATUS_META[l.status]?.variant ?? "outline"}>
                    {LOAN_STATUS_META[l.status]?.label ?? l.status}
                  </Badge>
                </TableCell>
                <TableCell className="text-right">
                  <Button size="sm" variant="outline" onClick={() => onOpen(l)}>
                    Detail
                  </Button>
                </TableCell>
              </TableRow>
            );
          })}
          {loans.length === 0 && !loading ? (
            <TableEmpty colSpan={6}>
              No loans yet — disburse an approved application to create one.
            </TableEmpty>
          ) : null}
          {loading ? (
            <TableEmpty colSpan={6}>Loading loans…</TableEmpty>
          ) : null}
        </TableBody>
      </Table>
    </div>
  );
}

export function LoanDetail({
  view,
  canManage,
  busy,
  onRepay,
  onClose,
}: {
  view: LoanView;
  canManage: boolean;
  busy: boolean;
  /** Returns true when the repay succeeded (parent reloads the view). */
  onRepay: (amountKobo: number, refId: string) => Promise<boolean>;
  onClose: () => void;
}) {
  const { loan } = view;
  const [amount, setAmount] = React.useState(loan.outstanding_kobo);
  const [refId, setRefId] = React.useState("");

  return (
    <Card className="mt-4">
      <CardHeader>
        <div className="flex items-center justify-between">
          <CardTitle className="text-base">
            Loan <span className="font-mono text-sm">{shortId(loan.id)}</span>{" "}
            <Badge variant={LOAN_STATUS_META[loan.status]?.variant ?? "outline"}>
              {LOAN_STATUS_META[loan.status]?.label ?? loan.status}
            </Badge>
          </CardTitle>
          <Button variant="outline" size="sm" onClick={onClose}>
            Close
          </Button>
        </div>
      </CardHeader>
      <CardContent>
        {/* Schedule */}
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
          {[
            ["Principal", formatKobo(loan.principal_kobo)],
            ["Interest", formatKobo(loan.interest_kobo)],
            ["Fee", formatKobo(loan.fee_kobo)],
            ["Total", formatKobo(view.total_kobo)],
            ["Outstanding", formatKobo(loan.outstanding_kobo)],
            [
              "Due",
              `${formatDay(loan.due_at)}${
                view.days_past_due > 0 && loan.status === "active"
                  ? ` (${view.days_past_due}d past due)`
                  : ""
              }`,
            ],
          ].map(([label, value]) => (
            <div key={label} className="rounded-md border border-border p-3">
              <p className="text-xs text-muted-foreground">{label}</p>
              <p className="mt-1 text-sm font-medium">{value}</p>
            </div>
          ))}
        </div>
        <p className="mt-2 text-xs text-muted-foreground">
          Disbursed {formatTs(loan.disbursed_at)} · application{" "}
          <span className="font-mono">{shortId(loan.application_id)}</span>
          {view.application?.decided_by
            ? ` · decided by ${view.application.decided_by}`
            : ""}
        </p>

        {/* Repay form */}
        {canManage && loan.status === "active" ? (
          <div className="mt-4 rounded-md border border-border bg-muted/40 p-3">
            <p className="text-sm font-medium">Record a repayment</p>
            <div className="mt-2 flex flex-wrap items-end gap-2">
              <div>
                <Label htmlFor="repay-amount">Amount (kobo)</Label>
                <Input
                  id="repay-amount"
                  type="number"
                  min={1}
                  value={amount}
                  onChange={(e) => setAmount(Number(e.target.value))}
                />
                <p className="mt-1 text-xs text-muted-foreground">
                  {formatKobo(amount)} — overpay is clamped to outstanding
                </p>
              </div>
              <div>
                <Label htmlFor="repay-ref">Reference (idempotency key)</Label>
                <Input
                  id="repay-ref"
                  value={refId}
                  onChange={(e) => setRefId(e.target.value)}
                  placeholder="e.g. transfer receipt no."
                />
              </div>
              <Button
                disabled={busy || amount <= 0 || refId.trim() === ""}
                onClick={() => void onRepay(amount, refId.trim())}
              >
                {busy ? "Posting…" : "Post repayment"}
              </Button>
            </div>
          </div>
        ) : null}

        {/* Repayments */}
        <div className="mt-4">
          <p className="mb-2 text-sm font-medium">Repayments</p>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Reference</TableHead>
                <TableHead>Amount</TableHead>
                <TableHead>Paid at</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {view.repayments.map((r) => (
                <TableRow key={r.id}>
                  <TableCell className="font-mono text-xs">{r.ref_id}</TableCell>
                  <TableCell>{formatKobo(r.amount_kobo)}</TableCell>
                  <TableCell className="text-sm text-muted-foreground">
                    {formatTs(r.paid_at)}
                  </TableCell>
                </TableRow>
              ))}
              {view.repayments.length === 0 ? (
                <TableEmpty colSpan={3}>No repayments recorded yet.</TableEmpty>
              ) : null}
            </TableBody>
          </Table>
        </div>
      </CardContent>
    </Card>
  );
}
