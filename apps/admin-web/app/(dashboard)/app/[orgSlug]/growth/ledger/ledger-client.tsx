"use client";

/**
 * Commission ledger page client (SPEC-W14 Agent C, contract §3): date-window
 * selector + double-entry ledger grouped by journal.
 *
 * Data source (BFF with x-tenant-slug header):
 *   - GET /api/bookings/v1/commissions/ledger?from&to
 */
import * as React from "react";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { addDays, toISODate } from "@/lib/utils";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { Input, Label } from "@/components/ui/input";
import { GrowthTabs } from "@/components/growth/growth-tabs";
import { LedgerTable } from "@/components/growth/ledger-table";
import { unwrap, type LedgerEntry } from "@/components/growth/types";

export function LedgerClient({
  orgSlug,
  canAnalytics,
  canBilling,
}: {
  orgSlug: string;
  canAnalytics: boolean;
  canBilling: boolean;
}) {
  const today = React.useMemo(() => new Date(), []);
  const [from, setFrom] = React.useState(() =>
    toISODate(addDays(today, -29)),
  );
  const [to, setTo] = React.useState(() => toISODate(today));
  const [entries, setEntries] = React.useState<LedgerEntry[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<unknown>(
          "/api/bookings/v1/commissions/ledger",
          { tenant: orgSlug, from, to },
        );
        if (signal?.aborted) return;
        setEntries(unwrap<LedgerEntry>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setEntries([]);
        setError(
          e instanceof ApiError && e.status !== 404
            ? e.message
            : "The commission ledger is not available yet — the booking-service referrals API may still be rolling out.",
        );
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [orgSlug, from, to],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  return (
    <div className="max-w-6xl">
      <PageHeader
        title="Commission ledger"
        description="Double-entry commission postings: every bounty is a balanced pair (expense 301 / payable 300), every payout settles payable against agent float (302) or house clearing (303)."
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

      <div className="mb-4 flex flex-wrap items-end gap-3">
        <div className="space-y-1.5">
          <Label htmlFor="ledger-from">From</Label>
          <Input
            id="ledger-from"
            type="date"
            value={from}
            max={to}
            onChange={(e) => setFrom(e.target.value)}
            className="w-40"
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="ledger-to">To</Label>
          <Input
            id="ledger-to"
            type="date"
            value={to}
            min={from}
            onChange={(e) => setTo(e.target.value)}
            className="w-40"
          />
        </div>
      </div>

      <LedgerTable entries={entries} loading={loading} from={from} to={to} />
    </div>
  );
}
