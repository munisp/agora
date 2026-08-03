"use client";

/**
 * Referrals page client (SPEC-W14 Agent C): referral list + create form +
 * verify action, and the per-referrer leaderboard (ranked by
 * verified+converted counts, paid totals from paid payouts).
 *
 * Data sources (all through the BFF with the x-tenant-slug header attached):
 *   - GET  /api/bookings/v1/referrals                (contract §1 list)
 *   - POST /api/bookings/v1/referrals                (contract §1 create)
 *   - POST /api/bookings/v1/referrals/{id}/verify    (fires rules → ledger;
 *     body {trigger, base_amount_ngn} — trigger required, base in kobo)
 *   - GET  /api/bookings/v1/payouts                  (optional: leaderboard
 *     paid totals — soft-fails like the CAC page's promo/campaign reads)
 */
import * as React from "react";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { useToast } from "@/components/ui/toast";
import { GrowthTabs } from "@/components/growth/growth-tabs";
import {
  ReferralCreateForm,
  type NewReferral,
} from "@/components/growth/referral-create-form";
import { ReferralTable } from "@/components/growth/referral-table";
import { ReferrerLeaderboard } from "@/components/growth/leaderboard";
import {
  buildLeaderboard,
  unwrap,
  type Payout,
  type Referral,
} from "@/components/growth/types";

export function ReferralsClient({
  orgSlug,
  canAnalytics,
  canBilling,
}: {
  orgSlug: string;
  canAnalytics: boolean;
  canBilling: boolean;
}) {
  const { toast } = useToast();
  const [referrals, setReferrals] = React.useState<Referral[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [creating, setCreating] = React.useState(false);
  const [verifyingId, setVerifyingId] = React.useState<string | null>(null);

  // Optional: paid totals for the leaderboard. null = unavailable.
  const [payouts, setPayouts] = React.useState<Payout[] | null>(null);

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<unknown>("/api/bookings/v1/referrals", {
          tenant: orgSlug,
        });
        if (signal?.aborted) return;
        setReferrals(unwrap<Referral>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setReferrals([]);
        setError(
          e instanceof ApiError && e.status !== 404
            ? e.message
            : "Referrals are not available yet — the booking-service referrals API may still be rolling out.",
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

  // Optional payouts read for leaderboard paid totals (soft failure: the
  // leaderboard renders "—" in the paid-total column instead of failing).
  React.useEffect(() => {
    (async () => {
      try {
        const data = await api.get<unknown>("/api/bookings/v1/payouts", {
          tenant: orgSlug,
        });
        setPayouts(unwrap<Payout>(data));
      } catch {
        setPayouts(null);
      }
    })();
  }, [orgSlug]);

  const create = async (input: NewReferral): Promise<boolean> => {
    setCreating(true);
    try {
      await api.post("/api/bookings/v1/referrals", input, { tenant: orgSlug });
      toast({ title: "Referral recorded", variant: "success" });
      await load();
      return true;
    } catch (e) {
      toast({
        title: "Failed to record referral",
        description:
          e instanceof ApiError
            ? e.message
            : "The referrals service may be offline.",
        variant: "destructive",
      });
      return false;
    } finally {
      setCreating(false);
    }
  };

  const verify = async (
    referral: Referral,
    trigger: string,
    baseAmountKobo: number,
  ) => {
    setVerifyingId(referral.referral_id);
    try {
      // Server contract: {trigger, base_amount_ngn} — trigger is required
      // (empty body → 400), base is the integer-kobo revenue base that
      // percent rules on first_txn/sale are computed against.
      await api.post(
        `/api/bookings/v1/referrals/${referral.referral_id}/verify`,
        { trigger, base_amount_ngn: baseAmountKobo },
        { tenant: orgSlug },
      );
      toast({
        title: "Referral verified",
        description:
          "Commission rules were evaluated and the bounty posted to the ledger.",
        variant: "success",
      });
      await load();
    } catch (e) {
      toast({
        title: "Verification failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setVerifyingId(null);
    }
  };

  const leaderboard = React.useMemo(
    () => buildLeaderboard(referrals, payouts),
    [referrals, payouts],
  );

  return (
    <div className="max-w-6xl">
      <PageHeader
        title="Referrals"
        description="Who referred each new customer, referral status, and the referrer leaderboard. Verifying a referral fires the commission rules and posts a balanced bounty to the ledger."
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
        <ReferralCreateForm busy={creating} onCreate={create} />
      </div>

      <div className="mb-4">
        <ReferralTable
          rows={referrals}
          loading={loading}
          verifyingId={verifyingId}
          onVerify={(r, trigger, baseAmountKobo) =>
            void verify(r, trigger, baseAmountKobo)
          }
        />
      </div>

      <ReferrerLeaderboard
        rows={leaderboard}
        loading={loading}
        payoutsAvailable={payouts !== null}
      />
    </div>
  );
}
