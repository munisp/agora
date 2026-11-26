"use client";

import * as React from "react";
import Link from "next/link";
import { ChevronLeft, ChevronRight, RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { useBookingEvents } from "@/lib/ws";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { addDays, toISODate, listFrom } from "@/lib/utils";
import type { Booking, TeamMember } from "@/lib/types";

const HOUR_PX = 56;
const POLL_MS = 10_000;

function parseDay(s?: string): Date {
  if (s && /^\d{4}-\d{2}-\d{2}$/.test(s)) {
    const [y, m, d] = s.split("-").map(Number);
    return new Date(y, m - 1, d);
  }
  const now = new Date();
  return new Date(now.getFullYear(), now.getMonth(), now.getDate());
}

function minutesOfDay(iso: string): number {
  const d = new Date(iso);
  return d.getHours() * 60 + d.getMinutes();
}

function hhmm(iso: string): string {
  return new Date(iso).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
}

const STATUS_STYLE: Record<string, string> = {
  confirmed: "border-l-emerald-600 bg-emerald-50 text-emerald-950",
  pending: "border-l-amber-500 bg-amber-50 text-amber-950",
  completed: "border-l-stone-400 bg-stone-100 text-stone-700",
  cancelled: "border-l-red-400 bg-red-50 text-red-900 line-through opacity-60",
  no_show: "border-l-red-400 bg-red-50 text-red-900 opacity-70",
};

/**
 * Advisor calendar: one column per active team member and the selected day's
 * bookings placed by time. Live booking events (and a 10 s poll as a fallback)
 * reload the day, and a booking that was not there before is highlighted, so
 * the office sees an appointment land in the advisor's diary as the
 * receptionist books it.
 */
export function CalendarClient({
  orgSlug,
  token,
  initialDate,
}: {
  orgSlug: string;
  token?: string;
  initialDate?: string;
}) {
  const [day, setDay] = React.useState<Date>(() => parseDay(initialDate));
  const [members, setMembers] = React.useState<TeamMember[]>([]);
  const [bookings, setBookings] = React.useState<Booking[]>([]);
  const [fresh, setFresh] = React.useState<Set<string>>(new Set());
  const [error, setError] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(false);
  const known = React.useRef<Set<string> | null>(null);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const [m, b] = await Promise.all([
        api.get<TeamMember[] | { items: TeamMember[] }>("/api/bookings/v1/team-members", { tenant: orgSlug }),
        api.get<Booking[] | { items: Booking[] }>("/api/bookings/v1/bookings", {
          tenant: orgSlug,
          from: toISODate(day),
          to: toISODate(addDays(day, 1)),
          limit: 500,
        }),
      ]);
      const items = listFrom(b).filter((x) => x.status !== "cancelled");
      const ids = new Set(items.map((x) => x.id));
      // New = not there on the previous load, or booked in the last 3 minutes
      // (so a booking still stands out right after jumping to its day).
      const recent = items.filter((x) => Date.now() - new Date(x.created_at).getTime() < 180_000).map((x) => x.id);
      if (recent.length && !known.current) {
        setFresh(new Set(recent));
        window.setTimeout(() => setFresh(new Set()), 12_000);
      }
      if (known.current) {
        const added = items.filter((x) => !known.current!.has(x.id)).map((x) => x.id);
        if (added.length) {
          setFresh(new Set(added));
          window.setTimeout(() => setFresh(new Set()), 12_000);
        }
      }
      known.current = ids;
      setMembers(listFrom(m).filter((x) => x.active !== false));
      setBookings(items);
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to load the calendar.");
    } finally {
      setLoading(false);
    }
  }, [orgSlug, day]);

  React.useEffect(() => {
    known.current = null;
    void load();
  }, [load]);

  React.useEffect(() => {
    const t = window.setInterval(() => void load(), POLL_MS);
    return () => window.clearInterval(t);
  }, [load]);

  const { connected } = useBookingEvents(orgSlug, token, () => void load());

  // Grid hours: 8:00-18:00, widened to fit any booking outside them.
  const startH = Math.min(8, ...bookings.map((b) => Math.floor(minutesOfDay(b.starts_at) / 60)));
  const endH = Math.max(18, ...bookings.map((b) => Math.ceil(minutesOfDay(b.ends_at) / 60)));
  const hours = Array.from({ length: endH - startH }, (_, i) => startH + i);
  const columns = members.length ? members : [{ id: "_", name: "Unassigned" } as TeamMember];
  const isToday = toISODate(day) === toISODate(new Date());

  return (
    <div>
      <PageHeader
        title="Advisor calendar"
        description="Every advisor's day. A booking appears in the advisor's column the moment it is made, by the receptionist, the website or the office."
        actions={
          <>
            <Badge variant={connected ? "success" : "outline"}>{connected ? "Live" : "Offline"}</Badge>
            <Link href={`/app/${orgSlug}/bookings`}>
              <Button variant="outline" size="sm">List view</Button>
            </Link>
            <Button variant="outline" size="sm" onClick={() => void load()} disabled={loading}>
              <RefreshCw className={loading ? "h-3.5 w-3.5 animate-spin" : "h-3.5 w-3.5"} />
              Refresh
            </Button>
          </>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <div className="mb-3 flex items-center gap-2">
        <Button variant="outline" size="sm" aria-label="Previous day" onClick={() => setDay((d) => addDays(d, -1))}>
          <ChevronLeft className="h-4 w-4" />
        </Button>
        <Button variant="outline" size="sm" aria-label="Next day" onClick={() => setDay((d) => addDays(d, 1))}>
          <ChevronRight className="h-4 w-4" />
        </Button>
        {!isToday ? (
          <Button variant="ghost" size="sm" onClick={() => setDay(parseDay())}>Today</Button>
        ) : null}
        <div className="ml-1 text-sm font-medium">
          {day.toLocaleDateString([], { weekday: "long", day: "numeric", month: "long", year: "numeric" })}
        </div>
        <div className="ml-auto text-xs text-muted-foreground">
          {bookings.length} booking{bookings.length === 1 ? "" : "s"}
        </div>
      </div>

      <Card className="overflow-x-auto">
        <div className="grid min-w-[480px]" style={{ gridTemplateColumns: `44px repeat(${columns.length}, minmax(84px, 1fr))` }}>
          <div className="border-b border-border" />
          {columns.map((m) => (
            <div key={m.id} className="truncate border-b border-l border-border px-2 py-2 text-xs font-semibold">
              {m.name}
            </div>
          ))}
          <div className="relative" style={{ height: hours.length * HOUR_PX }}>
            {hours.map((h, i) => (
              <div key={h} className="absolute right-1 text-[10px] text-muted-foreground" style={{ top: i * HOUR_PX - 6 }}>
                {new Date(2000, 0, 1, h).toLocaleTimeString([], { hour: "numeric" })}
              </div>
            ))}
          </div>
          {columns.map((m) => (
            <div key={m.id} className="relative border-l border-border" style={{ height: hours.length * HOUR_PX }}>
              {hours.map((h, i) => (
                <div key={h} className="absolute inset-x-0 border-t border-border/60" style={{ top: i * HOUR_PX }} />
              ))}
              {bookings
                .filter((b) => (b.team_member_id ?? "_") === m.id)
                .map((b) => {
                  const top = ((minutesOfDay(b.starts_at) - startH * 60) * HOUR_PX) / 60;
                  const height = Math.max(26, ((minutesOfDay(b.ends_at) - minutesOfDay(b.starts_at)) * HOUR_PX) / 60 - 2);
                  return (
                    <Link
                      key={b.id}
                      href={`/app/${orgSlug}/bookings/${b.id}`}
                      data-booking={b.id}
                      className={`absolute inset-x-1 overflow-hidden rounded-md border-l-4 px-1.5 py-1 text-[10.5px] leading-tight shadow-sm transition ${STATUS_STYLE[b.status] ?? STATUS_STYLE.confirmed} ${fresh.has(b.id) ? "animate-pulse ring-2 ring-amber-500" : ""}`}
                      style={{ top, height }}
                      title={`${hhmm(b.starts_at)} ${b.offering_name ?? ""} — ${b.contact_name ?? ""}`}
                    >
                      <div className="font-semibold">{hhmm(b.starts_at)} · {b.contact_name ?? "Contributor"}</div>
                      <div className="truncate opacity-80">{b.offering_name}</div>
                      {fresh.has(b.id) ? <div className="mt-0.5 font-semibold text-amber-700">New</div> : null}
                    </Link>
                  );
                })}
            </div>
          ))}
        </div>
      </Card>
    </div>
  );
}
