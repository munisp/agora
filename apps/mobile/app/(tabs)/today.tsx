/**
 * Today — the field dashboard: my bookings for today plus the next
 * upcoming ones (GET /v1/bookings?mine=true — the server resolves the
 * caller's team member from the JWT email claim).
 *
 * SPEC-W46 MB-5: both lists are CAPPED and rendered through React.memo
 * row components via stable useCallback renderers (bookings state changes
 * no longer re-render every row); derived lists are useMemo'd so
 * pull-to-refresh spinner updates don't re-filter the day.
 *
 * SPEC-W47 UIMOB-11: rows expand into a booking detail with the field ops
 * actions that already exist server-side — complete (POST
 * /v1/bookings/{id}/complete), cancel (…/cancel), reschedule
 * (…/reschedule) — plus an on-site check-in that feeds the W46 offline
 * capture queue (kind "checkin" → POST /v1/field/capture, exactly-once on
 * client_id). Honesty rules: a queued check-in is reported as QUEUED
 * (never as synced); a failed mutation surfaces the server's error
 * verbatim; an offline device is told the action was NOT sent.
 */
import React from "react";
import { ScrollView, RefreshControl, Text, StyleSheet, View, Alert } from "react-native";
import { useFocusEffect } from "expo-router";
import {
  listBookings,
  completeBooking,
  cancelBooking,
  rescheduleBooking,
  ApiError,
  NotAuthenticatedError,
  TimeoutError,
} from "../../src/api/client";
import type { Booking } from "../../src/api/types";
import { enqueueCapture, flushQueue, QueueFullError } from "../../src/field/queue";
import { useSession } from "../../src/auth/useSession";
import { Screen } from "../../components/Screen";
import { Card } from "../../components/Card";
import { ListItem } from "../../components/ListItem";
import { StatTile, StatTileRow } from "../../components/StatTile";
import { Badge, Button, EmptyState, ErrorBox, Field } from "../../components/ui";
import { colors, spacing } from "../../src/theme";

/** MB-5: bound the rendered rows — a day with hundreds of bookings used
 * to mount one native view tree per booking inside the ScrollView. */
const MAX_TODAY_ROWS = 25;
const MAX_UPCOMING_ROWS = 10;

function isSameDay(a: Date, b: Date): boolean {
  return (
    a.getFullYear() === b.getFullYear() &&
    a.getMonth() === b.getMonth() &&
    a.getDate() === b.getDate()
  );
}

function fmtTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleTimeString("en-NG", { hour: "2-digit", minute: "2-digit" });
}

function statusTone(status: string): "success" | "warning" | "info" | "secondary" | "destructive" {
  switch (status) {
    case "confirmed":
    case "completed":
      return "success";
    case "pending":
      return "warning";
    case "cancelled":
    case "no_show":
      return "destructive";
    default:
      return "secondary";
  }
}

/** Parse "YYYY-MM-DD HH:mm" (local device time) or any Date-parseable
 * string. Returns null when unparseable — the UI shows an honest error
 * instead of sending garbage to the server. */
function parseLocalDateTime(input: string): Date | null {
  const trimmed = input.trim();
  const m = trimmed.match(/^(\d{4})-(\d{2})-(\d{2})[T ](\d{2}):(\d{2})(?::(\d{2}))?\s*$/);
  if (m && m[1] && m[2] && m[3] && m[4] && m[5]) {
    const d = new Date(
      Number(m[1]),
      Number(m[2]) - 1,
      Number(m[3]),
      Number(m[4]),
      Number(m[5]),
      Number(m[6] ?? 0),
    );
    return Number.isNaN(d.getTime()) ? null : d;
  }
  const d = new Date(trimmed);
  return Number.isNaN(d.getTime()) ? null : d;
}

/** UIMOB-11: honest error mapping for booking mutations. A request that
 * never reached the server (timeout / dead network — RN fetch rejects with
 * TypeError) is reported as NOT SENT, never as a maybe-success. */
function bookingActionError(e: unknown): string {
  if (e instanceof TimeoutError || e instanceof TypeError) {
    return "Offline or unreachable — the action was NOT sent and nothing changed on the server. Reconnect and retry.";
  }
  if (e instanceof ApiError) {
    if (e.status === 401 || e.status === 403) {
      return "Not permitted — your role needs manage_bookings on this tenant.";
    }
    if (e.status === 404) {
      return "Booking not found — it may have been removed. Pull to refresh.";
    }
    if (e.status === 503) {
      return "Payments rail not configured on the server — the action was refused. Contact an admin.";
    }
    // 400/409/422 and everything else: surface the server's own reason.
    return e.message;
  }
  return e instanceof Error ? e.message : String(e);
}

/** MB-5: memo'd row — re-renders only when its Booking object identity or
 * its own expanded flag changes (bookings are replaced wholesale on load,
 * and onToggle is a stable useCallback, so unchanged rows keep their
 * previous element across focus/refresh/action-state updates). */
const BookingRow = React.memo(function BookingRow({
  booking,
  datePrefix,
  expanded,
  onToggle,
}: {
  booking: Booking;
  datePrefix: boolean;
  expanded: boolean;
  onToggle: (id: string) => void;
}) {
  return (
    <ListItem
      title={
        datePrefix
          ? `${new Date(booking.starts_at).toLocaleString("en-NG", {
              weekday: "short",
              month: "short",
              day: "numeric",
              hour: "2-digit",
              minute: "2-digit",
            })}`
          : `${fmtTime(booking.starts_at)} – ${fmtTime(booking.ends_at)}`
      }
      subtitle={`Booking ${booking.id.slice(0, 8)}… · source ${booking.source}`}
      right={<Badge label={booking.status} tone={statusTone(booking.status)} />}
      onPress={() => onToggle(booking.id)}
    />
  );
});

export default function TodayScreen() {
  const { session, signOut } = useSession();
  const [bookings, setBookings] = React.useState<Booking[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [refreshing, setRefreshing] = React.useState(false);
  // UIMOB-11: single expanded booking + its action state. Only one booking
  // is acted on at a time, so busy/error/message are not keyed by id.
  const [expandedId, setExpandedId] = React.useState<string | null>(null);
  const [actionBusy, setActionBusy] = React.useState<string | null>(null);
  const [actionError, setActionError] = React.useState<string | null>(null);
  const [actionMsg, setActionMsg] = React.useState<string | null>(null);
  const [rescheduleOpen, setRescheduleOpen] = React.useState(false);
  const [rescheduleText, setRescheduleText] = React.useState("");

  const load = React.useCallback(async () => {
    try {
      setError(null);
      const rows = await listBookings({ mine: true });
      rows.sort((a, b) => a.starts_at.localeCompare(b.starts_at));
      setBookings(rows);
    } catch (e) {
      if (!(e instanceof NotAuthenticatedError)) {
        setError(e instanceof Error ? e.message : String(e));
      }
    }
  }, []);

  useFocusEffect(
    React.useCallback(() => {
      void load();
    }, [load]),
  );

  const onRefresh = React.useCallback(async () => {
    setRefreshing(true);
    await load();
    setRefreshing(false);
  }, [load]);

  // MB-5: stable toggle — row memoization survives expand/collapse.
  const onToggle = React.useCallback((id: string) => {
    setExpandedId((cur) => (cur === id ? null : id));
    setActionError(null);
    setActionMsg(null);
    setRescheduleOpen(false);
    setRescheduleText("");
  }, []);

  /** Replace one booking in place after a successful mutation (the ops
   * routes return the updated row) — no full reload needed. */
  const applyUpdated = React.useCallback((updated: Booking) => {
    setBookings((rows) => rows.map((b) => (b.id === updated.id ? updated : b)));
  }, []);

  const runAction = React.useCallback(
    async (kind: string, fn: () => Promise<void>) => {
      setActionBusy(kind);
      setActionError(null);
      setActionMsg(null);
      try {
        await fn();
      } catch (e) {
        setActionError(bookingActionError(e));
      } finally {
        setActionBusy(null);
      }
    },
    [],
  );

  const onComplete = React.useCallback(
    (b: Booking) =>
      runAction("complete", async () => {
        const res = await completeBooking(b.id);
        applyUpdated(res.booking);
        setActionMsg(
          res.already_completed
            ? "Already completed on the server (idempotent replay) — no duplicate action taken."
            : "Booking marked completed on the server.",
        );
      }),
    [runAction, applyUpdated],
  );

  const doCancel = React.useCallback(
    (b: Booking) =>
      runAction("cancel", async () => {
        const updated = await cancelBooking(b.id, "field_staff_cancel");
        applyUpdated(updated);
        setActionMsg("Booking cancelled on the server.");
      }),
    [runAction, applyUpdated],
  );

  const onCancel = React.useCallback(
    (b: Booking) => {
      Alert.alert("Cancel this booking?", "This marks the booking cancelled on the server.", [
        { text: "Keep booking", style: "cancel" },
        { text: "Cancel booking", style: "destructive", onPress: () => void doCancel(b) },
      ]);
    },
    [doCancel],
  );

  const onRescheduleSubmit = React.useCallback(
    (b: Booking) =>
      runAction("reschedule", async () => {
        const parsed = parseLocalDateTime(rescheduleText);
        if (!parsed) {
          setActionError("Enter the new start as YYYY-MM-DD HH:mm (device local time).");
          return;
        }
        if (parsed.getTime() <= Date.now()) {
          setActionError("The new start must be in the future.");
          return;
        }
        const updated = await rescheduleBooking(b.id, parsed.toISOString());
        applyUpdated(updated);
        setRescheduleOpen(false);
        setRescheduleText("");
        setActionMsg(`Rescheduled on the server to ${updated.starts_at}.`);
      }),
    [runAction, applyUpdated, rescheduleText],
  );

  /** UIMOB-11: on-site check-in rides the W46 offline capture queue
   * (exactly-once on client_id, survives dead networks). The queue kind is
   * "checkin"; the payload carries the booking's contact (the server's
   * CheckinPayload: contact_id + note). No GPS fix is attached on this
   * build (no location module in the dependency set) — the check-in is an
   * honest timestamped record, never a fabricated location. The result is
   * reported as QUEUED unless the immediate best-effort flush actually
   * delivered it. */
  const onCheckIn = React.useCallback(
    (b: Booking) =>
      runAction("checkin", async () => {
        try {
          await enqueueCapture("checkin", {
            contact_id: b.contact_id,
            note: `booking ${b.id} on-site check-in`,
          });
        } catch (e) {
          setActionError(e instanceof QueueFullError ? e.message : String(e));
          return;
        }
        const report = await flushQueue().catch(() => null);
        if (report && report.synced && report.delivered > 0) {
          setActionMsg("Checked in — the check-in record is on the server.");
        } else if (report) {
          setActionMsg(
            `Check-in queued on this device — ${report.remaining} capture${report.remaining === 1 ? "" : "s"} still waiting to sync (not on the server yet).`,
          );
        } else {
          setActionMsg(
            "Check-in queued on this device (sync unreachable) — it will sync automatically when you're back online.",
          );
        }
      }),
    [runAction],
  );

  // MB-5: memoized derivations — recomputed only when bookings change,
  // not on every refreshing/error state flip.
  const { todays, upcoming, completedToday } = React.useMemo(() => {
    const now = new Date();
    const todayRows = bookings.filter((b) => isSameDay(new Date(b.starts_at), now));
    const upcomingRows = bookings.filter(
      (b) =>
        new Date(b.starts_at).getTime() > now.getTime() &&
        !isSameDay(new Date(b.starts_at), now),
    );
    return {
      todays: todayRows,
      upcoming: upcomingRows,
      completedToday: todayRows.filter((b) => b.status === "completed").length,
    };
  }, [bookings]);

  const now = new Date();

  /** UIMOB-11: detail + ops panel under an expanded row. Actionability
   * mirrors the server transitions (complete.go: confirmed|checked_in →
   * completed; ops.go: cancelled bookings can't reschedule) so the UI
   * doesn't offer actions the server would 409. */
  const renderActions = (b: Booking) => {
    const closed = b.status === "cancelled" || b.status === "completed";
    const completable = b.status === "confirmed" || b.status === "checked_in";
    return (
      <View style={styles.detail}>
        <Text style={styles.detailMeta}>
          id {b.id}{"\n"}contact {b.contact_id} · offering {b.offering_id.slice(0, 8)}…
        </Text>
        {actionError ? <ErrorBox message={actionError} /> : null}
        {actionMsg ? <Text style={styles.okText}>{actionMsg}</Text> : null}
        {!closed ? (
          <>
            <Button
              title="Check in (on-site)"
              variant="secondary"
              onPress={() => void onCheckIn(b)}
              loading={actionBusy === "checkin"}
              disabled={actionBusy !== null}
            />
            <View style={styles.actionGap} />
          </>
        ) : null}
        {completable ? (
          <>
            <Button
              title="Mark completed"
              onPress={() => void onComplete(b)}
              loading={actionBusy === "complete"}
              disabled={actionBusy !== null}
            />
            <View style={styles.actionGap} />
          </>
        ) : null}
        {!closed && rescheduleOpen ? (
          <>
            <Field
              label="New start (YYYY-MM-DD HH:mm, device local time)"
              value={rescheduleText}
              onChangeText={setRescheduleText}
              placeholder="2025-06-30 14:30"
            />
            <Button
              title="Confirm reschedule"
              onPress={() => void onRescheduleSubmit(b)}
              loading={actionBusy === "reschedule"}
              disabled={actionBusy !== null}
            />
            <View style={styles.actionGap} />
          </>
        ) : null}
        {!closed && !rescheduleOpen ? (
          <>
            <Button
              title="Reschedule…"
              variant="secondary"
              onPress={() => setRescheduleOpen(true)}
              disabled={actionBusy !== null}
            />
            <View style={styles.actionGap} />
          </>
        ) : null}
        {!closed ? (
          <Button
            title="Cancel booking"
            variant="destructive"
            onPress={() => onCancel(b)}
            loading={actionBusy === "cancel"}
            disabled={actionBusy !== null}
          />
        ) : null}
        {closed ? (
          <Text style={styles.closedNote}>
            This booking is {b.status} — no further field actions apply.
          </Text>
        ) : null}
      </View>
    );
  };

  // MB-5: stable renderers — row elements are only recreated when the
  // underlying slice changes, and React.memo rows skip re-render.
  const renderTodayRow = React.useCallback(
    (b: Booking) => (
      <View key={b.id}>
        <BookingRow booking={b} datePrefix={false} expanded={expandedId === b.id} onToggle={onToggle} />
        {expandedId === b.id ? renderActions(b) : null}
      </View>
    ),
    // renderActions closes over the action state; memo'd BookingRows still
    // skip re-render for unaffected bookings.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [expandedId, onToggle, actionBusy, actionError, actionMsg, rescheduleOpen, rescheduleText],
  );
  const renderUpcomingRow = React.useCallback(
    (b: Booking) => (
      <View key={b.id}>
        <BookingRow booking={b} datePrefix expanded={expandedId === b.id} onToggle={onToggle} />
        {expandedId === b.id ? renderActions(b) : null}
      </View>
    ),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [expandedId, onToggle, actionBusy, actionError, actionMsg, rescheduleOpen, rescheduleText],
  );

  const visibleTodays = React.useMemo(
    () => todays.slice(0, MAX_TODAY_ROWS),
    [todays],
  );
  const visibleUpcoming = React.useMemo(
    () => upcoming.slice(0, MAX_UPCOMING_ROWS),
    [upcoming],
  );

  return (
    <Screen
      title="Today"
      subtitle={
        session?.email
          ? `${session.email} · ${session.tenantSlug}`
          : session?.tenantSlug
      }
      right={<Button title="Sign out" variant="secondary" onPress={() => void signOut()} />}
    >
      <ScrollView
        refreshControl={
          <RefreshControl refreshing={refreshing} onRefresh={onRefresh} tintColor={colors.primary} />
        }
      >
        {error ? <ErrorBox message={error} /> : null}

        <StatTileRow>
          <StatTile value={todays.length} label="Today" />
          <StatTile value={completedToday} label="Completed" />
          <StatTile value={upcoming.length} label="Upcoming" />
        </StatTileRow>

        <Card title="Today's bookings" description={now.toDateString()}>
          {todays.length === 0 ? (
            <EmptyState title="Nothing scheduled today" hint="Pull to refresh." />
          ) : (
            <>
              {visibleTodays.map(renderTodayRow)}
              {todays.length > MAX_TODAY_ROWS ? (
                <Text style={styles.moreText}>
                  + {todays.length - MAX_TODAY_ROWS} more today (showing first{" "}
                  {MAX_TODAY_ROWS})
                </Text>
              ) : null}
            </>
          )}
        </Card>

        <Card title="Next up" description="Your next bookings after today">
          {upcoming.length === 0 ? (
            <EmptyState title="No upcoming bookings" />
          ) : (
            <>
              {visibleUpcoming.map(renderUpcomingRow)}
              {upcoming.length > MAX_UPCOMING_ROWS ? (
                <Text style={styles.moreText}>
                  + {upcoming.length - MAX_UPCOMING_ROWS} more upcoming
                </Text>
              ) : null}
            </>
          )}
        </Card>

        <View style={styles.footer}>
          <Text style={styles.footerText}>
            Data: GET /api/bookings/v1/bookings?mine=true
          </Text>
        </View>
        <View style={{ height: spacing.xl }} />
      </ScrollView>
    </Screen>
  );
}

const styles = StyleSheet.create({
  moreText: {
    marginTop: spacing.xs,
    fontSize: 12,
    color: colors.mutedForeground,
  },
  detail: {
    paddingHorizontal: spacing.sm,
    paddingBottom: spacing.sm,
    marginBottom: spacing.xs,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: colors.muted,
  },
  detailMeta: {
    fontSize: 11,
    color: colors.mutedForeground,
    marginBottom: spacing.sm,
  },
  okText: {
    fontSize: 13,
    color: colors.success,
    marginBottom: spacing.sm,
  },
  actionGap: { height: spacing.xs },
  closedNote: {
    fontSize: 12,
    color: colors.mutedForeground,
  },
  footer: {
    marginTop: spacing.sm,
    alignItems: "center",
  },
  footerText: {
    fontSize: 11,
    color: colors.mutedForeground,
  },
});
