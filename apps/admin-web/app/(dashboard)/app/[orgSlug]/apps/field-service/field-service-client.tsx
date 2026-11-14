"use client";

/**
 * Field-service app client (SPEC-W19 Agent B): dispatch board (status
 * lanes), work-order detail panel and the today view.
 *
 * Data sources (all through the BFF with the x-tenant-slug header):
 *   - GET   /api/bookings/v1/field-service/board
 *   - GET   /api/bookings/v1/field-service/today
 *   - POST  /api/bookings/v1/field-service/work-orders
 *   - PATCH /api/bookings/v1/field-service/work-orders/{id}
 *   - POST  /api/bookings/v1/field-service/work-orders/{id}/dispatch
 *
 * List payloads ride the tolerant unwrap<T>() envelope convention
 * (components/apps/types.ts) — the board endpoint answers
 * {"board": {status: [...]}} which we read keyed (unwrap is for arrays).
 */
import * as React from "react";
import { Plus, RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { useToast } from "@/components/ui/toast";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { DispatchBoard } from "@/components/apps/field-service/dispatch-board";
import { WorkOrderDetail } from "@/components/apps/field-service/workorder-detail";
import { TodayView } from "@/components/apps/field-service/today-view";
import {
  WorkOrderCreateForm,
  type NewWorkOrder,
} from "@/components/apps/field-service/workorder-create-form";
import type { Board, BoardItem } from "@/components/apps/field-service/types";

const BASE = "/api/bookings/v1/field-service";

export function FieldServiceClient({
  orgSlug,
  canWrite,
}: {
  orgSlug: string;
  /** manage_bookings (owner/admin/staff) — enables every write control */
  canWrite: boolean;
}) {
  const { toast } = useToast();
  const [board, setBoard] = React.useState<Board | null>(null);
  const [boardLoading, setBoardLoading] = React.useState(true);
  const [boardError, setBoardError] = React.useState<string | null>(null);

  const [today, setToday] = React.useState<BoardItem[]>([]);
  const [todayDayStart, setTodayDayStart] = React.useState<string | null>(null);
  const [todayLoading, setTodayLoading] = React.useState(true);
  const [todayError, setTodayError] = React.useState<string | null>(null);

  const [selected, setSelected] = React.useState<BoardItem | null>(null);
  const [creating, setCreating] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  const loadBoard = React.useCallback(
    async (signal?: AbortSignal) => {
      setBoardLoading(true);
      setBoardError(null);
      try {
        const data = await api.get<{ board?: Board }>(
          `${BASE}/board`,
          { tenant: orgSlug },
          signal,
        );
        if (signal?.aborted) return;
        setBoard(data?.board ?? null);
      } catch (e) {
        if (signal?.aborted) return;
        setBoard(null);
        setBoardError(
          e instanceof ApiError && e.status !== 404
            ? e.message
            : "Field service is not available yet — the booking-service field-service API may still be rolling out.",
        );
      } finally {
        if (!signal?.aborted) setBoardLoading(false);
      }
    },
    [orgSlug],
  );

  const loadToday = React.useCallback(
    async (signal?: AbortSignal) => {
      setTodayLoading(true);
      setTodayError(null);
      try {
        const data = await api.get<{
          work_orders?: BoardItem[];
          day_start?: string;
        }>(`${BASE}/today`, { tenant: orgSlug }, signal);
        if (signal?.aborted) return;
        setToday(data?.work_orders ?? []);
        setTodayDayStart(data?.day_start ?? null);
      } catch (e) {
        if (signal?.aborted) return;
        setToday([]);
        setTodayDayStart(null);
        setTodayError(
          e instanceof ApiError && e.status !== 404
            ? e.message
            : "Today view is not available yet.",
        );
      } finally {
        if (!signal?.aborted) setTodayLoading(false);
      }
    },
    [orgSlug],
  );

  const reload = React.useCallback(async () => {
    await Promise.all([loadBoard(), loadToday()]);
  }, [loadBoard, loadToday]);

  React.useEffect(() => {
    const controller = new AbortController();
    void loadBoard(controller.signal);
    void loadToday(controller.signal);
    return () => controller.abort();
  }, [loadBoard, loadToday]);

  const create = async (input: NewWorkOrder): Promise<boolean> => {
    setBusy(true);
    try {
      await api.post(`${BASE}/work-orders`, input, { tenant: orgSlug });
      toast({ title: "Work order created", variant: "success" });
      setCreating(false);
      await reload();
      return true;
    } catch (e) {
      toast({
        title: "Create failed",
        description: e instanceof ApiError ? e.message : "Unexpected error",
        variant: "destructive",
      });
      return false;
    } finally {
      setBusy(false);
    }
  };

  const patch = async (id: string, body: Record<string, unknown>): Promise<boolean> => {
    setBusy(true);
    try {
      const data = await api.patch<{ work_order?: BoardItem }>(
        `${BASE}/work-orders/${id}`,
        body,
        { tenant: orgSlug },
      );
      toast({ title: "Work order updated", variant: "success" });
      await reload();
      if (selected?.id === id && data?.work_order) {
        setSelected({ ...data.work_order, assignee_name: selected.assignee_name });
      }
      return true;
    } catch (e) {
      toast({
        title: "Update failed",
        description: e instanceof ApiError ? e.message : "Unexpected error",
        variant: "destructive",
      });
      return false;
    } finally {
      setBusy(false);
    }
  };

  const dispatch = async (id: string, assignee: string, notify: boolean): Promise<boolean> => {
    setBusy(true);
    try {
      const data = await api.post<{ work_order?: BoardItem; notified?: boolean }>(
        `${BASE}/work-orders/${id}/dispatch`,
        { assignee_id: assignee, notify },
        { tenant: orgSlug },
      );
      toast({
        title: data?.notified ? "Dispatched (push enqueued)" : "Dispatched",
        description:
          notify && !data?.notified
            ? "Push notification was skipped (notifications disabled or unavailable)."
            : undefined,
        variant: "success",
      });
      await reload();
      return true;
    } catch (e) {
      toast({
        title: "Dispatch failed",
        description: e instanceof ApiError ? e.message : "Unexpected error",
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
        title="Field service"
        description="Work orders & dispatch — board lanes follow the status machine (created → assigned → en route → on site → completed)."
        actions={
          <>
            <Button variant="secondary" size="sm" onClick={() => void reload()} disabled={boardLoading}>
              <RefreshCw className="mr-1 h-3.5 w-3.5" />
              Refresh
            </Button>
            {canWrite ? (
              <Button size="sm" onClick={() => setCreating((v) => !v)}>
                <Plus className="mr-1 h-3.5 w-3.5" />
                New work order
              </Button>
            ) : null}
          </>
        }
      />

      {creating && canWrite ? (
        <div className="mb-4">
          <WorkOrderCreateForm busy={busy} onCreate={create} onCancel={() => setCreating(false)} />
        </div>
      ) : null}

      {selected ? (
        <div className="mb-4">
          <WorkOrderDetail
            item={selected}
            canWrite={canWrite}
            busy={busy}
            onClose={() => setSelected(null)}
            onPatch={patch}
            onDispatch={dispatch}
          />
        </div>
      ) : null}

      <Tabs defaultValue="board">
        <TabsList>
          <TabsTrigger value="board">Dispatch board</TabsTrigger>
          <TabsTrigger value="today">Today</TabsTrigger>
        </TabsList>
        <TabsContent value="board" className="mt-4">
          {boardError ? <ErrorNote message={boardError} /> : null}
          <DispatchBoard
            board={board}
            loading={boardLoading}
            selectedId={selected?.id ?? null}
            onSelect={(item) => setSelected(item)}
          />
        </TabsContent>
        <TabsContent value="today" className="mt-4">
          {todayError ? <ErrorNote message={todayError} /> : null}
          <TodayView
            rows={today}
            loading={todayLoading}
            dayStart={todayDayStart}
            onSelect={(item) => setSelected(item)}
          />
        </TabsContent>
      </Tabs>
    </div>
  );
}
