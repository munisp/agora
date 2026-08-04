"use client";

/**
 * Shift create/edit dialog (SPEC-W20 Agent D): agent picker (active team
 * members), date + start/end time (entered local, sent as UTC ISO), an
 * optional role label and — on edit — the status machine. The backend's
 * overlap guard answers 409 with the conflicting shift id; the parent
 * surfaces the API message verbatim in a toast, so this form stays dumb.
 */
import * as React from "react";
import { Button } from "@/components/ui/button";
import { Input, Label, Select } from "@/components/ui/input";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  NEXT_SHIFT_STATUS,
  SHIFT_LABEL,
  type ShiftStatus,
  type ShiftView,
  type TeamMember,
} from "./types";

export interface ShiftInput {
  agent_id: string;
  starts_at: string; // ISO
  ends_at: string; // ISO
  role?: string;
  status?: ShiftStatus; // edit only
}

function toLocalInputValue(iso: string): { date: string; time: string } {
  const d = new Date(iso);
  const pad = (n: number) => String(n).padStart(2, "0");
  return {
    date: `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`,
    time: `${pad(d.getHours())}:${pad(d.getMinutes())}`,
  };
}

export function ShiftFormDialog({
  open,
  shift,
  members,
  busy,
  onClose,
  onSubmit,
}: {
  open: boolean;
  /** null = create; a shift = edit */
  shift: ShiftView | null;
  members: TeamMember[];
  busy: boolean;
  onClose: () => void;
  onSubmit: (input: ShiftInput, id?: string) => Promise<boolean>;
}) {
  const editing = shift !== null;
  const startParts = shift ? toLocalInputValue(shift.starts_at) : null;
  const endParts = shift ? toLocalInputValue(shift.ends_at) : null;

  const [agentId, setAgentId] = React.useState("");
  const [date, setDate] = React.useState("");
  const [start, setStart] = React.useState("");
  const [end, setEnd] = React.useState("");
  const [role, setRole] = React.useState("");
  const [status, setStatus] = React.useState<ShiftStatus | "">("");

  // Re-seed the form whenever the dialog target changes.
  React.useEffect(() => {
    if (!open) return;
    setAgentId(shift?.agent_id ?? "");
    setDate(startParts?.date ?? "");
    setStart(startParts?.time ?? "");
    setEnd(endParts?.time ?? "");
    setRole(shift?.role ?? "");
    setStatus("");
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, shift?.id]);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!agentId || !date || !start || !end) return;
    const startsAt = new Date(`${date}T${start}`).toISOString();
    const endsAt = new Date(`${date}T${end}`).toISOString();
    const input: ShiftInput = {
      agent_id: agentId,
      starts_at: startsAt,
      ends_at: endsAt,
    };
    if (role.trim()) input.role = role.trim();
    if (editing && status) input.status = status;
    const ok = await onSubmit(input, shift?.id);
    if (ok) onClose();
  };

  const nextStatuses = shift ? NEXT_SHIFT_STATUS[shift.status] : [];

  return (
    <Dialog open={open} onOpenChange={(v) => (!v ? onClose() : undefined)}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{editing ? "Edit shift" : "New shift"}</DialogTitle>
          <DialogDescription>
            Overlapping another shift of the same agent is rejected with the
            conflicting shift id.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} className="space-y-3">
          <div>
            <Label htmlFor="wf-agent">Agent</Label>
            <Select
              id="wf-agent"
              value={agentId}
              onChange={(e) => setAgentId(e.target.value)}
              required
            >
              <option value="" disabled>
                Select an agent…
              </option>
              {members.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.name}
                </option>
              ))}
            </Select>
          </div>
          <div>
            <Label htmlFor="wf-date">Date</Label>
            <Input
              id="wf-date"
              type="date"
              value={date}
              onChange={(e) => setDate(e.target.value)}
              required
            />
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div>
              <Label htmlFor="wf-start">Start</Label>
              <Input
                id="wf-start"
                type="time"
                value={start}
                onChange={(e) => setStart(e.target.value)}
                required
              />
            </div>
            <div>
              <Label htmlFor="wf-end">End</Label>
              <Input
                id="wf-end"
                type="time"
                value={end}
                onChange={(e) => setEnd(e.target.value)}
                required
              />
            </div>
          </div>
          <div>
            <Label htmlFor="wf-role">Role (optional)</Label>
            <Input
              id="wf-role"
              value={role}
              onChange={(e) => setRole(e.target.value)}
              placeholder="front desk, field, …"
              maxLength={120}
            />
          </div>
          {editing && nextStatuses.length > 0 ? (
            <div>
              <Label htmlFor="wf-status">Move status (optional)</Label>
              <Select
                id="wf-status"
                value={status}
                onChange={(e) => setStatus(e.target.value as ShiftStatus | "")}
              >
                <option value="">
                  Keep {SHIFT_LABEL[shift.status]}
                </option>
                {nextStatuses.map((s) => (
                  <option key={s} value={s}>
                    {SHIFT_LABEL[s]}
                  </option>
                ))}
              </Select>
            </div>
          ) : null}
          <DialogFooter>
            <Button type="button" variant="secondary" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button type="submit" disabled={busy}>
              {busy ? "Saving…" : editing ? "Save shift" : "Create shift"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
