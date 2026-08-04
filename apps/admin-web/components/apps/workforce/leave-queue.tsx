"use client";

/**
 * Leave queue (SPEC-W20 Agent D): file a leave request (agent, kind, date
 * range, reason) and decide the pending queue (approve/decline — the
 * backend records decided_by from the JWT sub). Decided requests render in
 * a separate history list with the decider and timestamp.
 */
import * as React from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input, Label, Select, Textarea } from "@/components/ui/input";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { formatDateTime } from "@/lib/utils";
import {
  LEAVE_KINDS,
  leaveVariant,
  shortId,
  type LeaveKind,
  type LeaveRequest,
  type TeamMember,
} from "./types";

export interface LeaveInput {
  agent_id: string;
  kind: LeaveKind;
  starts_on: string; // YYYY-MM-DD
  ends_on: string; // YYYY-MM-DD
  reason?: string;
}

export function LeaveQueue({
  members,
  requests,
  loading,
  busy,
  canWrite,
  onFile,
  onDecide,
}: {
  members: TeamMember[];
  requests: LeaveRequest[] | null;
  loading: boolean;
  busy: boolean;
  canWrite: boolean;
  onFile: (input: LeaveInput) => Promise<boolean>;
  onDecide: (id: string, action: "approve" | "decline") => Promise<boolean>;
}) {
  const [agentId, setAgentId] = React.useState("");
  const [kind, setKind] = React.useState<LeaveKind>("annual");
  const [startsOn, setStartsOn] = React.useState("");
  const [endsOn, setEndsOn] = React.useState("");
  const [reason, setReason] = React.useState("");

  const nameOf = React.useMemo(() => {
    const map = new Map(members.map((m) => [m.id, m.name]));
    return (id: string) => map.get(id) ?? shortId(id);
  }, [members]);

  const file = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!agentId || !startsOn || !endsOn) return;
    const ok = await onFile({
      agent_id: agentId,
      kind,
      starts_on: startsOn,
      ends_on: endsOn,
      reason: reason.trim() || undefined,
    });
    if (ok) {
      setReason("");
      setStartsOn("");
      setEndsOn("");
    }
  };

  const pending = (requests ?? []).filter((l) => l.status === "pending");
  const decided = (requests ?? []).filter((l) => l.status !== "pending");

  return (
    <div className="grid gap-4 lg:grid-cols-3">
      {canWrite ? (
        <Card>
          <CardHeader>
            <CardTitle>File leave</CardTitle>
            <CardDescription>New requests enter the queue as pending.</CardDescription>
          </CardHeader>
          <CardContent>
            <form onSubmit={file} className="space-y-3">
              <div>
                <Label htmlFor="wf-leave-agent">Agent</Label>
                <Select
                  id="wf-leave-agent"
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
                <Label htmlFor="wf-leave-kind">Kind</Label>
                <Select
                  id="wf-leave-kind"
                  value={kind}
                  onChange={(e) => setKind(e.target.value as LeaveKind)}
                >
                  {LEAVE_KINDS.map((k) => (
                    <option key={k} value={k}>
                      {k}
                    </option>
                  ))}
                </Select>
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <Label htmlFor="wf-leave-from">From</Label>
                  <Input
                    id="wf-leave-from"
                    type="date"
                    value={startsOn}
                    onChange={(e) => setStartsOn(e.target.value)}
                    required
                  />
                </div>
                <div>
                  <Label htmlFor="wf-leave-to">To</Label>
                  <Input
                    id="wf-leave-to"
                    type="date"
                    value={endsOn}
                    onChange={(e) => setEndsOn(e.target.value)}
                    required
                  />
                </div>
              </div>
              <div>
                <Label htmlFor="wf-leave-reason">Reason (optional)</Label>
                <Textarea
                  id="wf-leave-reason"
                  value={reason}
                  onChange={(e) => setReason(e.target.value)}
                  maxLength={2000}
                  rows={2}
                />
              </div>
              <Button type="submit" size="sm" disabled={busy}>
                {busy ? "Filing…" : "File request"}
              </Button>
            </form>
          </CardContent>
        </Card>
      ) : null}

      <Card className="lg:col-span-2">
        <CardHeader>
          <CardTitle>Leave queue</CardTitle>
          <CardDescription>
            {pending.length} pending · {decided.length} decided
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {loading && !requests ? (
            <div className="rounded-md border border-border p-6 text-center text-sm text-muted-foreground">
              Loading leave requests…
            </div>
          ) : null}
          {requests && pending.length === 0 ? (
            <div className="rounded border border-dashed border-border p-4 text-center text-sm text-muted-foreground">
              No pending requests — the queue is clear.
            </div>
          ) : null}
          {pending.map((l) => (
            <div
              key={l.id}
              className="flex flex-wrap items-center justify-between gap-3 rounded-md border border-border bg-card p-3"
            >
              <div>
                <div className="flex items-center gap-2">
                  <span className="text-sm font-medium">{nameOf(l.agent_id)}</span>
                  <Badge variant={leaveVariant(l.status)}>{l.status}</Badge>
                  <Badge variant="secondary">{l.kind}</Badge>
                </div>
                <div className="mt-1 text-xs text-muted-foreground">
                  {l.starts_on.slice(0, 10)} → {l.ends_on.slice(0, 10)}
                  {l.reason ? ` · ${l.reason}` : ""}
                </div>
              </div>
              {canWrite ? (
                <div className="flex gap-2">
                  <Button
                    size="sm"
                    onClick={() => void onDecide(l.id, "approve")}
                    disabled={busy}
                  >
                    Approve
                  </Button>
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={() => void onDecide(l.id, "decline")}
                    disabled={busy}
                  >
                    Decline
                  </Button>
                </div>
              ) : null}
            </div>
          ))}

          {decided.length > 0 ? (
            <div>
              <div className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                Decided
              </div>
              <div className="space-y-2">
                {decided.map((l) => (
                  <div
                    key={l.id}
                    className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border bg-muted/30 p-3"
                  >
                    <div className="flex items-center gap-2">
                      <span className="text-sm">{nameOf(l.agent_id)}</span>
                      <Badge variant={leaveVariant(l.status)}>{l.status}</Badge>
                      <span className="text-xs text-muted-foreground">
                        {l.starts_on.slice(0, 10)} → {l.ends_on.slice(0, 10)}
                      </span>
                    </div>
                    <span className="text-xs text-muted-foreground">
                      by {l.decided_by || "unknown"}
                      {l.decided_at ? ` · ${formatDateTime(l.decided_at)}` : ""}
                    </span>
                  </div>
                ))}
              </div>
            </div>
          ) : null}
        </CardContent>
      </Card>
    </div>
  );
}
