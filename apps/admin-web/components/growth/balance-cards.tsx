"use client";

/**
 * Commission balance cards (SPEC-W14 Agent C): outstanding payable balance
 * per beneficiary — the sum of account-300 credits minus debits as reported
 * by GET /v1/commissions/balance/{beneficiary}. Presentational.
 */
import { Wallet } from "lucide-react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { formatNgn } from "@/components/growth/types";

export interface BeneficiaryBalance {
  beneficiary_id: string;
  /** kobo; null when the balance read failed for this beneficiary */
  balanceKobo: number | null;
}

export function BalanceCards({
  rows,
  loading,
}: {
  rows: BeneficiaryBalance[];
  loading: boolean;
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Outstanding balances</CardTitle>
        <CardDescription>
          Unpaid commission payable per beneficiary (ledger account 300:
          credits minus debits).
        </CardDescription>
      </CardHeader>
      <CardContent>
        {loading ? (
          <p className="py-6 text-center text-sm text-muted-foreground">
            Loading balances…
          </p>
        ) : rows.length === 0 ? (
          <p className="py-6 text-center text-sm text-muted-foreground">
            No beneficiaries with payout activity yet.
          </p>
        ) : (
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            {rows.map((row) => (
              <div
                key={row.beneficiary_id}
                className="rounded-md border border-border p-3"
              >
                <div className="flex items-center justify-between">
                  <p className="truncate text-sm font-medium">
                    {row.beneficiary_id}
                  </p>
                  <Wallet className="h-4 w-4 shrink-0 text-muted-foreground" />
                </div>
                <p className="mt-1 text-xl font-bold">
                  {formatNgn(row.balanceKobo)}
                </p>
                <p className="text-xs text-muted-foreground">
                  {row.balanceKobo === null
                    ? "Balance read unavailable"
                    : "Outstanding payable"}
                </p>
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
