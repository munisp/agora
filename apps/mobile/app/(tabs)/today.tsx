/**
 * Today — the field dashboard: my bookings for today plus the next
 * upcoming ones (GET /v1/bookings?mine=true — the server resolves the
 * caller's team member from the JWT email claim).
 *
 * SPEC-W46 MB-5: both lists are CAPPED and rendered through React.memo
 * row components via stable useCallback renderers (bookings state changes
 * no longer re-render every row); derived lists are useMemo'd so
 * pull-to-refresh spinner updates don't re-filter the day.
 */
import React from "react";
import { ScrollView, RefreshControl, Text, StyleSheet, View } from "react-native";
import { useFocusEffect } from "expo-router";
import { listBookings, NotAuthenticatedError } from "../../src/api/client";
import type { Booking } from "../../src/api/types";
import { useSession } from "../../src/auth/useSession";
import { Screen } from "../../components/Screen";
import { Card } from "../../components/Card";
import { ListItem } from "../../components/ListItem";
import { StatTile, StatTileRow } from "../../components/StatTile";
import { Badge, Button, EmptyState, ErrorBox } from "../../components/ui";
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

/** MB-5: memo'd row — re-renders only when its Booking object identity
 * changes (bookings are replaced wholesale on load, so unchanged rows
 * keep their previous element across focus/refresh spinner updates). */
const TodayBookingRow = React.memo(function TodayBookingRow({ booking }: { booking: Booking }) {
  return (
    <ListItem
      title={`${fmtTime(booking.starts_at)} – ${fmtTime(booking.ends_at)}`}
      subtitle={`Booking ${booking.id.slice(0, 8)}… · source ${booking.source}`}
      right={<Badge label={booking.status} tone={statusTone(booking.status)} />}
    />
  );
});

const UpcomingBookingRow = React.memo(function UpcomingBookingRow({
  booking,
}: {
  booking: Booking;
}) {
  return (
    <ListItem
      title={new Date(booking.starts_at).toLocaleString("en-NG", {
        weekday: "short",
        month: "short",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
      })}
      subtitle={`Booking ${booking.id.slice(0, 8)}…`}
      right={<Badge label={booking.status} tone={statusTone(booking.status)} />}
    />
  );
});

export default function TodayScreen() {
  const { session, signOut } = useSession();
  const [bookings, setBookings] = React.useState<Booking[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [refreshing, setRefreshing] = React.useState(false);

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

  // MB-5: stable renderers — row elements are only recreated when the
  // underlying slice changes, and React.memo rows skip re-render.
  const renderTodayRow = React.useCallback(
    (b: Booking) => <TodayBookingRow key={b.id} booking={b} />,
    [],
  );
  const renderUpcomingRow = React.useCallback(
    (b: Booking) => <UpcomingBookingRow key={b.id} booking={b} />,
    [],
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
  footer: {
    marginTop: spacing.sm,
    alignItems: "center",
  },
  footerText: {
    fontSize: 11,
    color: colors.mutedForeground,
  },
});
