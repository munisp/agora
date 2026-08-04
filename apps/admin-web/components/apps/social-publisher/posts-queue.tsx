"use client";

/**
 * Posts queue (SPEC-W21 Agent B): the draft|queued|publishing|published|
 * failed queue with per-row publish buttons (publish on an expired|revoked
 * account is blocked server-side with 409 — surfaced via toast) and
 * status pills. New posts are created against a connected account + an
 * existing creative.
 *
 * Data: GET/POST /api/bookings/v1/social/posts,
 *       GET /api/bookings/v1/social/posts/{id},
 *       POST /api/bookings/v1/social/posts/{id}/publish
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
import { Label, Select } from "@/components/ui/input";
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
  POST_STATUS_META,
  formatTs,
  shortId,
  type SocialAccount,
  type SocialCreative,
  type SocialPost,
} from "@/components/apps/social-publisher/types";

export function PostsTable({
  posts,
  accounts,
  creatives,
  loading,
  busyId,
  canManage,
  onPublish,
}: {
  posts: SocialPost[];
  accounts: SocialAccount[];
  creatives: SocialCreative[];
  loading: boolean;
  busyId: string | null;
  canManage: boolean;
  onPublish: (p: SocialPost) => void;
}) {
  const accountOf = (id: string) => accounts.find((a) => a.id === id);
  const creativeOf = (id: string) => creatives.find((c) => c.id === id);
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Post</TableHead>
          <TableHead>Account</TableHead>
          <TableHead>Status</TableHead>
          <TableHead>Provider post</TableHead>
          <TableHead>Created</TableHead>
          {canManage ? <TableHead className="text-right">Actions</TableHead> : null}
        </TableRow>
      </TableHeader>
      <TableBody>
        {posts.map((p) => {
          const sm = POST_STATUS_META[p.status] ?? { label: p.status, variant: "outline" as const };
          const acct = accountOf(p.account_id);
          const creative = creativeOf(p.creative_id);
          const publishable =
            p.status === "draft" || p.status === "queued" || p.status === "failed";
          const blocked = acct !== undefined && acct.status !== "connected";
          return (
            <TableRow key={p.id}>
              <TableCell>
                <div className="font-medium">{creative?.name ?? shortId(p.creative_id)}</div>
                {p.error ? (
                  <div className="text-xs text-destructive">{p.error}</div>
                ) : (
                  <div className="text-xs text-muted-foreground">{shortId(p.id)}</div>
                )}
              </TableCell>
              <TableCell>
                {acct ? (
                  <span className="text-sm">
                    {acct.display_name}
                    {acct.status !== "connected" ? (
                      <Badge variant="warning" className="ml-2">
                        {acct.status}
                      </Badge>
                    ) : null}
                  </span>
                ) : (
                  <span className="text-sm text-muted-foreground">{shortId(p.account_id)}</span>
                )}
              </TableCell>
              <TableCell>
                <Badge variant={sm.variant}>{sm.label}</Badge>
              </TableCell>
              <TableCell className="text-sm text-muted-foreground">
                {p.provider_post_id ?? "—"}
              </TableCell>
              <TableCell className="text-sm text-muted-foreground">
                {formatTs(p.created_at)}
              </TableCell>
              {canManage ? (
                <TableCell className="text-right">
                  {publishable ? (
                    <Button
                      size="sm"
                      disabled={busyId === p.id || blocked}
                      title={
                        blocked
                          ? `Account is ${acct?.status} — reconnect before publishing`
                          : undefined
                      }
                      onClick={() => onPublish(p)}
                    >
                      {busyId === p.id
                        ? "Publishing…"
                        : p.status === "failed"
                          ? "Retry publish"
                          : "Publish"}
                    </Button>
                  ) : null}
                </TableCell>
              ) : null}
            </TableRow>
          );
        })}
        {!loading && posts.length === 0 ? (
          <TableEmpty colSpan={canManage ? 6 : 5}>
            No posts yet — queue one from a creative and publish it.
          </TableEmpty>
        ) : null}
      </TableBody>
    </Table>
  );
}

export function PostCreateDialog({
  accounts,
  creatives,
  busy,
  onCreate,
  onCancel,
}: {
  accounts: SocialAccount[];
  creatives: SocialCreative[];
  busy: boolean;
  onCreate: (accountId: string, creativeId: string, status: string) => Promise<boolean>;
  onCancel: () => void;
}) {
  const [accountId, setAccountId] = React.useState(accounts[0]?.id ?? "");
  const [creativeId, setCreativeId] = React.useState(creatives[0]?.id ?? "");
  const [status, setStatus] = React.useState("queued");
  return (
    <Dialog open onOpenChange={(open) => (!open ? onCancel() : undefined)}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Queue a post</DialogTitle>
          <DialogDescription>
            Pick the account + creative. Publishing happens explicitly from
            the queue (through the provider seam — mock by default).
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3 py-2">
          <div>
            <Label htmlFor="post-account">Account</Label>
            <Select
              id="post-account"
              value={accountId}
              onChange={(e) => setAccountId(e.target.value)}
            >
              {accounts.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.display_name} ({a.provider}
                  {a.status !== "connected" ? ` — ${a.status}` : ""})
                </option>
              ))}
            </Select>
          </div>
          <div>
            <Label htmlFor="post-creative">Creative</Label>
            <Select
              id="post-creative"
              value={creativeId}
              onChange={(e) => setCreativeId(e.target.value)}
            >
              {creatives.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name} ({c.kind})
                </option>
              ))}
            </Select>
          </div>
          <div>
            <Label htmlFor="post-status">Initial status</Label>
            <Select
              id="post-status"
              value={status}
              onChange={(e) => setStatus(e.target.value)}
            >
              <option value="queued">Queued</option>
              <option value="draft">Draft</option>
            </Select>
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button
            disabled={busy || !accountId || !creativeId}
            onClick={async () => {
              const ok = await onCreate(accountId, creativeId, status);
              if (ok) onCancel();
            }}
          >
            {busy ? "Creating…" : "Create post"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
