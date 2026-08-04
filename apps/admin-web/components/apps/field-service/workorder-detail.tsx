"use client";

/**
 * Work-order detail panel (SPEC-W19 Agent B): status flow buttons that
 * enforce the state machine (only legal next transitions render; Complete
 * stays disabled until the checklist is all-done and proof notes exist,
 * mirroring the server gate), a checklist editor, GPS display and the
 * proof-notes editor, plus the dispatch control (assignee id or "auto",
 * optional push notification).
 */
import * as React from "react";
import { MapPin, Plus, Send, Trash2, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input, Label, Textarea } from "@/components/ui/input";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { formatDateTime } from "@/lib/utils";
import {
  NEXT_STATUS,
  STATUS_LABEL,
  completionBlockers,
  statusVariant,
  type BoardItem,
  type ChecklistItem,
  type WorkOrderStatus,
} from "./types";

export function WorkOrderDetail({
  item,
  canWrite,
  busy,
  onClose,
  onPatch,
  onDispatch,
}: {
  item: BoardItem;
  /** manage_bookings (owner/admin/staff) — hides every write control */
  canWrite: boolean;
  /** a mutation is in flight (disables buttons) */
  busy: boolean;
  onClose: () => void;
  onPatch: (id: string, body: Record<string, unknown>) => Promise<boolean>;
  onDispatch: (id: string, assignee: string, notify: boolean) => Promise<boolean>;
}) {
  const [checklist, setChecklist] = React.useState<ChecklistItem[]>(item.checklist);
  const [notes, setNotes] = React.useState(item.proof?.notes ?? "");
  const [newItem, setNewItem] = React.useState("");
  const [assignee, setAssignee] = React.useState(item.assignee_id ?? "auto");
  const [notify, setNotify] = React.useState(true);

  // Re-sync local editors when the selected order changes / reloads.
  React.useEffect(() => {
    setChecklist(item.checklist);
    setNotes(item.proof?.notes ?? "");
    setAssignee(item.assignee_id ?? "auto");
  }, [item]);

  const terminal = item.status === "completed" || item.status === "cancelled";
  const writable = canWrite && !terminal;

  // Client-side gate hint: the server re-checks authoritatively; a draft
  // completion needs the CURRENT editor contents, not the last-saved row.
  const gateHint = completionBlockers({
    ...item,
    checklist,
    proof: { ...item.proof, notes },
  });

  const saveChecklist = (next: ChecklistItem[]) => {
    setChecklist(next);
    void onPatch(item.id, { checklist: next });
  };

  const toggleItem = (idx: number) => {
    saveChecklist(
      checklist.map((c, i) => (i === idx ? { ...c, done: !c.done } : c)),
    );
  };

  const addItem = () => {
    const label = newItem.trim();
    if (!label) return;
    saveChecklist([...checklist, { label, done: false }]);
    setNewItem("");
  };

  const removeItem = (idx: number) => {
    saveChecklist(checklist.filter((_, i) => i !== idx));
  };

  const saveNotes = () => {
    void onPatch(item.id, { proof: { ...item.proof, notes } });
  };

  const transitions = NEXT_STATUS[item.status].filter((s) => s !== "assigned");

  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between gap-3 space-y-0">
        <div>
          <CardTitle className="flex flex-wrap items-center gap-2 text-base">
            {item.title}
            <Badge variant={statusVariant(item.status)}>
              {STATUS_LABEL[item.status]}
            </Badge>
          </CardTitle>
          <CardDescription>
            {item.assignee_name ? `Assigned to ${item.assignee_name}` : "Unassigned"}
            {item.scheduled_start
              ? ` · ${formatDateTime(item.scheduled_start)}${item.scheduled_end ? ` → ${formatDateTime(item.scheduled_end)}` : ""}`
              : " · unscheduled"}
          </CardDescription>
        </div>
        <Button variant="ghost" size="sm" onClick={onClose} aria-label="Close detail">
          <X className="h-4 w-4" />
        </Button>
      </CardHeader>
      <CardContent className="space-y-5">
        {item.description ? (
          <p className="whitespace-pre-wrap text-sm text-muted-foreground">{item.description}</p>
        ) : null}

        {/* GPS fix */}
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <MapPin className="h-4 w-4 shrink-0" />
          {item.gps_lat != null && item.gps_lng != null ? (
            <span>
              {item.gps_lat.toFixed(5)}, {item.gps_lng.toFixed(5)}
              {item.gps_accuracy != null ? ` (±${Math.round(item.gps_accuracy)} m)` : ""}
            </span>
          ) : (
            <span>No GPS fix recorded</span>
          )}
        </div>

        {/* Status flow */}
        {writable && transitions.length > 0 ? (
          <div className="space-y-2">
            <Label>Advance status</Label>
            <div className="flex flex-wrap items-center gap-2">
              {transitions.map((next) => {
                const blocked = next === "completed" && gateHint.length > 0;
                return (
                  <Button
                    key={next}
                    size="sm"
                    variant={next === "cancelled" ? "destructive" : "default"}
                    disabled={busy || blocked}
                    title={blocked ? gateHint.join(" · ") : undefined}
                    onClick={() => void onPatch(item.id, { status: next as WorkOrderStatus })}
                  >
                    {next === "cancelled" ? "Cancel order" : `Mark ${STATUS_LABEL[next]}`}
                  </Button>
                );
              })}
            </div>
            {NEXT_STATUS[item.status].includes("completed") && gateHint.length > 0 ? (
              <p className="text-xs text-muted-foreground">
                Complete unlocks when: {gateHint.join(" · ")}.
              </p>
            ) : null}
          </div>
        ) : null}

        {/* Dispatch control (created / assigned lanes) */}
        {writable && (item.status === "created" || item.status === "assigned") ? (
          <div className="space-y-2 rounded-md border border-border p-3">
            <Label>{item.status === "assigned" ? "Re-dispatch" : "Dispatch"}</Label>
            <div className="flex flex-wrap items-center gap-2">
              <Input
                className="w-72"
                value={assignee}
                onChange={(e) => setAssignee(e.target.value)}
                placeholder='team member id or "auto"'
              />
              <label className="flex items-center gap-1.5 text-sm text-muted-foreground">
                <input
                  type="checkbox"
                  checked={notify}
                  onChange={(e) => setNotify(e.target.checked)}
                />
                push notify
              </label>
              <Button
                size="sm"
                variant="secondary"
                disabled={busy || assignee.trim() === ""}
                onClick={() => void onDispatch(item.id, assignee.trim(), notify)}
              >
                <Send className="mr-1 h-3.5 w-3.5" />
                Dispatch
              </Button>
            </div>
            <p className="text-xs text-muted-foreground">
              &quot;auto&quot; picks the active team member with the fewest open
              orders. Push notify is best-effort (no-op when notifications are
              disabled).
            </p>
          </div>
        ) : null}

        {/* Checklist editor */}
        <div className="space-y-2">
          <Label>Checklist</Label>
          {checklist.length === 0 ? (
            <p className="text-sm text-muted-foreground">No checklist items.</p>
          ) : (
            <ul className="space-y-1">
              {checklist.map((c, idx) => (
                <li key={idx} className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={c.done}
                    disabled={!writable || busy}
                    onChange={() => toggleItem(idx)}
                  />
                  <span className={c.done ? "text-muted-foreground line-through" : ""}>
                    {c.label}
                  </span>
                  {writable ? (
                    <button
                      type="button"
                      className="ml-auto text-muted-foreground hover:text-destructive"
                      onClick={() => removeItem(idx)}
                      aria-label={`Remove ${c.label}`}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </button>
                  ) : null}
                </li>
              ))}
            </ul>
          )}
          {writable ? (
            <div className="flex items-center gap-2">
              <Input
                value={newItem}
                onChange={(e) => setNewItem(e.target.value)}
                placeholder="Add checklist item"
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    addItem();
                  }
                }}
              />
              <Button size="sm" variant="secondary" disabled={busy || !newItem.trim()} onClick={addItem}>
                <Plus className="mr-1 h-3.5 w-3.5" />
                Add
              </Button>
            </div>
          ) : null}
        </div>

        {/* Proof notes */}
        <div className="space-y-2">
          <Label>Proof notes</Label>
          <Textarea
            rows={3}
            value={notes}
            disabled={!writable || busy}
            placeholder="Completion note (required to mark the order completed)"
            onChange={(e) => setNotes(e.target.value)}
          />
          {writable ? (
            <Button
              size="sm"
              variant="secondary"
              disabled={busy || notes === (item.proof?.notes ?? "")}
              onClick={saveNotes}
            >
              Save notes
            </Button>
          ) : null}
          {item.proof?.photos && item.proof.photos.length > 0 ? (
            <p className="text-xs text-muted-foreground">
              {item.proof.photos.length} photo{item.proof.photos.length > 1 ? "s" : ""} attached
            </p>
          ) : null}
        </div>
      </CardContent>
    </Card>
  );
}
