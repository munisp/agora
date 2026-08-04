"use client";

/**
 * Workforce app client (SPEC-W20 Agent D): week grid with coverage, shift
 * create/edit dialog, clock in/out panel, leave queue and the utilization
 * table.
 *
 * Data sources (all through the BFF with the x-tenant-slug header):
 *   - GET   /api/bookings/v1/workforce/team-members
 *   - GET   /api/bookings/v1/workforce/shifts/week?from=
 *   - POST  /api/bookings/v1/workforce/shifts
 *   - PATCH /api/bookings/v1/workforce/shifts/{id}
 *   - GET   /api/bookings/v1/workforce/coverage?from=&to=
 *   - POST  /api/bookings/v1/workforce/time/clock-in|clock-out
 *   - GET   /api/bookings/v1/workforce/time/entries?agent_id=
 *   - GET/POST /api/bookings/v1/workforce/leave
 *   - PATCH /api/bookings/v1/workforce/leave/{id}
 *   - GET   /api/bookings/v1/workforce/utilization?from=&to=
 *
 * List payloads ride the tolerant unwrap<T>() envelope convention
 * (components/apps/types.ts).
 */
import * as React from "react";
import { ChevronLeft, ChevronRight, Plus, RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { useToast } from "@/components/ui/toast";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { unwrap } from "@/components/apps/types";
import { WeekGridView } from "@/components/apps/workforce/week-grid";
import { ShiftFormDialog, type ShiftInput } from "@/components/apps/workforce/shift-form";
import { ClockPanel, type ClockInInput } from "@/components/apps/workforce/clock-panel";
import { LeaveQueue, type LeaveInput } from "@/components/apps/workforce/leave-queue";
import { UtilizationTable } from "@/components/apps/workforce/utilization-table";
import type {
  CoverageDay,
  LeaveRequest,
  ShiftView,
  TeamMember,
  TimeEntry,
  UtilizationRow,
  WeekGrid,
} from "@/components/apps/workforce/types";

const BASE = "/api/bookings/v1/workforce";

function isoDay(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

function addDays(day: string, n: number): string {
  const d = new Date(`${day}T00:00:00`);
  d.setDate(d.getDate() + n);
  return isoDay(d);
}

function apiMessage(e: unknown, fallback: string): string {
  return e instanceof ApiError ? e.message : fallback;
}

export function WorkforceClient({
  orgSlug,
  canWrite,
}: {
  orgSlug: string;
  /** manage_bookings (owner/admin/staff) — enables every write control */
  canWrite: boolean;
}) {
  const { toast } = useToast();

  const [members, setMembers] = React.useState<TeamMember[]>([]);
  const [membersError, setMembersError] = React.useState<string | null>(null);

  const [weekStart, setWeekStart] = React.useState(() => isoDay(new Date()));
  const [grid, setGrid] = React.useState<WeekGrid | null>(null);
  const [gridLoading, setGridLoading] = React.useState(true);
  const [gridError, setGridError] = React.useState<string | null>(null);
  const [coverage, setCoverage] = React.useState<CoverageDay[] | null>(null);

  const [entries, setEntries] = React.useState<TimeEntry[] | null>(null);
  const [entriesLoading, setEntriesLoading] = React.useState(false);
  const [entriesError, setEntriesError] = React.useState<string | null>(null);
  const [clockAgent, setClockAgent] = React.useState("");

  const [leave, setLeave] = React.useState<LeaveRequest[] | null>(null);
  const [leaveLoading, setLeaveLoading] = React.useState(true);
  const [leaveError, setLeaveError] = React.useState<string | null>(null);

  const [utilFrom, setUtilFrom] = React.useState(() => addDays(isoDay(new Date()), -6));
  const [utilTo, setUtilTo] = React.useState(() => addDays(isoDay(new Date()), 1));
  const [util, setUtil] = React.useState<UtilizationRow[] | null>(null);
  const [utilLoading, setUtilLoading] = React.useState(true);
  const [utilError, setUtilError] = React.useState<string | null>(null);

  const [dialogOpen, setDialogOpen] = React.useState(false);
  const [editing, setEditing] = React.useState<ShiftView | null>(null);
  const [busy, setBusy] = React.useState(false);

  const loadMembers = React.useCallback(
    async (signal?: AbortSignal) => {
      try {
        const data = await api.get(`${BASE}/team-members`, { tenant: orgSlug }, signal);
        if (signal?.aborted) return;
        setMembers(unwrap<TeamMember>(data));
        setMembersError(null);
      } catch (e) {
        if (signal?.aborted) return;
        setMembers([]);
        setMembersError(apiMessage(e, "Team members could not be loaded."));
      }
    },
    [orgSlug],
  );

  const loadWeek = React.useCallback(
    async (signal?: AbortSignal) => {
      setGridLoading(true);
      setGridError(null);
      try {
        const data = await api.get<WeekGrid>(
          `${BASE}/shifts/week?from=${weekStart}`,
          { tenant: orgSlug },
          signal,
        );
        if (signal?.aborted) return;
        setGrid(data);
      } catch (e) {
        if (signal?.aborted) return;
        setGrid(null);
        setGridError(
          e instanceof ApiError && e.status !== 404
            ? e.message
            : "Workforce is not available yet — the booking-service workforce API may still be rolling out.",
        );
      } finally {
        if (!signal?.aborted) setGridLoading(false);
      }
      // Coverage is a secondary feed: failures degrade to hidden day counts,
      // never to a grid-blocking error.
      try {
        const data = await api.get(
          `${BASE}/coverage?from=${weekStart}&to=${addDays(weekStart, 7)}`,
          { tenant: orgSlug },
          signal,
        );
        if (signal?.aborted) return;
        setCoverage(unwrap<CoverageDay>(data));
      } catch {
        if (!signal?.aborted) setCoverage(null);
      }
    },
    [orgSlug, weekStart],
  );

  const loadEntries = React.useCallback(
    async (agentId: string, signal?: AbortSignal) => {
      if (!agentId) {
        setEntries(null);
        return;
      }
      setEntriesLoading(true);
      setEntriesError(null);
      try {
        const data = await api.get(
          `${BASE}/time/entries?agent_id=${agentId}`,
          { tenant: orgSlug },
          signal,
        );
        if (signal?.aborted) return;
        setEntries(unwrap<TimeEntry>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setEntries(null);
        setEntriesError(apiMessage(e, "Time entries could not be loaded."));
      } finally {
        if (!signal?.aborted) setEntriesLoading(false);
      }
    },
    [orgSlug],
  );

  const loadLeave = React.useCallback(
    async (signal?: AbortSignal) => {
      setLeaveLoading(true);
      setLeaveError(null);
      try {
        const data = await api.get(`${BASE}/leave`, { tenant: orgSlug }, signal);
        if (signal?.aborted) return;
        setLeave(unwrap<LeaveRequest>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setLeave(null);
        setLeaveError(apiMessage(e, "Leave requests could not be loaded."));
      } finally {
        if (!signal?.aborted) setLeaveLoading(false);
      }
    },
    [orgSlug],
  );

  const loadUtilization = React.useCallback(
    async (from: string, to: string, signal?: AbortSignal) => {
      setUtilLoading(true);
      setUtilError(null);
      try {
        const data = await api.get(
          `${BASE}/utilization?from=${from}&to=${to}`,
          { tenant: orgSlug },
          signal,
        );
        if (signal?.aborted) return;
        setUtil(unwrap<UtilizationRow>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setUtil(null);
        setUtilError(apiMessage(e, "Utilization could not be loaded."));
      } finally {
        if (!signal?.aborted) setUtilLoading(false);
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void loadMembers(controller.signal);
    void loadLeave(controller.signal);
    return () => controller.abort();
  }, [loadMembers, loadLeave]);

  React.useEffect(() => {
    const controller = new AbortController();
    void loadWeek(controller.signal);
    return () => controller.abort();
  }, [loadWeek]);

  React.useEffect(() => {
    const controller = new AbortController();
    void loadUtilization(utilFrom, utilTo, controller.signal);
    return () => controller.abort();
  }, [loadUtilization, utilFrom, utilTo]);

  const reload = React.useCallback(async () => {
    await Promise.all([
      loadMembers(),
      loadWeek(),
      loadLeave(),
      loadUtilization(utilFrom, utilTo),
      clockAgent ? loadEntries(clockAgent) : Promise.resolve(),
    ]);
  }, [loadMembers, loadWeek, loadLeave, loadUtilization, loadEntries, utilFrom, utilTo, clockAgent]);

  // ------------------------------------------------------------------
  // mutations
  // ------------------------------------------------------------------

  const saveShift = async (input: ShiftInput, id?: string): Promise<boolean> => {
    setBusy(true);
    try {
      if (id) {
        await api.patch(`${BASE}/shifts/${id}`, input, { tenant: orgSlug });
        toast({ title: "Shift updated", variant: "success" });
      } else {
        await api.post(`${BASE}/shifts`, input, { tenant: orgSlug });
        toast({ title: "Shift created", variant: "success" });
      }
      await loadWeek();
      return true;
    } catch (e) {
      toast({
        title: id ? "Update failed" : "Create failed",
        description: apiMessage(e, "Unexpected error"),
        variant: "destructive",
      });
      return false;
    } finally {
      setBusy(false);
    }
  };

  const clockIn = async (input: ClockInInput): Promise<boolean> => {
    setBusy(true);
    try {
      await api.post(`${BASE}/time/clock-in`, input, { tenant: orgSlug });
      toast({ title: "Clocked in", variant: "success" });
      await loadEntries(input.agent_id);
      return true;
    } catch (e) {
      toast({
        title: "Clock-in failed",
        description: apiMessage(e, "Unexpected error"),
        variant: "destructive",
      });
      return false;
    } finally {
      setBusy(false);
    }
  };

  const clockOut = async (agentId: string): Promise<boolean> => {
    setBusy(true);
    try {
      await api.post(`${BASE}/time/clock-out`, { agent_id: agentId }, { tenant: orgSlug });
      toast({ title: "Clocked out", variant: "success" });
      await loadEntries(agentId);
      return true;
    } catch (e) {
      toast({
        title: "Clock-out failed",
        description: apiMessage(e, "Unexpected error"),
        variant: "destructive",
      });
      return false;
    } finally {
      setBusy(false);
    }
  };

  const fileLeave = async (input: LeaveInput): Promise<boolean> => {
    setBusy(true);
    try {
      await api.post(`${BASE}/leave`, input, { tenant: orgSlug });
      toast({ title: "Leave request filed", variant: "success" });
      await loadLeave();
      return true;
    } catch (e) {
      toast({
        title: "Filing failed",
        description: apiMessage(e, "Unexpected error"),
        variant: "destructive",
      });
      return false;
    } finally {
      setBusy(false);
    }
  };

  const decideLeave = async (id: string, action: "approve" | "decline"): Promise<boolean> => {
    setBusy(true);
    try {
      await api.patch(`${BASE}/leave/${id}`, { action }, { tenant: orgSlug });
      toast({ title: action === "approve" ? "Leave approved" : "Leave declined", variant: "success" });
      await loadLeave();
      return true;
    } catch (e) {
      toast({
        title: "Decision failed",
        description: apiMessage(e, "Unexpected error"),
        variant: "destructive",
      });
      return false;
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <PageHeader
        title="Workforce"
        description="Shifts, time tracking and leave — the week grid buckets by UTC day; overlapping shifts for one agent are rejected."
        actions={
          <>
            <Button variant="secondary" size="sm" onClick={() => void reload()} disabled={gridLoading}>
              <RefreshCw className="mr-1 h-3.5 w-3.5" />
              Refresh
            </Button>
            {canWrite ? (
              <Button
                size="sm"
                onClick={() => {
                  setEditing(null);
                  setDialogOpen(true);
                }}
              >
                <Plus className="mr-1 h-3.5 w-3.5" />
                New shift
              </Button>
            ) : null}
          </>
        }
      />

      {membersError ? <ErrorNote message={membersError} /> : null}

      <ShiftFormDialog
        open={dialogOpen}
        shift={editing}
        members={members}
        busy={busy}
        onClose={() => setDialogOpen(false)}
        onSubmit={saveShift}
      />

      <Tabs defaultValue="week">
        <TabsList>
          <TabsTrigger value="week">Week grid</TabsTrigger>
          <TabsTrigger value="time">Time</TabsTrigger>
          <TabsTrigger value="leave">Leave</TabsTrigger>
          <TabsTrigger value="utilization">Utilization</TabsTrigger>
        </TabsList>

        <TabsContent value="week" className="mt-4">
          <div className="mb-3 flex items-center gap-2">
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setWeekStart((d) => addDays(d, -7))}
            >
              <ChevronLeft className="h-3.5 w-3.5" />
              Prev week
            </Button>
            <span className="text-sm text-muted-foreground">
              Week of {grid?.week_start ?? weekStart}
            </span>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setWeekStart((d) => addDays(d, 7))}
            >
              Next week
              <ChevronRight className="h-3.5 w-3.5" />
            </Button>
          </div>
          {gridError ? <ErrorNote message={gridError} /> : null}
          <WeekGridView
            days={grid?.days ?? Array.from({ length: 7 }, (_, i) => addDays(weekStart, i))}
            shifts={grid?.shifts ?? []}
            coverage={coverage}
            loading={gridLoading}
            onSelect={(s) => {
              if (!canWrite) return;
              setEditing(s);
              setDialogOpen(true);
            }}
          />
        </TabsContent>

        <TabsContent value="time" className="mt-4">
          {entriesError ? <ErrorNote message={entriesError} /> : null}
          <ClockPanel
            members={members}
            entries={entries}
            loading={entriesLoading}
            busy={busy}
            canWrite={canWrite}
            onClockIn={clockIn}
            onClockOut={clockOut}
            onAgentChange={(id) => {
              setClockAgent(id);
              void loadEntries(id);
            }}
          />
        </TabsContent>

        <TabsContent value="leave" className="mt-4">
          {leaveError ? <ErrorNote message={leaveError} /> : null}
          <LeaveQueue
            members={members}
            requests={leave}
            loading={leaveLoading}
            busy={busy}
            canWrite={canWrite}
            onFile={fileLeave}
            onDecide={decideLeave}
          />
        </TabsContent>

        <TabsContent value="utilization" className="mt-4">
          {utilError ? <ErrorNote message={utilError} /> : null}
          <UtilizationTable
            rows={util}
            loading={utilLoading}
            from={utilFrom}
            to={utilTo}
            onRangeChange={(f, t) => {
              setUtilFrom(f);
              setUtilTo(t);
            }}
          />
        </TabsContent>
      </Tabs>
    </div>
  );
}
