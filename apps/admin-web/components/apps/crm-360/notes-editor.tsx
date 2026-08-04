"use client";

/**
 * Notes editor (SPEC-W20 Agent A): pinned-first note list with inline
 * edit + pin toggle, plus a create form. Pure presentational — the
 * parent owns the network calls (POST/PATCH /v1/crm/...).
 */
import * as React from "react";
import { Pin, PinOff, Pencil, Save, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/input";
import { formatDateTime } from "@/lib/utils";
import type { CrmNote } from "./types";

export function NotesEditor({
  notes,
  canWork,
  busy,
  onCreate,
  onUpdate,
}: {
  notes: CrmNote[];
  /** owner/admin/staff — may create/edit/pin notes */
  canWork: boolean;
  busy: boolean;
  onCreate: (body: string, pinned: boolean) => void;
  onUpdate: (id: string, patch: { body?: string; pinned?: boolean }) => void;
}) {
  const [draft, setDraft] = React.useState("");
  const [draftPinned, setDraftPinned] = React.useState(false);
  const [editingId, setEditingId] = React.useState<string | null>(null);
  const [editBody, setEditBody] = React.useState("");

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const body = draft.trim();
    if (!body) return;
    onCreate(body, draftPinned);
    setDraft("");
    setDraftPinned(false);
  };

  const startEdit = (n: CrmNote) => {
    setEditingId(n.id);
    setEditBody(n.body);
  };

  const saveEdit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!editingId) return;
    const body = editBody.trim();
    if (!body) return;
    onUpdate(editingId, { body });
    setEditingId(null);
  };

  return (
    <div className="space-y-3">
      {canWork ? (
        <form onSubmit={submit} className="space-y-2 rounded-md border border-border bg-muted/30 p-3">
          <Textarea
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            placeholder="Add a note — context the whole team should see…"
            maxLength={8000}
            disabled={busy}
          />
          <div className="flex items-center justify-between">
            <label className="flex items-center gap-1.5 text-sm text-muted-foreground">
              <input
                type="checkbox"
                checked={draftPinned}
                onChange={(e) => setDraftPinned(e.target.checked)}
                disabled={busy}
              />
              Pin to top
            </label>
            <Button type="submit" size="sm" disabled={busy || !draft.trim()}>
              Add note
            </Button>
          </div>
        </form>
      ) : null}

      {notes.length === 0 ? (
        <p className="rounded-md border border-border bg-card px-4 py-6 text-sm text-muted-foreground">
          No notes yet{canWork ? " — add the first one above" : ""}.
        </p>
      ) : (
        <ul className="space-y-2">
          {notes.map((n) => (
            <li
              key={n.id}
              className="rounded-md border border-border bg-card px-3 py-2.5"
            >
              <div className="mb-1 flex items-center justify-between gap-2">
                <div className="flex items-center gap-2 text-xs text-muted-foreground">
                  {n.pinned ? (
                    <Badge variant="warning" className="gap-1">
                      <Pin className="h-3 w-3" /> Pinned
                    </Badge>
                  ) : null}
                  <span>{n.author || "staff"}</span>
                  <span aria-hidden>·</span>
                  <span>{formatDateTime(n.created_at)}</span>
                  {n.updated_at !== n.created_at ? <span>(edited)</span> : null}
                </div>
                {canWork ? (
                  <div className="flex items-center gap-1">
                    <button
                      type="button"
                      aria-label={n.pinned ? "Unpin note" : "Pin note"}
                      title={n.pinned ? "Unpin" : "Pin"}
                      disabled={busy}
                      onClick={() => onUpdate(n.id, { pinned: !n.pinned })}
                      className="rounded p-1 text-muted-foreground hover:text-foreground disabled:opacity-50"
                    >
                      {n.pinned ? <PinOff className="h-4 w-4" /> : <Pin className="h-4 w-4" />}
                    </button>
                    <button
                      type="button"
                      aria-label="Edit note"
                      title="Edit"
                      disabled={busy}
                      onClick={() => startEdit(n)}
                      className="rounded p-1 text-muted-foreground hover:text-foreground disabled:opacity-50"
                    >
                      <Pencil className="h-4 w-4" />
                    </button>
                  </div>
                ) : null}
              </div>
              {editingId === n.id ? (
                <form onSubmit={saveEdit} className="space-y-2">
                  <Textarea
                    value={editBody}
                    onChange={(e) => setEditBody(e.target.value)}
                    maxLength={8000}
                    disabled={busy}
                    autoFocus
                  />
                  <div className="flex justify-end gap-2">
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      onClick={() => setEditingId(null)}
                      disabled={busy}
                    >
                      <X className="mr-1 h-3.5 w-3.5" /> Cancel
                    </Button>
                    <Button type="submit" size="sm" disabled={busy || !editBody.trim()}>
                      <Save className="mr-1 h-3.5 w-3.5" /> Save
                    </Button>
                  </div>
                </form>
              ) : (
                <p className="whitespace-pre-wrap text-sm text-foreground">{n.body}</p>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
