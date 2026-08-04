"use client";

/**
 * Social Publisher app client (SPEC-W21 Agent B): accounts (provider +
 * political-authorization badges), creatives editor, posts queue (publish
 * buttons + status pills), ads wizard (objective, ₦ budgets from kobo,
 * targeting, the political toggle that forces the disclaimer and shows the
 * authorization requirement inline) and ad stats tiles.
 *
 * Data sources (all through the BFF with the x-tenant-slug header
 * attached, mirroring the handlers in internal/socialpub):
 *   - GET/POST   /api/bookings/v1/social/accounts
 *   - PATCH      /api/bookings/v1/social/accounts/{id}
 *   - GET/POST   /api/bookings/v1/social/creatives
 *   - PATCH      /api/bookings/v1/social/creatives/{id}
 *   - GET/POST   /api/bookings/v1/social/posts
 *   - POST       /api/bookings/v1/social/posts/{id}/publish
 *   - GET/POST   /api/bookings/v1/social/ads
 *   - PATCH      /api/bookings/v1/social/ads/{id}
 *   - POST       /api/bookings/v1/social/ads/{id}/launch
 *   - GET        /api/bookings/v1/social/ads/{id}/stats
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
  type AdStatsResponse,
  type LaunchResponse,
  type SocialAccount,
  type SocialAd,
  type SocialCreative,
  type SocialPost,
} from "@/components/apps/social-publisher/types";
import {
  AccountEditorDialog,
  AccountsTable,
  draftFromAccount,
  emptyAccountDraft,
  type AccountDraft,
} from "@/components/apps/social-publisher/accounts-panel";
import {
  CreativeEditorDialog,
  CreativesTable,
  draftFromCreative,
  emptyCreativeDraft,
  type CreativeDraft,
} from "@/components/apps/social-publisher/creatives-editor";
import {
  PostCreateDialog,
  PostsTable,
} from "@/components/apps/social-publisher/posts-queue";
import {
  AdEditorDialog,
  AdsTable,
  draftFromAd,
  emptyAdDraft,
  splitList,
  type AdDraft,
} from "@/components/apps/social-publisher/ads-wizard";
import { StatsTiles } from "@/components/apps/social-publisher/stats-tiles";

const ROLLOUT_NOTE =
  "Social Publisher is not available yet — the booking-service social API may still be rolling out.";

function errMsg(e: unknown): string {
  // ApiError's message already surfaces the backend's {error} body (the
  // gate messages: 422 political gate, 409 account/transition).
  return e instanceof ApiError ? e.message : "Unexpected error";
}

export function SocialPublisherClient({
  orgSlug,
  canManage,
}: {
  orgSlug: string;
  canManage: boolean;
}) {
  const { toast } = useToast();

  // Accounts
  const [accounts, setAccounts] = React.useState<SocialAccount[]>([]);
  const [accountsLoading, setAccountsLoading] = React.useState(true);
  const [accountsError, setAccountsError] = React.useState<string | null>(null);
  const [editingAccount, setEditingAccount] = React.useState<AccountDraft | null>(null);

  // Creatives
  const [creatives, setCreatives] = React.useState<SocialCreative[]>([]);
  const [creativesLoading, setCreativesLoading] = React.useState(true);
  const [creativesError, setCreativesError] = React.useState<string | null>(null);
  const [editingCreative, setEditingCreative] = React.useState<CreativeDraft | null>(null);

  // Posts
  const [posts, setPosts] = React.useState<SocialPost[]>([]);
  const [postsLoading, setPostsLoading] = React.useState(true);
  const [postsError, setPostsError] = React.useState<string | null>(null);
  const [postStatusFilter, setPostStatusFilter] = React.useState("");
  const [creatingPost, setCreatingPost] = React.useState(false);

  // Ads
  const [ads, setAds] = React.useState<SocialAd[]>([]);
  const [adsLoading, setAdsLoading] = React.useState(true);
  const [adsError, setAdsError] = React.useState<string | null>(null);
  const [adStatusFilter, setAdStatusFilter] = React.useState("");
  const [editingAd, setEditingAd] = React.useState<AdDraft | null>(null);
  const [stats, setStats] = React.useState<AdStatsResponse | null>(null);
  const [statsLoading, setStatsLoading] = React.useState(false);

  const [busy, setBusy] = React.useState(false);
  const [busyId, setBusyId] = React.useState<string | null>(null);

  // ---------------------------------------------------------------------
  // Loads
  // ---------------------------------------------------------------------

  const loadAccounts = React.useCallback(
    async (signal?: AbortSignal) => {
      setAccountsLoading(true);
      setAccountsError(null);
      try {
        const data = await api.get<unknown>("/api/bookings/v1/social/accounts", {
          tenant: orgSlug,
        });
        if (signal?.aborted) return;
        setAccounts(unwrap<SocialAccount>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setAccounts([]);
        setAccountsError(
          e instanceof ApiError && e.status !== 404 ? errMsg(e) : ROLLOUT_NOTE,
        );
      } finally {
        if (!signal?.aborted) setAccountsLoading(false);
      }
    },
    [orgSlug],
  );

  const loadCreatives = React.useCallback(
    async (signal?: AbortSignal) => {
      setCreativesLoading(true);
      setCreativesError(null);
      try {
        const data = await api.get<unknown>("/api/bookings/v1/social/creatives", {
          tenant: orgSlug,
        });
        if (signal?.aborted) return;
        setCreatives(unwrap<SocialCreative>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setCreatives([]);
        setCreativesError(
          e instanceof ApiError && e.status !== 404 ? errMsg(e) : ROLLOUT_NOTE,
        );
      } finally {
        if (!signal?.aborted) setCreativesLoading(false);
      }
    },
    [orgSlug],
  );

  const loadPosts = React.useCallback(
    async (status: string, signal?: AbortSignal) => {
      setPostsLoading(true);
      setPostsError(null);
      try {
        const data = await api.get<unknown>("/api/bookings/v1/social/posts", {
          tenant: orgSlug,
          status,
        });
        if (signal?.aborted) return;
        setPosts(unwrap<SocialPost>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setPosts([]);
        setPostsError(
          e instanceof ApiError && e.status !== 404 ? errMsg(e) : ROLLOUT_NOTE,
        );
      } finally {
        if (!signal?.aborted) setPostsLoading(false);
      }
    },
    [orgSlug],
  );

  const loadAds = React.useCallback(
    async (status: string, signal?: AbortSignal) => {
      setAdsLoading(true);
      setAdsError(null);
      try {
        const data = await api.get<unknown>("/api/bookings/v1/social/ads", {
          tenant: orgSlug,
          status,
        });
        if (signal?.aborted) return;
        setAds(unwrap<SocialAd>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setAds([]);
        setAdsError(
          e instanceof ApiError && e.status !== 404 ? errMsg(e) : ROLLOUT_NOTE,
        );
      } finally {
        if (!signal?.aborted) setAdsLoading(false);
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    const c = new AbortController();
    void loadAccounts(c.signal);
    return () => c.abort();
  }, [loadAccounts]);
  React.useEffect(() => {
    const c = new AbortController();
    void loadCreatives(c.signal);
    return () => c.abort();
  }, [loadCreatives]);
  React.useEffect(() => {
    const c = new AbortController();
    void loadPosts(postStatusFilter, c.signal);
    return () => c.abort();
  }, [postStatusFilter, loadPosts]);
  React.useEffect(() => {
    const c = new AbortController();
    void loadAds(adStatusFilter, c.signal);
    return () => c.abort();
  }, [adStatusFilter, loadAds]);

  const reloadAll = async () => {
    await Promise.all([
      loadAccounts(),
      loadCreatives(),
      loadPosts(postStatusFilter),
      loadAds(adStatusFilter),
    ]);
  };

  // ---------------------------------------------------------------------
  // Account mutations
  // ---------------------------------------------------------------------

  const saveAccount = async (d: AccountDraft): Promise<boolean> => {
    setBusy(true);
    try {
      if (d.id) {
        await api.patch(
          `/api/bookings/v1/social/accounts/${d.id}`,
          {
            account_ref: d.account_ref,
            display_name: d.display_name,
            status: d.status,
            political_ads_authorized: d.political_ads_authorized,
          },
          { tenant: orgSlug },
        );
      } else {
        await api.post(
          "/api/bookings/v1/social/accounts",
          {
            provider: d.provider,
            account_ref: d.account_ref,
            display_name: d.display_name,
            political_ads_authorized: d.political_ads_authorized,
          },
          { tenant: orgSlug },
        );
      }
      toast({ title: d.id ? "Account updated" : "Account connected", variant: "success" });
      await loadAccounts();
      return true;
    } catch (e) {
      toast({ title: "Save failed", description: errMsg(e), variant: "destructive" });
      return false;
    } finally {
      setBusy(false);
    }
  };

  // ---------------------------------------------------------------------
  // Creative mutations
  // ---------------------------------------------------------------------

  const saveCreative = async (d: CreativeDraft): Promise<boolean> => {
    setBusy(true);
    try {
      const body = {
        name: d.name,
        kind: d.kind,
        body: d.body,
        media_url: d.media_url.trim() === "" ? null : d.media_url.trim(),
        disclaimer_text: d.disclaimer_text.trim() === "" ? null : d.disclaimer_text.trim(),
      };
      if (d.id) {
        await api.patch(`/api/bookings/v1/social/creatives/${d.id}`, body, {
          tenant: orgSlug,
        });
      } else {
        await api.post("/api/bookings/v1/social/creatives", body, { tenant: orgSlug });
      }
      toast({ title: d.id ? "Creative updated" : "Creative created", variant: "success" });
      await loadCreatives();
      return true;
    } catch (e) {
      toast({ title: "Save failed", description: errMsg(e), variant: "destructive" });
      return false;
    } finally {
      setBusy(false);
    }
  };

  // ---------------------------------------------------------------------
  // Post mutations
  // ---------------------------------------------------------------------

  const createPost = async (
    accountId: string,
    creativeId: string,
    status: string,
  ): Promise<boolean> => {
    setBusy(true);
    try {
      await api.post(
        "/api/bookings/v1/social/posts",
        { account_id: accountId, creative_id: creativeId, status },
        { tenant: orgSlug },
      );
      toast({ title: "Post queued", variant: "success" });
      await loadPosts(postStatusFilter);
      return true;
    } catch (e) {
      toast({ title: "Create failed", description: errMsg(e), variant: "destructive" });
      return false;
    } finally {
      setBusy(false);
    }
  };

  const publishPost = async (p: SocialPost) => {
    setBusyId(p.id);
    try {
      await api.post(
        `/api/bookings/v1/social/posts/${p.id}/publish`,
        {},
        { tenant: orgSlug },
      );
      toast({ title: "Post published", variant: "success" });
    } catch (e) {
      // 409 (expired/revoked account) and 502 (provider failure) both land
      // here with the backend's honest message.
      toast({ title: "Publish failed", description: errMsg(e), variant: "destructive" });
    } finally {
      setBusyId(null);
      await loadPosts(postStatusFilter);
    }
  };

  // ---------------------------------------------------------------------
  // Ad mutations
  // ---------------------------------------------------------------------

  const saveAd = async (d: AdDraft): Promise<boolean> => {
    setBusy(true);
    try {
      const body = {
        name: d.name,
        objective: d.objective,
        budget_kobo: d.budget_kobo,
        daily_budget_kobo: d.daily_budget_kobo,
        targeting: {
          lgas: splitList(d.lgas),
          age_min: d.age_min,
          age_max: d.age_max,
          interests: splitList(d.interests),
        },
        political: d.political,
        disclaimer_text: d.disclaimer_text.trim() === "" ? null : d.disclaimer_text.trim(),
      };
      if (d.id) {
        await api.patch(`/api/bookings/v1/social/ads/${d.id}`, body, { tenant: orgSlug });
      } else {
        await api.post(
          "/api/bookings/v1/social/ads",
          { ...body, account_id: d.account_id, creative_id: d.creative_id },
          { tenant: orgSlug },
        );
      }
      toast({ title: d.id ? "Ad updated" : "Ad created", variant: "success" });
      await loadAds(adStatusFilter);
      return true;
    } catch (e) {
      toast({ title: "Save failed", description: errMsg(e), variant: "destructive" });
      return false;
    } finally {
      setBusy(false);
    }
  };

  const launchAd = async (a: SocialAd) => {
    setBusyId(a.id);
    try {
      const resp = await api.post<LaunchResponse>(
        `/api/bookings/v1/social/ads/${a.id}/launch`,
        {},
        { tenant: orgSlug },
      );
      if (resp.rejected) {
        toast({
          title: "Ad rejected by provider review",
          description: resp.reason ?? "See the ad row for details",
          variant: "warning",
        });
      } else {
        toast({ title: "Ad launched — now in review", variant: "success" });
      }
    } catch (e) {
      // 422 (political gate) / 409 (account not connected / not draft)
      // land here with the backend's gate message.
      toast({ title: "Launch blocked", description: errMsg(e), variant: "destructive" });
    } finally {
      setBusyId(null);
      await loadAds(adStatusFilter);
    }
  };

  const setAdStatus = async (a: SocialAd, status: string) => {
    setBusyId(a.id);
    try {
      await api.patch(
        `/api/bookings/v1/social/ads/${a.id}`,
        { status },
        { tenant: orgSlug },
      );
      toast({ title: `Ad ${status}`, variant: "success" });
    } catch (e) {
      toast({ title: "Update failed", description: errMsg(e), variant: "destructive" });
    } finally {
      setBusyId(null);
      await loadAds(adStatusFilter);
    }
  };

  const loadStats = async (a: SocialAd) => {
    setStatsLoading(true);
    setStats(null);
    try {
      const data = await api.get<AdStatsResponse>(
        `/api/bookings/v1/social/ads/${a.id}/stats`,
        { tenant: orgSlug },
      );
      setStats(data);
    } catch (e) {
      toast({ title: "Stats unavailable", description: errMsg(e), variant: "destructive" });
    } finally {
      setStatsLoading(false);
    }
  };

  // ---------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------

  const statusFilterBar = (
    value: string,
    onChange: (v: string) => void,
    options: readonly string[],
  ) => (
    <div className="mb-3 flex flex-wrap gap-2">
      <Button
        size="sm"
        variant={value === "" ? "default" : "outline"}
        onClick={() => onChange("")}
      >
        All
      </Button>
      {options.map((s) => (
        <Button
          key={s}
          size="sm"
          variant={value === s ? "default" : "outline"}
          onClick={() => onChange(s)}
        >
          {s}
        </Button>
      ))}
    </div>
  );

  return (
    <div className="space-y-6">
      <PageHeader
        title="Social Publisher"
        description="Publish posts and run paid ads on Meta, TikTok and X — with the political-ads gates enforced (authorized account + disclaimer). Provider mocks are the default; no OAuth yet (see docs)."
        actions={
          <Button variant="outline" size="sm" onClick={() => void reloadAll()}>
            <RefreshCw className="mr-2 h-4 w-4" />
            Refresh
          </Button>
        }
      />

      <Tabs defaultValue="accounts">
        <TabsList>
          <TabsTrigger value="accounts">Accounts</TabsTrigger>
          <TabsTrigger value="creatives">Creatives</TabsTrigger>
          <TabsTrigger value="posts">Posts</TabsTrigger>
          <TabsTrigger value="ads">Ads</TabsTrigger>
        </TabsList>

        <TabsContent value="accounts">
          <div className="mb-3 flex justify-end">
            {canManage ? (
              <Button size="sm" onClick={() => setEditingAccount(emptyAccountDraft())}>
                Connect account
              </Button>
            ) : null}
          </div>
          {accountsError ? <ErrorNote message={accountsError} /> : null}
          <AccountsTable
            accounts={accounts}
            loading={accountsLoading}
            canManage={canManage}
            onEdit={(a) => setEditingAccount(draftFromAccount(a))}
          />
        </TabsContent>

        <TabsContent value="creatives">
          <div className="mb-3 flex justify-end">
            {canManage ? (
              <Button size="sm" onClick={() => setEditingCreative(emptyCreativeDraft())}>
                New creative
              </Button>
            ) : null}
          </div>
          {creativesError ? <ErrorNote message={creativesError} /> : null}
          <CreativesTable
            creatives={creatives}
            loading={creativesLoading}
            canManage={canManage}
            onEdit={(c) => setEditingCreative(draftFromCreative(c))}
          />
        </TabsContent>

        <TabsContent value="posts">
          <div className="mb-3 flex items-center justify-between">
            {statusFilterBar(postStatusFilter, setPostStatusFilter, [
              "draft",
              "queued",
              "published",
              "failed",
            ])}
            {canManage ? (
              <Button
                size="sm"
                disabled={accounts.length === 0 || creatives.length === 0}
                title={
                  accounts.length === 0 || creatives.length === 0
                    ? "Connect an account and create a creative first"
                    : undefined
                }
                onClick={() => setCreatingPost(true)}
              >
                Queue post
              </Button>
            ) : null}
          </div>
          {postsError ? <ErrorNote message={postsError} /> : null}
          <PostsTable
            posts={posts}
            accounts={accounts}
            creatives={creatives}
            loading={postsLoading}
            busyId={busyId}
            canManage={canManage}
            onPublish={(p) => void publishPost(p)}
          />
        </TabsContent>

        <TabsContent value="ads">
          <div className="mb-3 flex items-center justify-between">
            {statusFilterBar(adStatusFilter, setAdStatusFilter, [
              "draft",
              "review",
              "active",
              "paused",
              "rejected",
            ])}
            {canManage ? (
              <Button
                size="sm"
                disabled={accounts.length === 0 || creatives.length === 0}
                title={
                  accounts.length === 0 || creatives.length === 0
                    ? "Connect an account and create a creative first"
                    : undefined
                }
                onClick={() =>
                  setEditingAd(emptyAdDraft(accounts[0]?.id ?? "", creatives[0]?.id ?? ""))
                }
              >
                New ad
              </Button>
            ) : null}
          </div>
          {adsError ? <ErrorNote message={adsError} /> : null}
          <AdsTable
            ads={ads}
            accounts={accounts}
            creatives={creatives}
            loading={adsLoading}
            busyId={busyId}
            canManage={canManage}
            onEdit={(a) => setEditingAd(draftFromAd(a))}
            onLaunch={(a) => void launchAd(a)}
            onSetStatus={(a, s) => void setAdStatus(a, s)}
            onStats={(a) => void loadStats(a)}
          />
          {statsLoading ? (
            <p className="mt-4 text-sm text-muted-foreground">Loading stats…</p>
          ) : null}
          {stats ? (
            <div className="mt-4">
              <StatsTiles stats={stats} />
            </div>
          ) : null}
        </TabsContent>
      </Tabs>

      {editingAccount ? (
        <AccountEditorDialog
          draft={editingAccount}
          busy={busy}
          onSave={saveAccount}
          onCancel={() => setEditingAccount(null)}
        />
      ) : null}
      {editingCreative ? (
        <CreativeEditorDialog
          draft={editingCreative}
          busy={busy}
          onSave={saveCreative}
          onCancel={() => setEditingCreative(null)}
        />
      ) : null}
      {creatingPost ? (
        <PostCreateDialog
          accounts={accounts}
          creatives={creatives}
          busy={busy}
          onCreate={createPost}
          onCancel={() => setCreatingPost(false)}
        />
      ) : null}
      {editingAd ? (
        <AdEditorDialog
          draft={editingAd}
          accounts={accounts}
          creatives={creatives}
          busy={busy}
          onSave={saveAd}
          onCancel={() => setEditingAd(null)}
        />
      ) : null}
    </div>
  );
}
