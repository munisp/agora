"use client";

/**
 * Commission payouts page client (SPEC-W14 Agent C, contract §4): payout
 * queue plus outstanding commission balance per beneficiary.
 *
 * Data sources (BFF with x-tenant-slug header):
 *   - GET /api/bookings/v1/payouts
 *   - GET /api/bookings/v1/commissions/balance/{beneficiary}  (one call per
 *     beneficiary seen in the queue; per-beneficiary failures render "—"
 *     rather than failing the page — same soft-failure style as the CAC
 *     page's optional reads)
 */
import * as React from "react";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { GrowthTabs } from "@/components/growth/growth-tabs";
import { PayoutQueue } from "@/components/growth/payout-queue";
import {
  BalanceCards,
  type BeneficiaryBalance,
} from "@/components/growth/balance-cards";
import {
  extractBalance,
  unwrap,
  type Payout,
} from "@/components/growth/types";

export function PayoutsClient({
  orgSlug,
  canAnalytics,
  canBilling,
}: {
  orgSlug: string;
  canAnalytics: boolean;
  canBilling: boolean;
}) {
  const [payouts, setPayouts] = React.useState<Payout[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [balances, setBalances] = React.useState<BeneficiaryBalance[]>([]);
  const [balancesLoading, setBalancesLoading] = React.useState(true);

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<unknown>("/api/bookings/v1/payouts", {
          tenant: orgSlug,
        });
        if (signal?.aborted) return;
        setPayouts(unwrap<Payout>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setPayouts([]);
        setError(
          e instanceof ApiError && e.status !== 404
            ? e.message
            : "Payouts are not available yet — the booking-service referrals API may still be rolling out.",
        );
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  // Balances for every beneficiary seen in the queue. Each balance read is
  // individually fault-tolerant (null = unavailable).
  React.useEffect(() => {
    if (loading) return;
    const beneficiaries = [
      ...new Set(payouts.map((p) => p.beneficiary_id).filter(Boolean)),
    ].sort();
    if (beneficiaries.length === 0) {
      setBalances([]);
      setBalancesLoading(false);
      return;
    }
    let cancelled = false;
    setBalancesLoading(true);
    (async () => {
      const rows = await Promise.all(
        beneficiaries.map(async (id): Promise<BeneficiaryBalance> => {
          try {
            const data = await api.get<unknown>(
              `/api/bookings/v1/commissions/balance/${encodeURIComponent(id)}`,
              { tenant: orgSlug },
            );
            return { beneficiary_id: id, balanceKobo: extractBalance(data) };
          } catch {
            return { beneficiary_id: id, balanceKobo: null };
          }
        }),
      );
      if (!cancelled) {
        setBalances(rows);
        setBalancesLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [orgSlug, payouts, loading]);

  return (
    <div className="max-w-6xl">
      <PageHeader
        title="Commission payouts"
        description="Payout queue execution and outstanding commission payable per beneficiary. Payouts run as Temporal activities with their own retries; nightly recon compares the ledger against provider transfer status."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => void load()}
            disabled={loading}
          >
            <RefreshCw className="h-3.5 w-3.5" />
            {loading ? "Loading…" : "Refresh"}
          </Button>
        }
      />
      <GrowthTabs
        orgSlug={orgSlug}
        canAnalytics={canAnalytics}
        canBilling={canBilling}
      />
      {error ? <ErrorNote message={error} /> : null}

      <div className="mb-4">
        <PayoutQueue rows={payouts} loading={loading} />
      </div>

      <BalanceCards rows={balances} loading={loading || balancesLoading} />
    </div>
  );
}
