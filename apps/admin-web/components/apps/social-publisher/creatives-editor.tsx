"use client";

/**
 * Creatives editor (SPEC-W21 Agent B): kind picker (text|image|video),
 * body, optional media URL and disclaimer field. The disclaimer set here
 * is the creative-level fallback the political-ads launch gate checks
 * when the ad itself carries none.
 *
 * Data: GET/POST /api/bookings/v1/social/creatives,
 *       PATCH /api/bookings/v1/social/creatives/{id}
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
import { Input, Label, Select, Textarea } from "@/components/ui/input";
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
  CREATIVE_KINDS,
  formatTs,
  type SocialCreative,
} from "@/components/apps/social-publisher/types";

export interface CreativeDraft {
  id?: string;
  name: string;
  kind: string;
  body: string;
  media_url: string;
  disclaimer_text: string;
}

export function emptyCreativeDraft(): CreativeDraft {
  return { name: "", kind: "text", body: "", media_url: "", disclaimer_text: "" };
}

export function draftFromCreative(c: SocialCreative): CreativeDraft {
  return {
    id: c.id,
    name: c.name,
    kind: c.kind,
    body: c.body,
    media_url: c.media_url ?? "",
    disclaimer_text: c.disclaimer_text ?? "",
  };
}

export function CreativesTable({
  creatives,
  loading,
  canManage,
  onEdit,
}: {
  creatives: SocialCreative[];
  loading: boolean;
  canManage: boolean;
  onEdit: (c: SocialCreative) => void;
}) {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          <TableHead>Kind</TableHead>
          <TableHead>Body</TableHead>
          <TableHead>Disclaimer</TableHead>
          <TableHead>Updated</TableHead>
          {canManage ? <TableHead className="text-right">Actions</TableHead> : null}
        </TableRow>
      </TableHeader>
      <TableBody>
        {creatives.map((c) => (
          <TableRow key={c.id}>
            <TableCell className="font-medium">{c.name}</TableCell>
            <TableCell>
              <Badge variant="secondary">{c.kind}</Badge>
            </TableCell>
            <TableCell className="max-w-[320px]">
              <span className="line-clamp-2 text-sm text-muted-foreground">
                {c.body}
              </span>
            </TableCell>
            <TableCell>
              {c.disclaimer_text?.trim() ? (
                <Badge variant="info">Set</Badge>
              ) : (
                <Badge variant="outline">None</Badge>
              )}
            </TableCell>
            <TableCell className="text-sm text-muted-foreground">
              {formatTs(c.updated_at ?? c.created_at)}
            </TableCell>
            {canManage ? (
              <TableCell className="text-right">
                <Button size="sm" variant="outline" onClick={() => onEdit(c)}>
                  Edit
                </Button>
              </TableCell>
            ) : null}
          </TableRow>
        ))}
        {!loading && creatives.length === 0 ? (
          <TableEmpty colSpan={canManage ? 6 : 5}>
            No creatives yet — create the copy your posts and ads will use.
          </TableEmpty>
        ) : null}
      </TableBody>
    </Table>
  );
}

export function CreativeEditorDialog({
  draft,
  busy,
  onSave,
  onCancel,
}: {
  draft: CreativeDraft;
  busy: boolean;
  onSave: (d: CreativeDraft) => Promise<boolean>;
  onCancel: () => void;
}) {
  const [d, setD] = React.useState<CreativeDraft>(draft);
  const isNew = !draft.id;
  const needsMedia = d.kind !== "text";
  return (
    <Dialog open onOpenChange={(open) => (!open ? onCancel() : undefined)}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{isNew ? "New creative" : "Edit creative"}</DialogTitle>
          <DialogDescription>
            Reusable copy + media for posts and ads. The disclaimer is the
            fallback the political-ads launch gate checks when the ad itself
            has none.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3 py-2">
          <div>
            <Label htmlFor="cr-name">Name</Label>
            <Input
              id="cr-name"
              value={d.name}
              onChange={(e) => setD({ ...d, name: e.target.value })}
            />
          </div>
          <div>
            <Label htmlFor="cr-kind">Kind</Label>
            <Select
              id="cr-kind"
              value={d.kind}
              onChange={(e) => setD({ ...d, kind: e.target.value })}
            >
              {CREATIVE_KINDS.map((k) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </Select>
          </div>
          <div>
            <Label htmlFor="cr-body">Body</Label>
            <Textarea
              id="cr-body"
              rows={4}
              value={d.body}
              onChange={(e) => setD({ ...d, body: e.target.value })}
            />
          </div>
          {needsMedia ? (
            <div>
              <Label htmlFor="cr-media">Media URL (required for {d.kind})</Label>
              <Input
                id="cr-media"
                value={d.media_url}
                placeholder="https://…"
                onChange={(e) => setD({ ...d, media_url: e.target.value })}
              />
            </div>
          ) : null}
          <div>
            <Label htmlFor="cr-disc">Disclaimer (optional)</Label>
            <Input
              id="cr-disc"
              value={d.disclaimer_text}
              placeholder='e.g. "Paid for by the Progress Committee"'
              onChange={(e) => setD({ ...d, disclaimer_text: e.target.value })}
            />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onCancel} disabled={busy}>
            Cancel
          </Button>
          <Button
            disabled={
              busy ||
              d.name.trim() === "" ||
              d.body.trim() === "" ||
              (needsMedia && d.media_url.trim() === "")
            }
            onClick={async () => {
              const ok = await onSave(d);
              if (ok) onCancel();
            }}
          >
            {busy ? "Saving…" : isNew ? "Create" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
