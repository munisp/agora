"use client";

/**
 * SPEC-W99 (innovation #11 MVP): DFSP quote marketplace + payout
 * reconciliation panels for the Billing page.
 *
 * Quote comparison is READ-ONLY (no ledger write, no transfer send): it
 * fans a real FSPIOP POST /quotes out to every DFSP the payments-service
 * has configured (PAYOUT_DFSPS) and ranks the verified quotes by receive
 * amount. Payout execution itself still runs on the single configured rail
 * and remains NGN-only — the disclaimer below renders that verbatim.
 */
import * as React from "react";
import { GitCompareArrows, ShieldCheck } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
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
import { formatMoney } from "@/lib/utils";
import type {
  Beneficiary,
  PayoutQuoteComparison,
  ReconciliationReport,
} from "@/lib/types";

function moneyStr(cents: number | null | undefined, currency: string): string {
  if (cents === null || cents === undefined) return "—";
  return formatMoney(cents, currency);
}

/** Parse a decimal major-unit input into minor units; null when invalid. */
function toCents(raw: string): number | null {
  const n = Number(raw);
  if (!Number.isFinite(n) || n <= 0) return null;
  return Math.round(n * 100);
}

export function QuoteComparisonCard({
  orgSlug,
  beneficiaries,
}: {
  orgSlug: string;
  beneficiaries: Beneficiary[] | null;
}) {
  const [beneficiaryId, setBeneficiaryId] = React.useState<string>("");
  const [amount, setAmount] = React.useState<string>("");
  const [currency, setCurrency] = React.useState<string>("NGN");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [result, setResult] = React.useState<PayoutQuoteComparison | null>(
    null,
  );

  React.useEffect(() => {
    if (!beneficiaryId && beneficiaries && beneficiaries.length > 0) {
      setBeneficiaryId(beneficiaries[0].id);
    }
  }, [beneficiaries, beneficiaryId]);

  const compare = async (e: React.FormEvent) => {
    e.preventDefault();
    const cents = toCents(amount);
    if (cents === null) {
      setError("Enter an amount greater than zero.");
      return;
    }
    if (!beneficiaryId) {
      setError("Register and select a payout beneficiary first.");
      return;
    }
    setBusy(true);
    setError(null);
    setResult(null);
    try {
      const data = await api.post<PayoutQuoteComparison>(
        "/api/payments/v1/payouts/quotes",
        {
          tenant_id: orgSlug,
          amount_cents: cents,
          currency,
          beneficiary_id: beneficiaryId,
        },
      );
      setResult(data);
    } catch (err) {
      setError(
        err instanceof ApiError
          ? err.message
          : "Quote comparison failed unexpectedly.",
      );
    } finally {
      setBusy(false);
    }
  };

  const quoteByFsp = new Map(
    (result?.quotes ?? []).map((q) => [q.fsp_id, q] as const),
  );

  return (
    <Card className="mt-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <GitCompareArrows className="h-4 w-4" /> DFSP quote comparison
        </CardTitle>
        <CardDescription>
          Compare real FSPIOP quotes across the configured DFSPs before
          paying out.{" "}
          <span className="font-medium">
            Quotes are read-only and expire; the payout itself still executes
            on the configured rail and remains NGN-only.
          </span>
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form onSubmit={compare} className="flex flex-wrap items-end gap-3">
          <div className="grid min-w-48 flex-1 gap-1.5">
            <Label htmlFor="quote-beneficiary">Beneficiary</Label>
            <Select
              id="quote-beneficiary"
              value={beneficiaryId}
              onChange={(e) => setBeneficiaryId(e.target.value)}
            >
              {(beneficiaries ?? []).map((b) => (
                <option key={b.id} value={b.id}>
                  {b.label}
                </option>
              ))}
            </Select>
          </div>
          <div className="grid w-40 gap-1.5">
            <Label htmlFor="quote-amount">Amount</Label>
            <Input
              id="quote-amount"
              inputMode="decimal"
              placeholder="e.g. 25000.00"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
            />
          </div>
          <div className="grid w-32 gap-1.5">
            <Label htmlFor="quote-currency">Currency</Label>
            <Select
              id="quote-currency"
              value={currency}
              onChange={(e) => setCurrency(e.target.value)}
            >
              <option value="NGN">NGN</option>
              <option value="USD">USD</option>
              <option value="KES">KES</option>
              <option value="GHS">GHS</option>
              <option value="ZAR">ZAR</option>
            </Select>
          </div>
          <Button type="submit" disabled={busy || !beneficiaries?.length}>
            {busy ? "Comparing…" : "Compare quotes"}
          </Button>
        </form>

        {error ? (
          <p className="mt-3 text-sm text-destructive">{error}</p>
        ) : null}

        {result ? (
          <div className="mt-4">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Rank</TableHead>
                  <TableHead>DFSP</TableHead>
                  <TableHead>Transfer</TableHead>
                  <TableHead>Fee</TableHead>
                  <TableHead>Commission</TableHead>
                  <TableHead>Beneficiary receives</TableHead>
                  <TableHead className="pr-2 text-right">Expires</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {result.ranked.length === 0 ? (
                  <TableEmpty colSpan={7}>
                    No DFSP returned a usable quote.
                  </TableEmpty>
                ) : (
                  result.ranked.map((fsp, i) => {
                    const q = quoteByFsp.get(fsp);
                    if (!q) return null;
                    return (
                      <TableRow key={fsp}>
                        <TableCell className="font-medium">
                          #{i + 1}
                          {result.best_fsp === fsp ? (
                            <>
                              {" "}
                              <Badge variant="secondary">best</Badge>
                            </>
                          ) : null}
                        </TableCell>
                        <TableCell className="font-medium">{fsp}</TableCell>
                        <TableCell>
                          {q.transfer_amount.amount} {q.transfer_amount.currency}
                        </TableCell>
                        <TableCell>
                          {q.fees_disclosed && q.payee_fsp_fee
                            ? `${q.payee_fsp_fee.amount}`
                            : "undisclosed"}
                        </TableCell>
                        <TableCell>
                          {q.fees_disclosed && q.payee_fsp_commission
                            ? `${q.payee_fsp_commission.amount}`
                            : "undisclosed"}
                        </TableCell>
                        <TableCell className="font-medium">
                          {moneyStr(q.receive_amount_cents, result.currency)}
                        </TableCell>
                        <TableCell className="pr-2 text-right text-xs text-muted-foreground">
                          {q.expiration
                            ? new Date(q.expiration).toLocaleString()
                            : "—"}
                        </TableCell>
                      </TableRow>
                    );
                  })
                )}
              </TableBody>
            </Table>
            {result.errors.length > 0 ? (
              <div className="mt-3 rounded-md border border-border p-3">
                <p className="text-sm font-medium">
                  DFSPs that could not quote ({result.errors.length})
                </p>
                <ul className="mt-1 list-disc pl-5 text-sm text-muted-foreground">
                  {result.errors.map((e) => (
                    <li key={e.fsp_id}>
                      <span className="font-medium">{e.fsp_id}</span>: {e.error}
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}
            <p className="mt-2 text-xs text-muted-foreground">
              Compared {result.quotes.length + result.errors.length} DFSP(s) at{" "}
              {new Date(result.generated_at).toLocaleString()}.
            </p>
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}

export function ReconciliationCard({ orgSlug }: { orgSlug: string }) {
  const [report, setReport] = React.useState<ReconciliationReport | null>(
    null,
  );
  const [unavailable, setUnavailable] = React.useState<string | null>(null);

  const load = React.useCallback(async () => {
    setUnavailable(null);
    try {
      const data = await api.get<ReconciliationReport>(
        `/api/payments/v1/reconciliation/report?tenant_id=${encodeURIComponent(orgSlug)}`,
      );
      setReport(data);
    } catch (e) {
      setReport(null);
      setUnavailable(
        e instanceof ApiError
          ? e.message
          : "Payments service unreachable.",
      );
    }
  }, [orgSlug]);

  React.useEffect(() => {
    void load();
  }, [load]);

  return (
    <Card className="mt-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldCheck className="h-4 w-4" /> Payout reconciliation
        </CardTitle>
        <CardDescription>
          Ledger snapshot vs durable payout attempts, with computed checks.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {unavailable ? (
          <p className="text-sm text-muted-foreground">
            Reconciliation report unavailable ({unavailable}).
          </p>
        ) : report === null ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : (
          <div className="grid gap-4">
            <div>
              <p className="mb-2 text-sm font-medium">Checks</p>
              <ul className="grid gap-1.5">
                {report.checks.map((c) => (
                  <li key={c.id} className="flex items-start gap-2 text-sm">
                    <Badge
                      variant={
                        c.severity === "warn" ? "warning" : "secondary"
                      }
                    >
                      {c.severity}
                    </Badge>
                    <span className="text-muted-foreground">{c.message}</span>
                  </li>
                ))}
              </ul>
            </div>

            <div>
              <p className="mb-2 text-sm font-medium">Ledger accounts</p>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Account</TableHead>
                    <TableHead>Posted net</TableHead>
                    <TableHead className="pr-2 text-right">
                      Pending net
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {report.ledger.accounts.length === 0 ? (
                    <TableEmpty colSpan={3}>
                      No ledger accounts yet.
                    </TableEmpty>
                  ) : (
                    report.ledger.accounts.map((a) => (
                      <TableRow key={a.account}>
                        <TableCell className="font-mono text-xs">
                          {a.account}
                        </TableCell>
                        <TableCell>
                          {formatMoney(a.posted_net, "NGN")}
                        </TableCell>
                        <TableCell className="pr-2 text-right">
                          {formatMoney(a.pending_net, "NGN")}
                        </TableCell>
                      </TableRow>
                    ))
                  )}
                </TableBody>
              </Table>
            </div>

            <div>
              <p className="mb-2 text-sm font-medium">
                Payout attempts by state (reserved in flight:{" "}
                {formatMoney(report.payout_attempts.reserved_cents, "NGN")})
              </p>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>State</TableHead>
                    <TableHead>Count</TableHead>
                    <TableHead className="pr-2 text-right">Amount</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {report.payout_attempts.by_state.length === 0 ? (
                    <TableEmpty colSpan={3}>
                      No payout attempts recorded yet.
                    </TableEmpty>
                  ) : (
                    report.payout_attempts.by_state.map((r) => (
                      <TableRow key={r.state}>
                        <TableCell className="font-medium">{r.state}</TableCell>
                        <TableCell>{r.count}</TableCell>
                        <TableCell className="pr-2 text-right">
                          {formatMoney(r.amount_cents, "NGN")}
                        </TableCell>
                      </TableRow>
                    ))
                  )}
                </TableBody>
              </Table>
            </div>

            {report.payout_attempts.in_flight.length > 0 ? (
              <div>
                <p className="mb-2 text-sm font-medium">
                  In-flight attempts (oldest first)
                </p>
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Payout</TableHead>
                      <TableHead>State</TableHead>
                      <TableHead>Amount</TableHead>
                      <TableHead className="pr-2 text-right">
                        Created
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {report.payout_attempts.in_flight.map((a) => (
                      <TableRow key={a.payout_id}>
                        <TableCell className="font-mono text-xs">
                          {a.payout_id}
                        </TableCell>
                        <TableCell>{a.state}</TableCell>
                        <TableCell>
                          {formatMoney(a.amount_cents, a.currency)}
                        </TableCell>
                        <TableCell className="pr-2 text-right text-xs text-muted-foreground">
                          {new Date(a.created_at).toLocaleString()}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            ) : null}

            <div>
              <Button variant="secondary" onClick={() => void load()}>
                Refresh
              </Button>
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  );
}
