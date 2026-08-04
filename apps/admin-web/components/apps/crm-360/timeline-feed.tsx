"use client";

/**
 * Timeline feed (SPEC-W20 Agent A): the merged chronological stream
 * across bookings, ticket events, work orders, loyalty ledger entries and
 * crm notes. Pure presentational.
 */
import * as React from "react";
import { Badge } from "@/components/ui/badge";
import { formatDateTime } from "@/lib/utils";
import {
  TIMELINE_KIND_LABELS,
  TIMELINE_KIND_VARIANTS,
  type TimelineItem,
} from "./types";

export function TimelineFeed({
  items,
  loading,
}: {
  items: TimelineItem[];
  loading: boolean;
}) {
  if (loading && items.length === 0) {
    return (
      <div className="space-y-2">
        {Array.from({ length: 4 }).map((_, i) => (
          <div key={i} className="h-10 animate-pulse rounded-md border border-border bg-muted" />
        ))}
      </div>
    );
  }
  if (items.length === 0) {
    return (
      <p className="rounded-md border border-border bg-card px-4 py-6 text-sm text-muted-foreground">
        No activity yet — bookings, ticket events, work orders, loyalty
        movements and notes will merge here as they happen.
      </p>
    );
  }
  return (
    <ul className="space-y-1.5">
      {items.map((it, i) => (
        <li
          key={`${it.ref_id}-${it.kind}-${i}`}
          className="flex items-start gap-3 rounded-md border border-border bg-card px-3 py-2"
        >
          <Badge variant={TIMELINE_KIND_VARIANTS[it.kind] ?? "outline"} className="mt-0.5 shrink-0">
            {TIMELINE_KIND_LABELS[it.kind] ?? it.kind}
          </Badge>
          <div className="min-w-0 flex-1">
            <p className="text-sm text-foreground">{it.summary}</p>
            <p className="text-xs text-muted-foreground">{formatDateTime(it.ts)}</p>
          </div>
        </li>
      ))}
    </ul>
  );
}
