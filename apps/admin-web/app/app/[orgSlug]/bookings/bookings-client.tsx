"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { useBookingEvents } from "@/lib/ws";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { BookingStatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { CalendarLite } from "@/components/ui/calendar";
import { ConfirmDialog, Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Label } from "@/components/ui/input";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useToast } from "@/components/ui/toast";
import { addDays, formatDateTime, minutesToTime, titleCase, toISODate } from "@/lib/utils";
import { type BookingLabels } from "@/lib/terminology";
import type { Booking, BookingEvent, EscalationRequestedEvent, WsEvent } from "@/lib/types";

type Range = "today" | "upcoming" | "past";

/**
 * SPEC-W46 AW-4: bounded list pages. booking-service honors limit up to 500
 * (no cursor), so "load more" refetches with a larger limit. Must match the
 * server-side fetch in page.tsx.
 */
const PAGE_SIZE = 100;
const MAX_LIMIT = 500;

function rangeQuery(range: Range): { from: string; to: string } {
  const now = new Date();
  if (range === "today")
    return { from: toISODate(now), to: toISODate(addDays(now, 1)) };
  if (range === "upcoming")
    return { from: toISODate(addDays(now, 1)), to: toISODate(addDays(now, 30)) };
  return { from: toISODate(addDays(now, -30)), to: toISODate(now) };
}

export function BookingsClient({
  orgSlug,
  token,
  initialBookings,
  initialLabels,
  initialError,
}: {
  orgSlug: string;
  token?: string;
  /** SPEC-W46 AW-2: default range ("upcoming") fetched server-side. */
  initialBookings: Booking[];
  initialLabels: BookingLabels;
  initialError: string | null;
}) {
  const { toast } = useToast();
  const router = useRouter();
  const [range, setRange] = React.useState<Range>("upcoming");
  const [bookings, setBookings] = React.useState<Booking[]>(initialBookings);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState<string | null>(initialError);
  const [limit, setLimit] = React.useState(PAGE_SIZE);
  const [rescheduleTarget, setRescheduleTarget] = React.useState<Booking | null>(null);
  const [cancelTarget, setCancelTarget] = React.useState<Booking | null>(null);
  const [busy, setBusy] = React.useState(false);
  // Pack/tenant-aware copy ("Bookings" -> "Reservations", "Guest" -> "Patient", …).
  const [labels, setLabels] = React.useState<BookingLabels>(initialLabels);

  // Sync when the server props change (router.refresh()).
  React.useEffect(() => {
    setBookings(initialBookings);
    setLabels(initialLabels);
    setError(initialError);
  }, [initialBookings, initialLabels, initialError]);

  const load = React.useCallback(
    async (r: Range, effectiveLimit?: number) => {
      const lim = effectiveLimit ?? limit;
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<Booking[] | { items: Booking[] }>(
          "/api/bookings/v1/bookings",
          { tenant: orgSlug, ...rangeQuery(r), limit: lim },
        );
        const items = Array.isArray(data) ? data : (data.items ?? []);
        setBookings(
          items.slice().sort((a, b) =>
            r === "past"
              ? b.starts_at.localeCompare(a.starts_at)
              : a.starts_at.localeCompare(b.starts_at),
          ),
        );
      } catch (e) {
        setError(e instanceof ApiError ? e.message : "Failed to load bookings.");
      } finally {
        setLoading(false);
      }
    },
    [orgSlug, limit],
  );

  // The default range arrives server-rendered; refetch only on range switch.
  const skipInitialLoad = React.useRef(true);
  React.useEffect(() => {
    if (skipInitialLoad.current) {
      skipInitialLoad.current = false;
      return;
    }
    setLimit(PAGE_SIZE);
    void load(range, PAGE_SIZE);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [range]);

  const loadMore = () => {
    const next = Math.min(limit + PAGE_SIZE, MAX_LIMIT);
    setLimit(next);
    void load(range, next);
  };
  // A full page means the server may hold more rows in this range.
  const canLoadMore = bookings.length >= limit && limit < MAX_LIMIT;

  // SPEC-W45 K15(e): the staff LiveKit join token is no longer published on
  // the events topic. When an operator picks up the escalation we mint a
  // token on demand via the staff-gated voice-admin endpoint, then navigate
  // to the call page with it.
  const joinEscalation = React.useCallback(
    async (conversationId: string) => {
      try {
        const res = await api.post<{ room: string; token: string }>(
          `/api/voice-admin/escalations/${encodeURIComponent(conversationId)}/staff-token`,
          {},
        );
        const params = new URLSearchParams({ room: res.room, token: res.token });
        router.push(`/app/${orgSlug}/call?${params.toString()}`);
      } catch (e) {
        toast({
          title: "Could not join the escalation",
          description:
            e instanceof ApiError
              ? e.message
              : "The voice runtime did not issue a staff token.",
          variant: "destructive",
        });
      }
    },
    [orgSlug, router, toast],
  );

  // Live updates from gateway-edge: toast + silent refresh. Also listens for
  // warm-handoff escalations from the voice runtime (innovation 1) and offers
  // the staff member a join action for the LiveKit escalation room.
  const { connected } = useBookingEvents(orgSlug, token, (event: WsEvent) => {
    if (event.type === "EscalationRequested") {
      const data = (event as EscalationRequestedEvent).data;
      toast({
        title: "Human handoff requested",
        description: `The receptionist asked a human to take over (room ${data.room}).`,
        variant: "warning",
        actionLabel: "Join the call",
        onAction: () => void joinEscalation(data.conversation_id),
      });
      return;
    }
    const bookingEvent = event as BookingEvent;
    const who = bookingEvent.booking?.contact_name ?? "A guest";
    const what = titleCase(event.type.replace(/^Booking/, ""));
    toast({
      title: `Booking ${what.toLowerCase()}`,
      description: `${who} · ${formatDateTime(bookingEvent.booking?.starts_at ?? bookingEvent.time)}`,
      variant: event.type.includes("Cancelled") ? "warning" : "success",
    });
    void load(range);
  });

  const reschedule = async (startsAt: string) => {
    if (!rescheduleTarget) return;
    setBusy(true);
    try {
      await api.post(
        `/api/bookings/v1/bookings/${rescheduleTarget.id}/reschedule`,
        { starts_at: startsAt },
        { tenant: orgSlug },
      );
      toast({ title: "Booking rescheduled", variant: "success" });
      setRescheduleTarget(null);
      await load(range);
    } catch (e) {
      toast({
        title: "Reschedule failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const cancel = async () => {
    if (!cancelTarget) return;
    setBusy(true);
    try {
      await api.post(
        `/api/bookings/v1/bookings/${cancelTarget.id}/cancel`,
        {},
        { tenant: orgSlug },
      );
      toast({ title: "Booking cancelled", variant: "success" });
      setCancelTarget(null);
      await load(range);
    } catch (e) {
      toast({
        title: "Cancel failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <PageHeader
        title={labels.bookingPlural}
        description="Everything the receptionist and your customers have scheduled."
        actions={
          <>
            <Badge variant={connected ? "success" : "secondary"}>
              {connected ? "Live" : "Offline"}
            </Badge>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void load(range)}
              disabled={loading}
            >
              <RefreshCw className={loading ? "h-3.5 w-3.5 animate-spin" : "h-3.5 w-3.5"} />
              Refresh
            </Button>
          </>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <Tabs value={range} onValueChange={(v) => setRange(v as Range)}>
        <TabsList>
          <TabsTrigger value="today">Today</TabsTrigger>
          <TabsTrigger value="upcoming">Upcoming</TabsTrigger>
          <TabsTrigger value="past">Past</TabsTrigger>
        </TabsList>
      </Tabs>

      <Card className="mt-4">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">When</TableHead>
              <TableHead>{labels.customerTerm}</TableHead>
              <TableHead>Offering</TableHead>
              <TableHead>With</TableHead>
              <TableHead>Source</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="pr-5 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {bookings.length === 0 ? (
              <TableEmpty colSpan={7}>
                {loading
                  ? "Loading…"
                  : `No ${labels.bookingPlural.toLowerCase()} in this range.`}
              </TableEmpty>
            ) : (
              bookings.map((b) => {
                const actionable = b.status === "pending" || b.status === "confirmed";
                return (
                  <TableRow key={b.id}>
                    <TableCell className="pl-5 font-medium">
                      {formatDateTime(b.starts_at)}
                    </TableCell>
                    <TableCell>
                      <div>{b.contact_name ?? b.contact_id}</div>
                      {b.contact_phone ? (
                        <div className="text-xs text-muted-foreground">{b.contact_phone}</div>
                      ) : null}
                    </TableCell>
                    <TableCell>{b.offering_name ?? b.offering_id}</TableCell>
                    <TableCell>{b.team_member_name ?? "—"}</TableCell>
                    <TableCell>
                      <Badge variant="outline">{titleCase(b.source)}</Badge>
                    </TableCell>
                    <TableCell>
                      <BookingStatusBadge status={b.status} />
                    </TableCell>
                    <TableCell className="pr-5 text-right">
                      {actionable ? (
                        <div className="flex justify-end gap-2">
                          <Button
                            variant="outline"
                            size="sm"
                            onClick={() => setRescheduleTarget(b)}
                          >
                            Reschedule
                          </Button>
                          <Button
                            variant="ghost"
                            size="sm"
                            className="text-destructive"
                            onClick={() => setCancelTarget(b)}
                          >
                            Cancel
                          </Button>
                        </div>
                      ) : (
                        <span className="text-xs text-muted-foreground">—</span>
                      )}
                    </TableCell>
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </Card>

      {canLoadMore ? (
        <div className="mt-3 flex justify-center">
          <Button
            variant="outline"
            size="sm"
            onClick={loadMore}
            disabled={loading}
          >
            {loading ? "Loading…" : "Load more"}
          </Button>
        </div>
      ) : null}

      <RescheduleDialog
        booking={rescheduleTarget}
        busy={busy}
        onClose={() => setRescheduleTarget(null)}
        onConfirm={reschedule}
      />
      <ConfirmDialog
        open={cancelTarget !== null}
        onOpenChange={(open) => !open && setCancelTarget(null)}
        title="Cancel this booking?"
        description={
          cancelTarget
            ? `${cancelTarget.contact_name ?? "Guest"} · ${formatDateTime(cancelTarget.starts_at)}. The guest will be notified and any deposit hold released.`
            : undefined
        }
        confirmLabel="Cancel booking"
        destructive
        busy={busy}
        onConfirm={cancel}
      />
    </div>
  );
}

function RescheduleDialog({
  booking,
  busy,
  onClose,
  onConfirm,
}: {
  booking: Booking | null;
  busy: boolean;
  onClose: () => void;
  onConfirm: (startsAt: string) => void;
}) {
  const [date, setDate] = React.useState<string>(toISODate(new Date()));
  const [time, setTime] = React.useState<string>("09:00");

  React.useEffect(() => {
    if (booking) {
      const d = new Date(booking.starts_at);
      setDate(toISODate(d));
      const hh = d.getHours().toString().padStart(2, "0");
      const mm = d.getMinutes().toString().padStart(2, "0");
      setTime(`${hh}:${mm}`);
    }
  }, [booking]);

  const submit = () => {
    const [h, m] = time.split(":").map(Number);
    const d = new Date(`${date}T00:00:00`);
    d.setHours(h, m, 0, 0);
    onConfirm(d.toISOString());
  };

  return (
    <Dialog open={booking !== null} onOpenChange={(open) => !open && onClose()}>
      <DialogContent onClose={onClose}>
        <DialogHeader>
          <DialogTitle>Reschedule booking</DialogTitle>
        </DialogHeader>
        {booking ? (
          <p className="mb-4 text-sm text-muted-foreground">
            Currently {formatDateTime(booking.starts_at)} ·{" "}
            {booking.contact_name ?? "Guest"}
          </p>
        ) : null}
        <div className="flex flex-wrap items-start gap-6">
          <CalendarLite
            selected={date}
            onSelect={setDate}
            minDate={toISODate(new Date())}
          />
          <div className="space-y-2">
            <Label htmlFor="reschedule-time">New start time</Label>
            <input
              id="reschedule-time"
              type="time"
              value={time}
              onChange={(e) => setTime(e.target.value)}
              className="flex h-9 rounded-md border border-input bg-card px-3 py-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            />
            <p className="text-xs text-muted-foreground">
              {date} at{" "}
              {minutesToTime(
                Number(time.split(":")[0]) * 60 + Number(time.split(":")[1]),
              )}
            </p>
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Back
          </Button>
          <Button onClick={submit} disabled={busy}>
            {busy ? "Saving…" : "Confirm new time"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
