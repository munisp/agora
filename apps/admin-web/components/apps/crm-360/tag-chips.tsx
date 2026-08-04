"use client";

/**
 * Tag chips (SPEC-W20 Agent A): current tags as removable chips + an add
 * input. Pure presentational — the parent owns the network calls. Tags
 * are normalized lowercase [a-z0-9-_]{1,40} server-side; the input gives
 * the same hint client-side.
 */
import * as React from "react";
import { Plus, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

export function TagChips({
  tags,
  canWork,
  busy,
  onAdd,
  onRemove,
}: {
  tags: string[];
  /** owner/admin/staff — may add/remove tags */
  canWork: boolean;
  busy: boolean;
  onAdd: (tag: string) => void;
  onRemove: (tag: string) => void;
}) {
  const [draft, setDraft] = React.useState("");

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const tag = draft.trim().toLowerCase();
    if (!tag) return;
    onAdd(tag);
    setDraft("");
  };

  return (
    <div>
      <div className="flex flex-wrap items-center gap-1.5">
        {tags.length === 0 ? (
          <span className="text-sm text-muted-foreground">
            No tags yet — labels like “vip” or “gold-tier” make contacts
            searchable by segment.
          </span>
        ) : (
          tags.map((tag) => (
            <Badge key={tag} variant="secondary" className="gap-1">
              {tag}
              {canWork ? (
                <button
                  type="button"
                  aria-label={`Remove tag ${tag}`}
                  disabled={busy}
                  onClick={() => onRemove(tag)}
                  className="ml-0.5 rounded-full hover:text-destructive disabled:opacity-50"
                >
                  <X className="h-3 w-3" />
                </button>
              ) : null}
            </Badge>
          ))
        )}
      </div>
      {canWork ? (
        <form onSubmit={submit} className="mt-2 flex max-w-xs items-center gap-2">
          <Input
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            placeholder="add-tag"
            maxLength={40}
            pattern="[a-z0-9\-_]{1,40}"
            title="Lowercase, 1-40 chars: a-z 0-9 - _"
            disabled={busy}
          />
          <Button type="submit" variant="outline" size="sm" disabled={busy || !draft.trim()}>
            <Plus className="mr-1 h-3.5 w-3.5" />
            Add
          </Button>
        </form>
      ) : null}
    </div>
  );
}
