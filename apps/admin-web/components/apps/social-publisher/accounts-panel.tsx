"use client";

/**
 * Social accounts panel (SPEC-W21 Agent B): provider + authorization
 * badges, status pills, a "connect account" dialog (RECORD ONLY — no real
 * OAuth; the docs runbook covers out-of-band token provisioning) and a
 * status/political-flag editor.
 *
 * Data: GET/POST /api/bookings/v1/social/accounts,
 *       PATCH /api/bookings/v1/social/accounts/{id}
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
  ACCOUNT_STATUS_META,
  ACCOUNT_STATUSES,
  PROVIDERS,
  PROVIDER_META,
  formatTs,
  type SocialAccount,
} from "@/components/apps/social-publisher/types";

export interface AccountDraft {
  id?: string;
  provider: string;
  account_ref: string;
  display_name: string;
  status: string;
  political_ads_authorized: boolean;
}

export function emptyAccountDraft(): AccountDraft {
  return {
    provider: "meta",
    account_ref: "",
    display_name: "",
    status: "connected",
    political_ads_authorized: false,
  };
}

export function draftFromAccount(a: SocialAccount): AccountDraft {
  return {
    id: a.id,
    provider: a.provider,
    account_ref: a.account_ref,
    display_name: a.display_name,
    status: a.status,
    political_ads_authorized: a.political_ads_authorized,
  };
}

export function AccountsTable({
  accounts,
  loading,
  canManage,
  onEdit,
}: {
  accounts: SocialAccount[];
  loading: boolean;
  canManage: boolean;
  onEdit: (a: SocialAccount) => void;
}) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Account</TableHead>
          <TableHead>Provider</TableHead>
          <TableHead>Status</TableHead>
          <TableHead>Political ads</TableHead>
          <TableHead>Added</TableHead>
          {canManage ? <TableHead className="text-right">Actions</TableHead> : null}
        </TableRow>
      </TableHeader>
      <TableBody>
        {accounts.map((a) => {
          const pm = PROVIDER_META[a.provider] ?? { label: a.provider, variant: "outline" as const };
          const sm = ACCOUNT_STATUS_META[a.status] ?? { label: a.status, variant: "outline" as const };
          return (
            <TableRow key={a.id}>
              <TableCell>
                <div className="font-medium">{a.display_name}</div>
                <div className="text-xs text-muted-foreground">{a.account_ref}</div>
              </TableCell>
              <TableCell>
                <Badge variant={pm.variant}>{pm.label}</Badge>
              </TableCell>
              <TableCell>
                <Badge variant={sm.variant}>{sm.label}</Badge>
              </TableCell>
              <TableCell>
                {a.political_ads_authorized ? (
                  <Badge variant="success">Authorized</Badge>
                ) : (
                  <Badge variant="outline">Not authorized</Badge>
                )}
              </TableCell>
              <TableCell className="text-sm text-muted-foreground">
                {formatTs(a.created_at)}
              </TableCell>
              {canManage ? (
                <TableCell className="text-right">
                  <Button size="sm" variant="outline" onClick={() => onEdit(a)}>
                    Edit
                  </Button>
                </TableCell>
              ) : null}
            </TableRow>
          );
        })}
        {!loading && accounts.length === 0 ? (
          <TableEmpty colSpan={canManage ? 6 : 5}>
            No accounts yet — connect one to start publishing. Connect is a
            record only (no OAuth yet); see the docs runbook for token setup.
          </TableEmpty>
        ) : null}
      </TableBody>
    </Table>
  );
}

export function AccountEditorDialog({
  draft,
  busy,
  onSave,
  onCancel,
}: {
  draft: AccountDraft;
  busy: boolean;
  onSave: (d: AccountDraft) => Promise<boolean>;
  onCancel: () => void;
}) {
  const [d, setD] = React.useState<AccountDraft>(draft);
  const isNew = !draft.id;
  return (
    <Dialog open onOpenChange={(open) => (!open ? onCancel() : undefined)}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{isNew ? "Connect account" : "Edit account"}</DialogTitle>
          <DialogDescription>
            {isNew
              ? "Connect records the account — there is no OAuth flow yet. Provision provider tokens out-of-band (see docs/apps/social-publisher.md)."
              : "Update the account record. Mark expired/revoked when the provider token lapses — publishing and ad launches are blocked (409) while it is not connected."}
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3 py-2">
          {isNew ? (
            <div>
              <Label htmlFor="acct-provider">Provider</Label>
              <Select
                id="acct-provider"
                value={d.provider}
                onChange={(e) => setD({ ...d, provider: e.target.value })}
              >
                {PROVIDERS.map((p) => (
                  <option key={p} value={p}>
                    {PROVIDER_META[p]?.label ?? p}
                  </option>
                ))}
              </Select>
            </div>
          ) : null}
          <div>
            <Label htmlFor="acct-ref">Account reference</Label>
            <Input
              id="acct-ref"
              value={d.account_ref}
              placeholder="e.g. page id / advertiser id / handle"
              onChange={(e) => setD({ ...d, account_ref: e.target.value })}
            />
          </div>
          <div>
            <Label htmlFor="acct-name">Display name</Label>
            <Input
              id="acct-name"
              value={d.display_name}
              onChange={(e) => setD({ ...d, display_name: e.target.value })}
            />
          </div>
          {!isNew ? (
            <div>
              <Label htmlFor="acct-status">Status</Label>
              <Select
                id="acct-status"
                value={d.status}
                onChange={(e) => setD({ ...d, status: e.target.value })}
              >
                {ACCOUNT_STATUSES.map((s) => (
                  <option key={s} value={s}>
                    {ACCOUNT_STATUS_META[s]?.label ?? s}
                  </option>
                ))}
              </Select>
            </div>
          ) : null}
          <label className="flex items-start gap-2 text-sm">
            <input
              type="checkbox"
              className="mt-1"
              checked={d.political_ads_authorized}
              onChange={(e) =>
                setD({ ...d, political_ads_authorized: e.target.checked })
              }
            />
            <span>
              <span className="font-medium">Authorized for political ads</span>
              <span className="block text-xs text-muted-foreground">
                Only tick this after the provider-side political-ads
                authorization completed (for Meta this is an external,
                multi-week process — see the docs runbook). The launch gate
                rejects political ads (422) without it.
              </span>
            </span>
          </label>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button
            disabled={
              busy ||
              d.display_name.trim() === "" ||
              d.account_ref.trim() === ""
            }
            onClick={async () => {
              const ok = await onSave(d);
              if (ok) onCancel();
            }}
          >
            {busy ? "Saving…" : isNew ? "Connect" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
