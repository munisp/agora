"use client";

/**
 * Helpdesk stats header tiles (SPEC-W19 Agent A): open tickets, current SLA
 * breaches, 30-day averages for first response / resolve and CSAT. Data:
 * GET /api/bookings/v1/helpdesk/stats.
 */
import { AlertTriangle, Clock, Inbox, Star, Timer } from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { formatMinutes, type HelpdeskStats } from "@/components/apps/helpdesk/types";

export function StatsTiles({
  stats,
  loading,
}: {
  stats: HelpdeskStats | null;
  loading: boolean;
}) {
  if (loading && !stats) {
    return (
      <div className="mb-6 grid grid-cols-2 gap-3 md:grid-cols-5">
        {Array.from({ length: 5 }).map((_, i) => (
          <div
            key={i}
            className="h-20 animate-pulse rounded-lg border border-border bg-muted"
          />
        ))}
      </div>
    );
  }
  if (!stats) return null;

  const tiles = [
    {
      icon: Inbox,
      label: "Open tickets",
      value: String(stats.open_count),
      tone: "text-foreground",
    },
    {
      icon: AlertTriangle,
      label: "SLA breaches",
      value: String(stats.breached_count),
      tone: stats.breached_count > 0 ? "text-destructive" : "text-foreground",
    },
    {
      icon: Clock,
      label: "Avg first response (30d)",
      value: formatMinutes(stats.avg_first_response_minutes_30d),
      tone: "text-foreground",
    },
    {
      icon: Timer,
      label: "Avg resolve (30d)",
      value: formatMinutes(stats.avg_resolve_minutes_30d),
      tone: "text-foreground",
    },
    {
      icon: Star,
      label: "Avg CSAT (30d)",
      value:
        stats.avg_csat_30d === null ? "—" : stats.avg_csat_30d.toFixed(1) + " / 5",
      tone: "text-foreground",
    },
  ];

  return (
    <div className="mb-6 grid grid-cols-2 gap-3 md:grid-cols-5">
      {tiles.map((t) => (
        <Card key={t.label}>
          <CardContent className="flex items-center gap-3 p-4">
            <t.icon className="h-5 w-5 shrink-0 text-muted-foreground" />
            <div className="min-w-0">
              <p className={`text-xl font-semibold ${t.tone}`}>{t.value}</p>
              <p className="truncate text-xs text-muted-foreground">{t.label}</p>
            </div>
          </CardContent>
        </Card>
      ))}
    </div>
  );
}
