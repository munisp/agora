"use client";

/**
 * CAC dashboards (SPEC-W13 Agent D): blended CAC / payback / LTV cards,
 * by-channel table with period-over-period CAC trend, by-LGA choropleth and
 * top promo codes / campaign spend.
 *
 * Data sources (all through the BFF with the tenant header attached):
 *   - GET /api/analytics/v1/cac/summary?from&to   (contract §5; the BFF
 *     special-case forwards /api/analytics/* to the analytics service, same
 *     as /api/analytics/v1/recommendations on the billing page)
 *   - GET /api/bookings/v1/promo                  (contract §6 list — optional)
 *   - GET /api/bookings/v1/campaigns              (contract §4 list — optional)
 *
 * The trend column needs no extra backend surface: the previous period of
 * equal length is fetched from the same §5 endpoint and compared client-side.
 */
import * as React from "react";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { addDays, cn, toISODate } from "@/lib/utils";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { CacSummaryCards } from "@/components/cac/cac-summary-cards";
import { CacChannelTable } from "@/components/cac/cac-channel-table";
import { CacLgaSection } from "@/components/cac/cac-lga-section";
import { CacPromoTables } from "@/components/cac/cac-promo-table";
import type {
  CacSummary,
  CampaignRow,
  PromoCodeRow,
} from "@/components/cac/types";

const PERIODS = [
  { days: 7, label: "7d" },
  { days: 30, label: "30d" },
  { days: 90, label: "90d" },
];

/**
 * Tolerant list unwrap. booking-service list endpoints answer with keyed
 * envelopes (GET /v1/promo → {"promo_codes":[...]}, GET /v1/campaigns →
 * {"campaigns":[...]}), while other services use {items:[...]} or a bare
 * array — accept all of them by taking the first own-property value that is
 * an array; anything else yields [].
 */
function unwrap<T>(data: unknown): T[] {
  if (Array.isArray(data)) return data as T[];
  if (typeof data === "object" && data !== null) {
    for (const value of Object.values(data)) {
      if (Array.isArray(value)) return value as T[];
    }
  }
  return [];
}

export function CacClient({ orgSlug }: { orgSlug: string }) {
  const [days, setDays] = React.useState(30);

  const [summary, setSummary] = React.useState<CacSummary | null>(null);
  const [previous, setPrevious] = React.useState<CacSummary | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);

  const [promos, setPromos] = React.useState<PromoCodeRow[]>([]);
  const [campaigns, setCampaigns] = React.useState<CampaignRow[]>([]);
  const [promosLoading, setPromosLoading] = React.useState(true);
  const [promosUnavailable, setPromosUnavailable] = React.useState<
    string | null
  >(null);
  const [campaignsUnavailable, setCampaignsUnavailable] = React.useState<
    string | null
  >(null);

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      const to = new Date();
      const from = addDays(to, -(days - 1));
      // Previous window of equal length, ending the day before `from`.
      const prevTo = addDays(from, -1);
      const prevFrom = addDays(prevTo, -(days - 1));
      try {
        const [current, prior] = await Promise.all([
          api.get<CacSummary>("/api/analytics/v1/cac/summary", {
            tenant: orgSlug,
            from: toISODate(from),
            to: toISODate(to),
          }),
          api.get<CacSummary>("/api/analytics/v1/cac/summary", {
            tenant: orgSlug,
            from: toISODate(prevFrom),
            to: toISODate(prevTo),
          }),
        ]);
        if (signal?.aborted) return;
        setSummary(current);
        setPrevious(prior);
      } catch (e) {
        if (signal?.aborted) return;
        setError(
          e instanceof ApiError
            ? e.message
            : "Failed to load CAC metrics — the analytics service may be offline.",
        );
        setSummary(null);
        setPrevious(null);
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [orgSlug, days],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  // Optional reads: promo codes and campaign spend. These list endpoints are
  // additive on Agent A's side — until they ship, the tables show a muted
  // note instead of failing the page.
  React.useEffect(() => {
    let settled = 0;
    const done = () => {
      settled += 1;
      if (settled === 2) setPromosLoading(false);
    };
    (async () => {
      try {
        const data = await api.get<unknown>("/api/bookings/v1/promo", {
          tenant: orgSlug,
        });
        setPromos(unwrap<PromoCodeRow>(data));
        setPromosUnavailable(null);
      } catch (e) {
        setPromos([]);
        setPromosUnavailable(
          e instanceof ApiError && e.status !== 404
            ? `Promo codes unavailable: ${e.message}`
            : "Promo code listing is not available yet.",
        );
      } finally {
        done();
      }
    })();
    (async () => {
      try {
        const data = await api.get<unknown>("/api/bookings/v1/campaigns", {
          tenant: orgSlug,
        });
        setCampaigns(unwrap<CampaignRow>(data));
        setCampaignsUnavailable(null);
      } catch (e) {
        setCampaigns([]);
        setCampaignsUnavailable(
          e instanceof ApiError && e.status !== 404
            ? `Campaigns unavailable: ${e.message}`
            : "Campaign listing is not available yet.",
        );
      } finally {
        done();
      }
    })();
  }, [orgSlug]);

  return (
    <div className="max-w-6xl">
      <PageHeader
        title="Customer acquisition cost"
        description="What each new customer costs you — blended and per channel, with payback estimate, LGA breakdown and promo-code performance. Realtime rollup, reconciled nightly against the lakehouse gold tables."
        actions={
          <div className="flex items-center gap-2">
            <div className="flex gap-1 rounded-md border border-border p-0.5">
              {PERIODS.map((p) => (
                <button
                  key={p.days}
                  type="button"
                  onClick={() => setDays(p.days)}
                  className={cn(
                    "rounded px-2.5 py-1 text-xs font-medium cursor-pointer",
                    days === p.days
                      ? "bg-secondary text-secondary-foreground"
                      : "text-muted-foreground hover:text-foreground",
                  )}
                >
                  {p.label}
                </button>
              ))}
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void load()}
              disabled={loading}
            >
              <RefreshCw className="h-3.5 w-3.5" />
              {loading ? "Loading…" : "Refresh"}
            </Button>
          </div>
        }
      />

      {error ? <ErrorNote message={error} /> : null}

      <div className="mb-4">
        <CacSummaryCards summary={summary} loading={loading} days={days} />
      </div>

      <div className="mb-4">
        <CacChannelTable
          rows={summary?.by_channel ?? []}
          previousRows={previous?.by_channel ?? []}
          loading={loading}
          days={days}
        />
      </div>

      <div className="mb-4">
        <CacLgaSection
          rows={summary?.by_lga ?? []}
          loading={loading}
          days={days}
        />
      </div>

      <CacPromoTables
        promos={promos}
        campaigns={campaigns}
        loading={promosLoading}
        promosUnavailable={promosUnavailable}
        campaignsUnavailable={campaignsUnavailable}
      />
    </div>
  );
}
