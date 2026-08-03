"use client";

/**
 * Apps management portal (SPEC-W18 Agent B, contract §2): the full app
 * catalog for this tenant with lifecycle actions and per-app config.
 *
 * All traffic goes through the BFF with the SAME /api/identity/... path
 * style the settings page uses, and ONLY the endpoints the SPEC-W18
 * contract §1 defines (the W15 settings PATCH bug — calling an endpoint
 * identity doesn't implement — is not replicated here):
 *   GET    /api/identity/v1/tenants/{slug}/apps              (catalog LEFT
 *          JOIN tenant_apps — every app with status|not_provisioned + config)
 *   POST   /api/identity/v1/tenants/{slug}/apps/{app_id}     (provision+enable)
 *   PATCH  /api/identity/v1/tenants/{slug}/apps/{app_id}     ({status?, config?})
 *   DELETE /api/identity/v1/tenants/{slug}/apps/{app_id}     (not used by the
 *          UI — disable is the soft-delete the contract specifies)
 *
 * Mutations are owner/admin only (`canManage`, computed server-side from the
 * session in page.tsx — mirrors the W14 growth role-guard pattern); the
 * backend enforces the same rule. List responses are read through the W13
 * tolerant unwrap<T>() envelope convention (components/apps/types.ts).
 */
import * as React from "react";
import { RefreshCw, Search } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { Input, Label, Select } from "@/components/ui/input";
import { ConfirmDialog } from "@/components/ui/dialog";
import { useToast } from "@/components/ui/toast";
import { AppCard } from "@/components/apps/app-card";
import { unwrap } from "@/components/apps/types";
import { ConfigDrawer } from "./config-drawer";
import type { TenantApp } from "@/lib/types";

export function AppsClient({
  orgSlug,
  canManage,
}: {
  orgSlug: string;
  /** owner/admin only (server-computed in page.tsx) */
  canManage: boolean;
}) {
  const { toast } = useToast();
  const [apps, setApps] = React.useState<TenantApp[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);

  const [search, setSearch] = React.useState("");
  const [category, setCategory] = React.useState("all");

  const [busyId, setBusyId] = React.useState<string | null>(null);
  const [disableTarget, setDisableTarget] = React.useState<TenantApp | null>(
    null,
  );
  const [configTarget, setConfigTarget] = React.useState<TenantApp | null>(
    null,
  );

  const base = `/api/identity/v1/tenants/${orgSlug}/apps`;

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<unknown>(base, undefined, signal);
        if (signal?.aborted) return;
        setApps(unwrap<TenantApp>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setApps([]);
        setError(
          e instanceof ApiError
            ? e.message
            : "Failed to load the app catalog — the identity service may be offline, or the apps registry (SPEC-W18) is not deployed yet.",
        );
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [base],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  const provision = async (app: TenantApp) => {
    setBusyId(app.app_id);
    try {
      await api.post(`${base}/${app.app_id}`);
      toast({ title: `${app.name} provisioned`, variant: "success" });
      await load();
    } catch (e) {
      toast({
        title: `Could not provision ${app.name}`,
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusyId(null);
    }
  };

  const setStatus = async (app: TenantApp, status: "enabled" | "disabled") => {
    setBusyId(app.app_id);
    try {
      await api.patch(`${base}/${app.app_id}`, { status });
      toast({
        title: status === "enabled" ? `${app.name} enabled` : `${app.name} disabled`,
        variant: "success",
      });
      await load();
    } catch (e) {
      toast({
        title: `Could not update ${app.name}`,
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
      await load();
    } finally {
      setBusyId(null);
      setDisableTarget(null);
    }
  };

  /**
   * Optimistic-but-verified config save: patch local state immediately,
   * PATCH the server, then re-fetch the list as the authoritative check —
   * on any failure the optimistic edit is rolled back and the reason is
   * thrown back to the drawer (which stays open and shows it inline).
   */
  const saveConfig = async (
    app: TenantApp,
    config: Record<string, unknown>,
  ) => {
    const previous = apps;
    setApps((cur) =>
      cur.map((a) => (a.app_id === app.app_id ? { ...a, config } : a)),
    );
    try {
      await api.patch(`${base}/${app.app_id}`, { config });
    } catch (e) {
      setApps(previous);
      throw e instanceof ApiError
        ? e
        : new Error("Failed to save the config — please try again.");
    }
    try {
      // Verified: reconcile with the server's view of the world.
      const data = await api.get<unknown>(base);
      setApps(unwrap<TenantApp>(data));
      toast({ title: `${app.name} config saved`, variant: "success" });
    } catch {
      toast({
        title: `${app.name} config saved`,
        description:
          "The save succeeded but the list could not be refreshed to verify it.",
        variant: "warning",
      });
    }
  };

  const categories = React.useMemo(
    () =>
      Array.from(new Set(apps.map((a) => a.category).filter(Boolean))).sort(),
    [apps],
  );

  const filtered = React.useMemo(() => {
    const q = search.trim().toLowerCase();
    return apps.filter((a) => {
      if (category !== "all" && a.category !== category) return false;
      if (!q) return true;
      return [a.name, a.app_id, a.category, a.description ?? ""]
        .join(" ")
        .toLowerCase()
        .includes(q);
    });
  }, [apps, search, category]);

  return (
    <div className="max-w-6xl">
      <PageHeader
        title="Apps"
        description="The OpenDesk app catalog for this organisation — provision apps, toggle them on or off, and edit their per-tenant configuration. Disabling an app always retains its data."
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

      {error ? <ErrorNote message={error} /> : null}

      <div className="mb-4 flex flex-wrap items-end gap-3">
        <div className="grid min-w-64 flex-1 gap-1.5">
          <Label htmlFor="apps-search">Search</Label>
          <div className="relative">
            <Search className="pointer-events-none absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
            <Input
              id="apps-search"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search by name, id or description…"
              className="pl-8"
            />
          </div>
        </div>
        <div className="grid w-52 gap-1.5">
          <Label htmlFor="apps-category">Category</Label>
          <Select
            id="apps-category"
            value={category}
            onChange={(e) => setCategory(e.target.value)}
          >
            <option value="all">All categories</option>
            {categories.map((c) => (
              <option key={c} value={c}>
                {c}
              </option>
            ))}
          </Select>
        </div>
      </div>

      {loading ? (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {Array.from({ length: 6 }).map((_, i) => (
            <div
              key={i}
              className="h-44 animate-pulse rounded-lg border border-border bg-muted"
            />
          ))}
        </div>
      ) : error ? (
        <p className="rounded-md border border-border bg-card px-4 py-6 text-sm text-muted-foreground">
          The catalog could not be loaded (reason above). Use Refresh to retry
          — nothing is shown rather than a misleading empty grid.
        </p>
      ) : apps.length === 0 ? (
        <p className="rounded-md border border-border bg-card px-4 py-6 text-sm text-muted-foreground">
          The app catalog is empty — the identity service answered, but no
          platform apps are seeded yet (SPEC-W18 catalog seed).
        </p>
      ) : filtered.length === 0 ? (
        <p className="rounded-md border border-border bg-card px-4 py-6 text-sm text-muted-foreground">
          No apps match {search.trim() ? `“${search.trim()}”` : "this filter"}
          {category !== "all" ? ` in category “${category}”` : ""}.
        </p>
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {filtered.map((app) => (
            <AppCard
              key={app.app_id}
              app={app}
              canManage={canManage}
              busy={busyId === app.app_id}
              onProvision={(a) => void provision(a)}
              onEnable={(a) => void setStatus(a, "enabled")}
              onDisable={(a) => setDisableTarget(a)}
              onConfigure={(a) => setConfigTarget(a)}
            />
          ))}
        </div>
      )}

      <ConfirmDialog
        open={disableTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDisableTarget(null);
        }}
        title={`Disable ${disableTarget?.name ?? "app"}?`}
        description="The app stops working for this organisation immediately. Existing data is retained and the app can be re-enabled at any time."
        confirmLabel="Disable app"
        busy={busyId !== null}
        onConfirm={() => {
          if (disableTarget) void setStatus(disableTarget, "disabled");
        }}
      />

      <ConfigDrawer
        app={configTarget}
        open={configTarget !== null}
        onOpenChange={(open) => {
          if (!open) setConfigTarget(null);
        }}
        canManage={canManage}
        onSave={saveConfig}
      />
    </div>
  );
}
