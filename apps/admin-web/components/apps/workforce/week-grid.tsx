"use client";

/**
 * Week grid (SPEC-W20 Agent D): 7 day columns, shift cards bucketed by UTC
 * day (the backend's bucketing — times render as UTC HH:mm). Day headers
 * carry the coverage line (agents scheduled vs bookings) when the coverage
 * feed loaded. Clicking a card opens the edit dialog.
 */
import * as React from "react";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import {
  SHIFT_LABEL,
  dayOf,
  shiftVariant,
  timeOf,
  type CoverageDay,
  type ShiftView,
} from "./types";

export function WeekGridView({
  days,
  shifts,
  coverage,
  loading,
  onSelect,
}: {
  days: string[];
  shifts: ShiftView[];
  coverage: CoverageDay[] | null;
  loading: boolean;
  onSelect: (shift: ShiftView) => void;
}) {
  if (loading && shifts.length === 0) {
    return (
      <div className="rounded-md border border-border p-8 text-center text-sm text-muted-foreground">
        Loading week grid…
      </div>
    );
  }
  const byDay = new Map<string, ShiftView[]>();
  for (const d of days) byDay.set(d, []);
  for (const s of shifts) {
    // A shift can span midnight — list it on every day it touches.
    const start = dayOf(s.starts_at);
    const end = dayOf(new Date(new Date(s.ends_at).getTime() - 1).toISOString());
    for (const d of days) {
      if (d >= start && d <= end) byDay.get(d)?.push(s);
    }
  }
  const coverageByDay = new Map((coverage ?? []).map((c) => [c.date, c]));

  return (
    <div className="grid gap-3 md:grid-cols-4 xl:grid-cols-7">
      {days.map((day) => {
        const items = byDay.get(day) ?? [];
        const cov = coverageByDay.get(day);
        return (
          <div
            key={day}
            className="flex min-h-40 flex-col rounded-md border border-border bg-muted/30"
          >
            <div className="border-b border-border px-3 py-2">
              <div className="flex items-center justify-between">
                <span className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                  {day}
                </span>
                <Badge variant="secondary">{items.length}</Badge>
              </div>
              {cov ? (
                <div className="mt-0.5 text-[11px] text-muted-foreground">
                  {cov.agents_scheduled} agent{cov.agents_scheduled === 1 ? "" : "s"} ·{" "}
                  {cov.bookings} booking{cov.bookings === 1 ? "" : "s"}
                </div>
              ) : null}
            </div>
            <div className="flex flex-1 flex-col gap-2 p-2">
              {items.length === 0 ? (
                <div className="flex flex-1 items-center justify-center rounded border border-dashed border-border p-3 text-center text-xs text-muted-foreground">
                  No shifts
                </div>
              ) : (
                items.map((s) => (
                  <button
                    key={s.id + day}
                    type="button"
                    onClick={() => onSelect(s)}
                    className={cn(
                      "rounded-md border border-border bg-card p-2 text-left shadow-sm transition-colors hover:border-primary/50",
                      s.status === "cancelled" && "opacity-60",
                    )}
                  >
                    <div className="flex items-center justify-between gap-2">
                      <span className="text-sm font-medium leading-snug">
                        {s.agent_name || "Unknown agent"}
                      </span>
                      <Badge variant={shiftVariant(s.status)}>{SHIFT_LABEL[s.status]}</Badge>
                    </div>
                    <div className="mt-1 text-xs text-muted-foreground">
                      {timeOf(s.starts_at)}–{timeOf(s.ends_at)} UTC
                      {s.role ? ` · ${s.role}` : ""}
                    </div>
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
