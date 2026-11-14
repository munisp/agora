"use client";

/**
 * Loyalty & Wallet app client (SPEC-W19 Agent C): program editor (friendly
 * form over the earn_rules/tiers jsonb + raw JSON fallback), wallet lookup
 * with ledger table and accrue/redeem actions, and the leaderboard.
 *
 * Data sources (all through the BFF with the x-tenant-slug header
 * attached, mirroring the handlers in internal/loyalty):
 *   - GET   /api/bookings/v1/loyalty/programs
 *   - POST  /api/bookings/v1/loyalty/programs
 *   - PATCH /api/bookings/v1/loyalty/programs/{id}
 *   - GET   /api/bookings/v1/loyalty/wallets/{contact_id}
 *   - POST  /api/bookings/v1/loyalty/accrue
 *   - POST  /api/bookings/v1/loyalty/redeem
 *   - GET   /api/bookings/v1/loyalty/leaderboard?metric=
 */
import * as React from "react";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { useToast } from "@/components/ui/toast";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  unwrap,
  formatPoints,
  type AccrueResponse,
  type LeaderboardEntry,
  type Program,
  type RedeemResponse,
  type WalletView,
} from "@/components/apps/loyalty-wallet/types";
import {
  draftFromProgram,
  emptyDraft,
  ProgramCard,
  ProgramEditor,
  type ProgramDraft,
} from "@/components/apps/loyalty-wallet/program-editor";
import {
  WalletActions,
  WalletCards,
  WalletLedgerTable,
  WalletLookup,
  type AccrueInput,
  type RedeemInput,
} from "@/components/apps/loyalty-wallet/wallet-view";
import { Leaderboard } from "@/components/apps/loyalty-wallet/leaderboard";

const ROLLOUT_NOTE =
  "Loyalty & Wallet is not available yet — the booking-service loyalty API may still be rolling out.";

export function LoyaltyWalletClient({
  orgSlug,
  canManage,
}: {
  orgSlug: string;
  canManage: boolean;
}) {
  const { toast } = useToast();
  const [programs, setPrograms] = React.useState<Program[]>([]);
  const [programsLoading, setProgramsLoading] = React.useState(true);
  const [programsError, setProgramsError] = React.useState<string | null>(null);
  const [editing, setEditing] = React.useState<ProgramDraft | null>(null);
  const [saving, setSaving] = React.useState(false);

  const [walletView, setWalletView] = React.useState<WalletView | null>(null);
  const [walletLoading, setWalletLoading] = React.useState(false);
  const [walletError, setWalletError] = React.useState<string | null>(null);
  const [actionBusy, setActionBusy] = React.useState(false);

  const [lbEntries, setLbEntries] = React.useState<LeaderboardEntry[]>([]);
  const [lbLoading, setLbLoading] = React.useState(true);
  const [lbMetric, setLbMetric] = React.useState("lifetime_earned");

  const loadPrograms = React.useCallback(
    async (signal?: AbortSignal) => {
      setProgramsLoading(true);
      setProgramsError(null);
      try {
        const data = await api.get<unknown>("/api/bookings/v1/loyalty/programs", {
          tenant: orgSlug,
        });
        if (signal?.aborted) return;
        setPrograms(unwrap<Program>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setPrograms([]);
        setProgramsError(
          e instanceof ApiError && e.status !== 404 ? e.message : ROLLOUT_NOTE,
        );
      } finally {
        if (!signal?.aborted) setProgramsLoading(false);
      }
    },
    [orgSlug],
  );

  const loadLeaderboard = React.useCallback(
    async (metric: string, signal?: AbortSignal) => {
      setLbLoading(true);
      try {
        const data = await api.get<unknown>(
          "/api/bookings/v1/loyalty/leaderboard",
          { tenant: orgSlug, metric },
        );
        if (signal?.aborted) return;
        setLbEntries(unwrap<LeaderboardEntry>(data));
      } catch {
        if (!signal?.aborted) setLbEntries([]);
      } finally {
        if (!signal?.aborted) setLbLoading(false);
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    const c = new AbortController();
    void loadPrograms(c.signal);
    return () => c.abort();
  }, [loadPrograms]);

  React.useEffect(() => {
    const c = new AbortController();
    void loadLeaderboard(lbMetric, c.signal);
    return () => c.abort();
  }, [lbMetric, loadLeaderboard]);

  const saveProgram = async (draft: ProgramDraft): Promise<boolean> => {
    setSaving(true);
    try {
      const body = {
        name: draft.name,
        active: draft.active,
        earn_rules: draft.earn_rules,
        tiers: draft.tiers,
        cap_per_day: draft.cap_per_day,
      };
      if (draft.program_id) {
        await api.patch(
          `/api/bookings/v1/loyalty/programs/${draft.program_id}`,
          body,
          { tenant: orgSlug },
        );
      } else {
        await api.post("/api/bookings/v1/loyalty/programs", body, {
          tenant: orgSlug,
        });
      }
      toast({
        title: draft.program_id ? "Program updated" : "Program created",
        variant: "success",
      });
      setEditing(null);
      await loadPrograms();
      return true;
    } catch (e) {
      toast({
        title: "Save failed",
        description: e instanceof ApiError ? e.message : "Unexpected error",
        variant: "destructive",
      });
      return false;
    } finally {
      setSaving(false);
    }
  };

  const toggleProgram = async (p: Program, active: boolean) => {
    try {
      await api.patch(
        `/api/bookings/v1/loyalty/programs/${p.program_id}`,
        { active },
        { tenant: orgSlug },
      );
      await loadPrograms();
    } catch (e) {
      toast({
        title: "Update failed",
        description: e instanceof ApiError ? e.message : "Unexpected error",
        variant: "destructive",
      });
    }
  };

  const lookupWallet = React.useCallback(
    async (contactID: string) => {
      setWalletLoading(true);
      setWalletError(null);
      try {
        const data = await api.get<WalletView>(
          `/api/bookings/v1/loyalty/wallets/${contactID}`,
          { tenant: orgSlug },
        );
        setWalletView(data);
      } catch (e) {
        setWalletView(null);
        setWalletError(
          e instanceof ApiError && e.status === 404
            ? "No wallet for this contact yet — it is created on the first accrual."
            : e instanceof ApiError
              ? e.message
              : ROLLOUT_NOTE,
        );
      } finally {
        setWalletLoading(false);
      }
    },
    [orgSlug],
  );

  const refreshWalletAndBoard = async () => {
    if (walletView) await lookupWallet(walletView.wallet.contact_id);
    await loadLeaderboard(lbMetric);
  };

  const accrue = async (input: AccrueInput) => {
    setActionBusy(true);
    try {
      const res = await api.post<AccrueResponse>(
        "/api/bookings/v1/loyalty/accrue",
        input,
        { tenant: orgSlug },
      );
      toast({
        title: res.applied
          ? `Awarded ${formatPoints(res.awarded)} pts${res.capped ? " (daily cap applied)" : ""}`
          : "Already applied — this ref_id + event was accrued before",
        variant: "success",
      });
      await refreshWalletAndBoard();
    } catch (e) {
      toast({
        title: "Accrual failed",
        description: e instanceof ApiError ? e.message : "Unexpected error",
        variant: "destructive",
      });
    } finally {
      setActionBusy(false);
    }
  };

  const redeem = async (input: RedeemInput) => {
    setActionBusy(true);
    try {
      const res = await api.post<RedeemResponse>(
        "/api/bookings/v1/loyalty/redeem",
        input,
        { tenant: orgSlug },
      );
      toast({
        title: res.applied
          ? `Redeemed ${formatPoints(res.redeemed)} pts`
          : "Already applied — this ref_id was redeemed before",
        variant: "success",
      });
      await refreshWalletAndBoard();
    } catch (e) {
      toast({
        title: "Redemption failed",
        description:
          e instanceof ApiError
            ? e.status === 409
              ? `${e.message} — top up the balance first.`
              : e.message
            : "Unexpected error",
        variant: "destructive",
      });
    } finally {
      setActionBusy(false);
    }
  };

  return (
    <div className="max-w-6xl">
      <PageHeader
        title="Loyalty & Wallet"
        description="Points programs, wallets and redemption: earn rules award points per event, tiers recompute from lifetime earned, every movement is a balanced double-entry journal."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              void loadPrograms();
              void loadLeaderboard(lbMetric);
            }}
            disabled={programsLoading}
          >
            <RefreshCw className="h-3.5 w-3.5" />
            {programsLoading ? "Loading…" : "Refresh"}
          </Button>
        }
      />

      <Tabs defaultValue="programs">
        <TabsList className="mb-4">
          <TabsTrigger value="programs">Programs</TabsTrigger>
          <TabsTrigger value="wallets">Wallets</TabsTrigger>
          <TabsTrigger value="leaderboard">Leaderboard</TabsTrigger>
        </TabsList>

        <TabsContent value="programs">
          {programsError ? <ErrorNote message={programsError} /> : null}
          {editing ? (
            <ProgramEditor
              initial={editing}
              busy={saving}
              onSave={saveProgram}
              onCancel={() => setEditing(null)}
            />
          ) : (
            <div className="space-y-3">
              {canManage ? (
                <div>
                  <Button size="sm" onClick={() => setEditing(emptyDraft())}>
                    New program
                  </Button>
                </div>
              ) : null}
              {programs.map((p) => (
                <ProgramCard
                  key={p.program_id}
                  program={p}
                  canManage={canManage}
                  onEdit={() => setEditing(draftFromProgram(p))}
                  onToggle={(active) => void toggleProgram(p, active)}
                />
              ))}
              {!programsLoading && programs.length === 0 && !programsError ? (
                <p className="text-sm text-muted-foreground">
                  No loyalty programs yet
                  {canManage ? " — create one to start awarding points." : "."}
                </p>
              ) : null}
              {programsLoading ? (
                <p className="text-sm text-muted-foreground">
                  Loading programs…
                </p>
              ) : null}
            </div>
          )}
        </TabsContent>

        <TabsContent value="wallets">
          <WalletLookup loading={walletLoading} onLookup={(id) => void lookupWallet(id)} />
          {walletError ? <ErrorNote message={walletError} /> : null}
          {walletView ? (
            <>
              <WalletCards view={walletView} />
              <WalletActions
                contactID={walletView.wallet.contact_id}
                canManage={canManage}
                busy={actionBusy}
                onAccrue={accrue}
                onRedeem={redeem}
              />
              <WalletLedgerTable
                entries={walletView.entries}
                loading={walletLoading}
              />
            </>
          ) : null}
        </TabsContent>

        <TabsContent value="leaderboard">
          <Leaderboard
            entries={lbEntries}
            loading={lbLoading}
            metric={lbMetric}
            onMetricChange={setLbMetric}
          />
        </TabsContent>
      </Tabs>
    </div>
  );
}
