"use client";

/**
 * Journey list (SPEC-W19 Agent D): status pills, lifecycle actions
 * (activate / pause / resume / archive through the status machine),
 * operator-triggered step run, manual enrollment by contact ids, and the
 * expandable stats panel.
 */

import * as React from "react";
import { ChevronDown, ChevronRight, Play, UserPlus } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { useToast } from "@/components/ui/toast";
import { StatsPanel } from "./stats-panel";
import {
  STUDIO_API,
  statusTone,
  type Journey,
  type StepRunResult,
} from "./types";

export function JourneyList({
  orgSlug,
  canManage,
  journeys,
  onEdit,
  onChanged,
}: {
  orgSlug: string;
  canManage: boolean;
  journeys: Journey[];
  onEdit: (j: Journey) => void;
  onChanged: () => void;
}) {
  const { toast } = useToast();
  const [expandedId, setExpandedId] = React.useState<string | null>(null);
  const [busyId, setBusyId] = React.useState<string | null>(null);
  const [refreshKey, setRefreshKey] = React.useState(0);

  const act = async (j: Journey, fn: () => Promise<unknown>, done: string) => {
    setBusyId(j.id);
    try {
      await fn();
      if (done) toast({ title: done });
      onChanged();
      setRefreshKey((k) => k + 1);
    } catch (e) {
      toast({
        title: "Action failed",
        description: e instanceof ApiError ? e.message : String(e),
      });
    } finally {
      setBusyId(null);
    }
  };

  const transition = (j: Journey, status: string) =>
    act(
      j,
      () => api.patch(`${STUDIO_API}/journeys/${j.id}`, { status }, { tenant: orgSlug }),
      `Journey ${status}`,
    );

  const runStep = (j: Journey) =>
    act(
      j,
      async () => {
        const res = await api.post<StepRunResult>(
          `${STUDIO_API}/journeys/${j.id}/step`,
          {},
          { tenant: orgSlug },
        );
        toast({
          title: "Step run complete",
          description: `advanced ${res.advanced} · completed ${res.completed} · exited ${res.exited} · sends queued ${res.sends_queued}${res.sends_deferred ? " · sends deferred (dispatcher unavailable)" : ""}`,
        });
      },
      "",
    );

  const enroll = (j: Journey) => {
    const raw = window.prompt(
      "Contact ids to enroll (comma-separated UUIDs) — idempotent per journey+contact:",
    );
    if (!raw) return;
    const ids = raw.split(",").map((s) => s.trim()).filter(Boolean);
    if (ids.length === 0) return;
    void act(
      j,
      async () => {
        const res = await api.post<{ enrolled: number; existing: number }>(
          `${STUDIO_API}/journeys/${j.id}/enroll`,
          { contact_ids: ids },
          { tenant: orgSlug },
        );
        toast({
          title: "Enrollment complete",
          description: `${res.enrolled} new · ${res.existing} already enrolled`,
        });
      },
      "",
    );
  };

  if (journeys.length === 0) {
    return (
      <p className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
        No journeys yet — create one to start orchestrating lifecycle messaging.
      </p>
    );
  }

  return (
    <ul className="space-y-2">
      {journeys.map((j) => {
        const expanded = expandedId === j.id;
        return (
          <li key={j.id} className="rounded-md border">
            <div className="flex flex-wrap items-center justify-between gap-2 p-3">
              <button
                type="button"
                className="flex min-w-0 items-center gap-2 text-left"
                onClick={() => setExpandedId(expanded ? null : j.id)}
              >
                {expanded ? (
                  <ChevronDown className="h-4 w-4 shrink-0" />
                ) : (
                  <ChevronRight className="h-4 w-4 shrink-0" />
                )}
                <span className="truncate text-sm font-medium">{j.name}</span>
                <span className={`rounded-full px-2 py-0.5 text-xs ${statusTone(j.status)}`}>
                  {j.status}
                </span>
                <span className="text-xs text-muted-foreground">
                  {j.steps.length} step{j.steps.length === 1 ? "" : "s"} · {j.trigger_kind}
                </span>
              </button>
              {canManage ? (
                <div className="flex shrink-0 flex-wrap items-center gap-2">
                  {j.status === "draft" ? (
                    <>
                      <Button size="sm" variant="outline" disabled={busyId === j.id} onClick={() => onEdit(j)}>
                        Edit
                      </Button>
                      <Button size="sm" disabled={busyId === j.id} onClick={() => void transition(j, "active")}>
                        Activate
                      </Button>
                    </>
                  ) : null}
                  {j.status === "active" ? (
                    <>
                      <Button size="sm" variant="outline" disabled={busyId === j.id} onClick={() => enroll(j)}>
                        <UserPlus className="mr-1 h-3.5 w-3.5" /> Enroll
                      </Button>
                      <Button size="sm" disabled={busyId === j.id} onClick={() => void runStep(j)}>
                        <Play className="mr-1 h-3.5 w-3.5" /> Run step
                      </Button>
                      <Button size="sm" variant="outline" disabled={busyId === j.id} onClick={() => void transition(j, "paused")}>
                        Pause
                      </Button>
                    </>
                  ) : null}
                  {j.status === "paused" ? (
                    <>
                      <Button size="sm" variant="outline" disabled={busyId === j.id} onClick={() => onEdit(j)}>
                        Edit
                      </Button>
                      <Button size="sm" disabled={busyId === j.id} onClick={() => void transition(j, "active")}>
                        Resume
                      </Button>
                    </>
                  ) : null}
                  {j.status !== "archived" ? (
                    <Button size="sm" variant="ghost" disabled={busyId === j.id} onClick={() => void transition(j, "archived")}>
                      Archive
                    </Button>
                  ) : null}
                </div>
              ) : null}
            </div>
            {expanded ? (
              <div className="border-t p-3">
                <StatsPanel orgSlug={orgSlug} journey={j} refreshKey={refreshKey} />
              </div>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}
