"use client";

/**
 * Ads wizard (SPEC-W21 Agent B): objective picker, budget fields (kobo
 * with ₦ hints), targeting form (LGAs, age band 18..100, interests), the
 * political toggle that FORCES the disclaimer field and shows the
 * authorization requirement inline, launch button (422/409 gates surface
 * via toast) and the active⇄paused operator controls.
 *
 * Data: GET/POST /api/bookings/v1/social/ads,
 *       PATCH /api/bookings/v1/social/ads/{id},
 *       POST /api/bookings/v1/social/ads/{id}/launch
 */
import * as React from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input, Label, Select } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  AD_OBJECTIVES,
  AD_STATUS_META,
  effectiveDisclaimer,
  formatKobo,
  formatTs,
  launchGateBlocker,
  type SocialAccount,
  type SocialAd,
  type SocialCreative,
} from "@/components/apps/social-publisher/types";

export interface AdDraft {
  id?: string;
  account_id: string;
  creative_id: string;
  name: string;
  objective: string;
  budget_kobo: number;
  daily_budget_kobo: number;
  lgas: string; // comma-separated in the form
  age_min: number;
  age_max: number;
  interests: string; // comma-separated in the form
  political: boolean;
  disclaimer_text: string;
}

export function emptyAdDraft(accountId: string, creativeId: string): AdDraft {
  return {
    account_id: accountId,
    creative_id: creativeId,
    name: "",
    objective: "awareness",
    budget_kobo: 500000,
    daily_budget_kobo: 100000,
    lgas: "",
    age_min: 18,
    age_max: 65,
    interests: "",
    political: false,
    disclaimer_text: "",
  };
}

export function draftFromAd(a: SocialAd): AdDraft {
  return {
    id: a.id,
    account_id: a.account_id,
    creative_id: a.creative_id,
    name: a.name,
    objective: a.objective,
    budget_kobo: a.budget_kobo,
    daily_budget_kobo: a.daily_budget_kobo,
    lgas: (a.targeting?.lgas ?? []).join(", "),
    age_min: a.targeting?.age_min ?? 18,
    age_max: a.targeting?.age_max ?? 65,
    interests: (a.targeting?.interests ?? []).join(", "),
    political: a.political,
    disclaimer_text: a.disclaimer_text ?? "",
  };
}

export function splitList(v: string): string[] {
  return v
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

function nairaHint(kobo: number): string {
  if (!Number.isFinite(kobo) || kobo <= 0) return "";
  return `≈ ${formatKobo(kobo)}`;
}

export function AdsTable({
  ads,
  accounts,
  creatives,
  loading,
  busyId,
  canManage,
  onEdit,
  onLaunch,
  onSetStatus,
  onStats,
}: {
  ads: SocialAd[];
  accounts: SocialAccount[];
  creatives: SocialCreative[];
  loading: boolean;
  busyId: string | null;
  canManage: boolean;
  onEdit: (a: SocialAd) => void;
  onLaunch: (a: SocialAd) => void;
  onSetStatus: (a: SocialAd, status: string) => void;
  onStats: (a: SocialAd) => void;
}) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Ad</TableHead>
          <TableHead>Budget</TableHead>
          <TableHead>Targeting</TableHead>
          <TableHead>Political</TableHead>
          <TableHead>Status</TableHead>
          {canManage ? <TableHead className="text-right">Actions</TableHead> : null}
        </TableRow>
      </TableHeader>
      <TableBody>
        {ads.map((a) => {
          const sm = AD_STATUS_META[a.status] ?? { label: a.status, variant: "outline" as const };
          const acct = accounts.find((x) => x.id === a.account_id);
          const creative = creatives.find((x) => x.id === a.creative_id);
          const blocker = launchGateBlocker(a, acct, creative);
          const editable = a.status === "draft" || a.status === "review" || a.status === "rejected";
          return (
            <TableRow key={a.id}>
              <TableCell>
                <div className="font-medium">{a.name}</div>
                <div className="text-xs text-muted-foreground">
                  {a.objective} · {acct?.display_name ?? "—"}
                </div>
                {a.error ? <div className="text-xs text-destructive">{a.error}</div> : null}
              </TableCell>
              <TableCell className="text-sm">
                <div>{formatKobo(a.budget_kobo)}</div>
                <div className="text-xs text-muted-foreground">
                  {formatKobo(a.daily_budget_kobo)}/day
                </div>
              </TableCell>
              <TableCell className="text-sm text-muted-foreground">
                {a.targeting?.age_min}–{a.targeting?.age_max}
                {(a.targeting?.lgas?.length ?? 0) > 0
                  ? ` · ${a.targeting.lgas.join(", ")}`
                  : ""}
              </TableCell>
              <TableCell>
                {a.political ? (
                  effectiveDisclaimer(a, creative) ? (
                    <Badge variant="warning">Political</Badge>
                  ) : (
                    <Badge variant="destructive">Political — no disclaimer</Badge>
                  )
                ) : (
                  <Badge variant="outline">No</Badge>
                )}
              </TableCell>
              <TableCell>
                <Badge variant={sm.variant}>{sm.label}</Badge>
              </TableCell>
              {canManage ? (
                <TableCell className="text-right">
                  <div className="flex justify-end gap-2">
                    {a.status === "draft" ? (
                      <>
                        <Button size="sm" variant="outline" onClick={() => onEdit(a)}>
                          Edit
                        </Button>
                        <Button
                          size="sm"
                          disabled={busyId === a.id || blocker !== null}
                          title={blocker ?? undefined}
                          onClick={() => onLaunch(a)}
                        >
                          {busyId === a.id ? "Launching…" : "Launch"}
                        </Button>
                      </>
                    ) : null}
                    {editable && a.status !== "draft" ? (
                      <Button size="sm" variant="outline" onClick={() => onEdit(a)}>
                        Edit
                      </Button>
                    ) : null}
                    {a.status === "active" ? (
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={busyId === a.id}
                        onClick={() => onSetStatus(a, "paused")}
                      >
                        Pause
                      </Button>
                    ) : null}
                    {a.status === "paused" ? (
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={busyId === a.id}
                        onClick={() => onSetStatus(a, "active")}
                      >
                        Resume
                      </Button>
                    ) : null}
                    {a.provider_ad_id ? (
                      <Button size="sm" variant="ghost" onClick={() => onStats(a)}>
                        Stats
                      </Button>
                    ) : null}
                  </div>
                </TableCell>
              ) : null}
            </TableRow>
          );
        })}
        {!loading && ads.length === 0 ? (
          <TableEmpty colSpan={canManage ? 6 : 5}>
            No ads yet — create one and launch it (political ads are gated:
            authorized account + disclaimer required).
          </TableEmpty>
        ) : null}
      </TableBody>
    </Table>
  );
}

export function AdEditorDialog({
  draft,
  accounts,
  creatives,
  busy,
  onSave,
  onCancel,
}: {
  draft: AdDraft;
  accounts: SocialAccount[];
  creatives: SocialCreative[];
  busy: boolean;
  onSave: (d: AdDraft) => Promise<boolean>;
  onCancel: () => void;
}) {
  const [d, setD] = React.useState<AdDraft>(draft);
  const isNew = !draft.id;
  const account = accounts.find((a) => a.id === d.account_id);
  const creative = creatives.find((c) => c.id === d.creative_id);
  const discOk =
    !d.political ||
    d.disclaimer_text.trim() !== "" ||
    (creative?.disclaimer_text?.trim() ?? "") !== "";
  const num =
    (key: "budget_kobo" | "daily_budget_kobo" | "age_min" | "age_max") =>
    (e: React.ChangeEvent<HTMLInputElement>) =>
      setD({ ...d, [key]: Number(e.target.value) });
  return (
    <Dialog open onOpenChange={(open) => (!open ? onCancel() : undefined)}>
      <DialogContent className="max-h-[90vh] overflow-y-auto">
        <DialogHeader>
          <DialogTitle>{isNew ? "New ad" : "Edit ad"}</DialogTitle>
          <DialogDescription>
            Objective, budget (₦, stored as kobo) and targeting. Launch is a
            separate gated step from the ads table.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3 py-2">
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label htmlFor="ad-account">Account</Label>
              <Select
                id="ad-account"
                value={d.account_id}
                disabled={!isNew}
                onChange={(e) => setD({ ...d, account_id: e.target.value })}
              >
                {accounts.map((a) => (
                  <option key={a.id} value={a.id}>
                    {a.display_name} ({a.provider})
                  </option>
                ))}
              </Select>
            </div>
            <div>
              <Label htmlFor="ad-creative">Creative</Label>
              <Select
                id="ad-creative"
                value={d.creative_id}
                disabled={!isNew}
                onChange={(e) => setD({ ...d, creative_id: e.target.value })}
              >
                {creatives.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.name} ({c.kind})
                  </option>
                ))}
              </Select>
            </div>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label htmlFor="ad-name">Name</Label>
              <Input
                id="ad-name"
                value={d.name}
                onChange={(e) => setD({ ...d, name: e.target.value })}
              />
            </div>
            <div>
              <Label htmlFor="ad-objective">Objective</Label>
              <Select
                id="ad-objective"
                value={d.objective}
                onChange={(e) => setD({ ...d, objective: e.target.value })}
              >
                {AD_OBJECTIVES.map((o) => (
                  <option key={o} value={o}>
                    {o}
                  </option>
                ))}
              </Select>
            </div>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label htmlFor="ad-budget">Total budget (kobo)</Label>
              <Input
                id="ad-budget"
                type="number"
                min={1}
                value={d.budget_kobo}
                onChange={num("budget_kobo")}
              />
              <p className="mt-1 text-xs text-muted-foreground">
                {nairaHint(d.budget_kobo)}
              </p>
            </div>
            <div>
              <Label htmlFor="ad-daily">Daily budget (kobo)</Label>
              <Input
                id="ad-daily"
                type="number"
                min={1}
                value={d.daily_budget_kobo}
                onChange={num("daily_budget_kobo")}
              />
              <p className="mt-1 text-xs text-muted-foreground">
                {nairaHint(d.daily_budget_kobo)}/day — must be ≤ total
              </p>
            </div>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label htmlFor="ad-age-min">Age min (18–100)</Label>
              <Input
                id="ad-age-min"
                type="number"
                min={18}
                max={100}
                value={d.age_min}
                onChange={num("age_min")}
              />
            </div>
            <div>
              <Label htmlFor="ad-age-max">Age max (18–100)</Label>
              <Input
                id="ad-age-max"
                type="number"
                min={18}
                max={100}
                value={d.age_max}
                onChange={num("age_max")}
              />
            </div>
          </div>
          <div>
            <Label htmlFor="ad-lgas">LGAs (comma-separated)</Label>
            <Input
              id="ad-lgas"
              value={d.lgas}
              placeholder="Ikeja, Eti-Osa"
              onChange={(e) => setD({ ...d, lgas: e.target.value })}
            />
          </div>
          <div>
            <Label htmlFor="ad-interests">Interests (comma-separated)</Label>
            <Input
              id="ad-interests"
              value={d.interests}
              placeholder="politics, community"
              onChange={(e) => setD({ ...d, interests: e.target.value })}
            />
          </div>
          <label className="flex items-start gap-2 text-sm">
            <input
              type="checkbox"
              className="mt-1"
              checked={d.political}
              onChange={(e) => setD({ ...d, political: e.target.checked })}
            />
            <span>
              <span className="font-medium">This is a political ad</span>
              <span className="block text-xs text-muted-foreground">
                Launch requires an account with political-ads authorization
                (external provider process) AND a non-empty disclaimer.
              </span>
            </span>
          </label>
          {d.political ? (
            <div className="rounded-md border border-amber-300 bg-amber-50 p-3 dark:border-amber-800 dark:bg-amber-950/30">
              <p className="text-xs font-medium">
                Political ads requirements
              </p>
              <ul className="mt-1 list-inside list-disc text-xs text-muted-foreground">
                <li>
                  Account authorization:{" "}
                  {account?.political_ads_authorized ? (
                    <span className="text-green-700 dark:text-green-400">
                      authorized ✓
                    </span>
                  ) : (
                    <span className="text-destructive">
                      NOT authorized — launch will be rejected (422). See the
                      docs runbook (Meta authorization takes weeks).
                    </span>
                  )}
                </li>
                <li>
                  Disclaimer: required — set it here or on the creative.
                </li>
              </ul>
              <div className="mt-2">
                <Label htmlFor="ad-disc">
                  Disclaimer {creative?.disclaimer_text?.trim() ? "(overrides the creative's)" : "(required)"}
                </Label>
                <Input
                  id="ad-disc"
                  value={d.disclaimer_text}
                  placeholder='e.g. "Paid for by the Progress Committee"'
                  onChange={(e) => setD({ ...d, disclaimer_text: e.target.value })}
                />
                {!discOk ? (
                  <p className="mt-1 text-xs text-destructive">
                    A disclaimer is required for political ads.
                  </p>
                ) : null}
              </div>
            </div>
          ) : null}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button
            disabled={
              busy ||
              d.name.trim() === "" ||
              !d.account_id ||
              !d.creative_id ||
              d.budget_kobo <= 0 ||
              d.daily_budget_kobo <= 0 ||
              d.daily_budget_kobo > d.budget_kobo ||
              d.age_min < 18 ||
              d.age_max > 100 ||
              d.age_min > d.age_max ||
              !discOk
            }
            onClick={async () => {
              const ok = await onSave(d);
              if (ok) onCancel();
            }}
          >
            {busy ? "Saving…" : isNew ? "Create ad" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
