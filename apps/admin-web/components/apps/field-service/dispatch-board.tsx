"use client";

/**
 * Dispatch board (SPEC-W19 Agent B): one lane per work-order status. Each
 * card shows the title, assignee, scheduled window and open-checklist
 * count; clicking a card opens the detail panel. Lanes render even when
 * empty (honest empty states; the backend always returns all six keys).
 */
import * as React from "react";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { formatDateTime } from "@/lib/utils";
import {
  STATUS_LABEL,
  WO_STATUSES,
  statusVariant,
  type Board,
  type BoardItem,
} from "./types";

export function DispatchBoard({
  board,
  loading,
  selectedId,
  onSelect,
}: {
  board: Board | null;
  loading: boolean;
  selectedId: string | null;
  onSelect: (item: BoardItem) => void;
}) {
  if (loading && !board) {
    return (
      <div className="rounded-md border border-border p-8 text-center text-sm text-muted-foreground">
        Loading dispatch board…
      </div>
    );
  }
  if (!board) {
    return (
      <div className="rounded-md border border-border p-8 text-center text-sm text-muted-foreground">
        The dispatch board is not available yet — the booking-service
        field-service API may still be rolling out.
      </div>
    );
  }
  return (
    <div className="grid gap-3 md:grid-cols-3 xl:grid-cols-6">
      {WO_STATUSES.map((status) => {
        const items = board[status] ?? [];
        return (
          <div
            key={status}
            className="flex min-h-40 flex-col rounded-md border border-border bg-muted/30"
          >
            <div className="flex items-center justify-between border-b border-border px-3 py-2">
              <span className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                {STATUS_LABEL[status]}
              </span>
              <Badge variant={statusVariant(status)}>{items.length}</Badge>
            </div>
            <div className="flex flex-1 flex-col gap-2 p-2">
              {items.length === 0 ? (
                <div className="flex flex-1 items-center justify-center rounded border border-dashed border-border p-3 text-center text-xs text-muted-foreground">
                  No orders
                </div>
              ) : (
                items.map((item) => (
                  <button
                    key={item.id}
                    type="button"
                    onClick={() => onSelect(item)}
                    className={cn(
                      "rounded-md border border-border bg-card p-2 text-left shadow-sm transition-colors hover:border-primary/50",
                      selectedId === item.id && "border-primary ring-1 ring-primary/40",
                    )}
                  >
                    <div className="text-sm font-medium leading-snug">{item.title}</div>
                    <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-muted-foreground">
                      <span>{item.assignee_name || "Unassigned"}</span>
                      {item.scheduled_start ? (
                        <span>· {formatDateTime(item.scheduled_start)}</span>
                      ) : null}
                    </div>
                    {item.checklist.length > 0 ? (
                      <div className="mt-1 text-xs text-muted-foreground">
                        {item.checklist.filter((c) => c.done).length}/{item.checklist.length} checklist
                      </div>
                    ) : null}
                  </button>
                ))
              )}
            </div>
          </div>
        );
      })}
    </div>
  );
}
