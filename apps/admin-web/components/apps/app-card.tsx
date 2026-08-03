"use client";

/**
 * One catalog app card in the /apps grid (SPEC-W18 Agent B, contract §2):
 * icon, name, category, tier badge and status pill, plus the owner/admin
 * lifecycle actions. Presentational — all mutations live in the parent
 * (apps-client.tsx) so busy/error handling stays in one place.
 */
import { PackageOpen, Power, PowerOff, Settings2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardFooter,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { StatusPill } from "@/components/apps/status-pill";
import { TierBadge } from "@/components/apps/tier-badge";
import { titleCase } from "@/lib/utils";
import type { TenantApp } from "@/lib/types";

export function AppCard({
  app,
  canManage,
  busy,
  onProvision,
  onEnable,
  onDisable,
  onConfigure,
}: {
  app: TenantApp;
  /** owner/admin only — lifecycle actions are hidden for everyone else */
  canManage: boolean;
  /** a mutation for this app is in flight */
  busy: boolean;
  onProvision: (app: TenantApp) => void;
  onEnable: (app: TenantApp) => void;
  /** opens the confirm dialog (existing data is retained) */
  onDisable: (app: TenantApp) => void;
  onConfigure: (app: TenantApp) => void;
}) {
  const provisioned = app.status !== "not_provisioned";
  return (
    <Card className="flex flex-col">
      <CardHeader className="flex-row items-start justify-between gap-3 space-y-0">
        <div className="flex items-start gap-3">
          <span
            aria-hidden
            className="flex h-10 w-10 shrink-0 items-center justify-center rounded-md bg-secondary text-xl"
          >
            {app.icon || "📦"}
          </span>
          <div>
            <CardTitle className="text-base">{app.name}</CardTitle>
            <p className="mt-0.5 text-xs text-muted-foreground">
              {titleCase(app.category || "uncategorised")}
              {app.version ? ` · v${app.version}` : ""}
            </p>
          </div>
        </div>
        <StatusPill status={app.status} />
      </CardHeader>
      <CardContent className="flex-1 space-y-3">
        {app.description ? (
          <p className="text-sm text-muted-foreground line-clamp-3">
            {app.description}
          </p>
        ) : null}
        <div className="flex flex-wrap items-center gap-2">
          <TierBadge tier={app.default_plan_tier} />
        </div>
        {app.backend_note ? (
          // Data-driven hint (no hardcoded app list): for the apps whose
          // backends land in a later wave the note itself says so
          // ("backend module lands W19/W20" — the catalog's "module ships
          // separately" signal); for shipped apps it names the backing
          // modules. Rendered as-is, subtly, whenever present.
          <p className="flex items-start gap-1.5 text-xs text-muted-foreground">
            <PackageOpen className="mt-0.5 h-3.5 w-3.5 shrink-0" />
            <span>{app.backend_note}</span>
          </p>
        ) : null}
        {app.status === "suspended" ? (
          <p className="text-xs text-warning">
            Suspended by the platform — contact support to reinstate.
          </p>
        ) : null}
      </CardContent>
      {canManage ? (
        <CardFooter className="flex-wrap gap-2">
          {app.status === "not_provisioned" ? (
            <Button size="sm" onClick={() => onProvision(app)} disabled={busy}>
              {busy ? "Working…" : "Provision"}
            </Button>
          ) : null}
          {app.status === "disabled" ? (
            <Button size="sm" onClick={() => onEnable(app)} disabled={busy}>
              <Power className="h-3.5 w-3.5" />
              {busy ? "Working…" : "Enable"}
            </Button>
          ) : null}
          {app.status === "enabled" ? (
            <Button
              size="sm"
              variant="outline"
              onClick={() => onDisable(app)}
              disabled={busy}
            >
              <PowerOff className="h-3.5 w-3.5" />
              Disable
            </Button>
          ) : null}
          {provisioned ? (
            <Button
              size="sm"
              variant="outline"
              onClick={() => onConfigure(app)}
              disabled={busy}
            >
              <Settings2 className="h-3.5 w-3.5" />
              Configure
            </Button>
          ) : null}
        </CardFooter>
      ) : null}
    </Card>
  );
}
