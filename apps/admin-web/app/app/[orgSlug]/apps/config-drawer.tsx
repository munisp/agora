"use client";

/**
 * Per-app configuration drawer (SPEC-W18 Agent B, contract §2). Slides in
 * from the right (same overlay/Escape idiom as components/ui/dialog.tsx —
 * there is no drawer primitive) and edits the app's tenant-scoped config
 * jsonb as raw JSON:
 *   - client-side validation: the editor must contain a JSON *object*
 *     (tenant_apps.config is jsonb DEFAULT '{}') before any request is made;
 *   - server errors surface inline and keep the drawer open;
 *   - the actual save (optimistic-but-verified PATCH) lives in
 *     apps-client.tsx and is passed in as `onSave`.
 */
import * as React from "react";
import { X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Label, Textarea } from "@/components/ui/input";
import { StatusPill } from "@/components/apps/status-pill";
import { TierBadge } from "@/components/apps/tier-badge";
import { formatDateTime, titleCase } from "@/lib/utils";
import type { TenantApp } from "@/lib/types";

export function ConfigDrawer({
  app,
  open,
  onOpenChange,
  canManage,
  onSave,
}: {
  /** the app being configured; null while closed */
  app: TenantApp | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** owner/admin only — read-only otherwise */
  canManage: boolean;
  /** throws on server failure — the drawer stays open and shows the reason */
  onSave: (app: TenantApp, config: Record<string, unknown>) => Promise<void>;
}) {
  const [text, setText] = React.useState("{}");
  const [clientError, setClientError] = React.useState<string | null>(null);
  const [serverError, setServerError] = React.useState<string | null>(null);
  const [saving, setSaving] = React.useState(false);

  // Re-seed the editor whenever a different app is opened.
  React.useEffect(() => {
    if (open && app) {
      setText(JSON.stringify(app.config ?? {}, null, 2));
      setClientError(null);
      setServerError(null);
    }
  }, [open, app]);

  // Escape closes — same idiom as components/ui/dialog.tsx.
  React.useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onOpenChange(false);
    };
    document.addEventListener("keydown", onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = prev;
    };
  }, [open, onOpenChange]);

  if (!open || !app) return null;

  const save = async () => {
    setClientError(null);
    setServerError(null);
    let parsed: unknown;
    try {
      parsed = JSON.parse(text);
    } catch (e) {
      setClientError(
        `Invalid JSON: ${e instanceof Error ? e.message : String(e)}`,
      );
      return;
    }
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
      setClientError("Config must be a JSON object, e.g. { \"key\": \"value\" }.");
      return;
    }
    setSaving(true);
    try {
      await onSave(app, parsed as Record<string, unknown>);
      onOpenChange(false);
    } catch (e) {
      setServerError(e instanceof Error ? e.message : "Save failed.");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="fixed inset-0 z-50">
      <div
        className="absolute inset-0 bg-foreground/40"
        aria-hidden
        onClick={() => onOpenChange(false)}
      />
      <div
        role="dialog"
        aria-modal="true"
        aria-label={`Configure ${app.name}`}
        className="absolute inset-y-0 right-0 flex w-full max-w-md flex-col border-l border-border bg-card shadow-lg"
      >
        <div className="flex items-start justify-between gap-3 border-b border-border p-5">
          <div className="flex items-start gap-3">
            <span
              aria-hidden
              className="flex h-10 w-10 shrink-0 items-center justify-center rounded-md bg-secondary text-xl"
            >
              {app.icon || "📦"}
            </span>
            <div>
              <h2 className="text-lg font-semibold leading-none">{app.name}</h2>
              <p className="mt-1 text-xs text-muted-foreground">
                {app.app_id}
                {app.version ? ` · v${app.version}` : ""} ·{" "}
                {titleCase(app.category || "uncategorised")}
              </p>
              <div className="mt-2 flex flex-wrap items-center gap-2">
                <StatusPill status={app.status} />
                <TierBadge tier={app.default_plan_tier} />
              </div>
            </div>
          </div>
          <button
            onClick={() => onOpenChange(false)}
            aria-label="Close"
            className="rounded-sm text-muted-foreground transition-opacity hover:opacity-70 cursor-pointer"
          >
            <X className="h-4 w-4" />
          </button>
        </div>

        <div className="flex-1 space-y-4 overflow-y-auto p-5">
          {app.description ? (
            <p className="text-sm text-muted-foreground">{app.description}</p>
          ) : null}

          <dl className="grid grid-cols-2 gap-3 text-sm">
            <div>
              <dt className="text-xs text-muted-foreground">Provisioned</dt>
              <dd className="font-medium">
                {app.provisioned_at
                  ? formatDateTime(app.provisioned_at)
                  : "—"}
              </dd>
            </div>
            <div>
              <dt className="text-xs text-muted-foreground">Provisioned by</dt>
              <dd className="font-medium">{app.provisioned_by || "—"}</dd>
            </div>
            <div>
              <dt className="text-xs text-muted-foreground">Last updated</dt>
              <dd className="font-medium">
                {app.updated_at ? formatDateTime(app.updated_at) : "—"}
              </dd>
            </div>
            <div>
              <dt className="text-xs text-muted-foreground">Required perms</dt>
              <dd className="font-medium">
                {app.required_perms?.length ? app.required_perms.join(", ") : "—"}
              </dd>
            </div>
          </dl>

          <div className="grid gap-1.5">
            <Label htmlFor="app-config-json">Configuration (JSON)</Label>
            <Textarea
              id="app-config-json"
              value={text}
              onChange={(e) => {
                setText(e.target.value);
                setClientError(null);
              }}
              rows={14}
              spellCheck={false}
              disabled={!canManage || saving}
              className="font-mono text-xs"
            />
            {clientError ? (
              <p className="text-xs text-destructive">{clientError}</p>
            ) : (
              <p className="text-xs text-muted-foreground">
                Tenant-scoped settings for this app (config jsonb). Saved via
                PATCH /v1/tenants/{`{slug}`}/apps/{`{app_id}`} — the server
                validates again.
              </p>
            )}
            {serverError ? (
              <p className="rounded-md border border-destructive/40 bg-danger-soft px-3 py-2 text-xs text-destructive">
                Server rejected the config: {serverError}
              </p>
            ) : null}
          </div>
        </div>

        <div className="flex justify-end gap-2 border-t border-border p-5">
          <Button
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={saving}
          >
            {canManage ? "Cancel" : "Close"}
          </Button>
          {canManage ? (
            <Button onClick={() => void save()} disabled={saving}>
              {saving ? "Saving…" : "Save config"}
            </Button>
          ) : null}
        </div>
      </div>
    </div>
  );
}
