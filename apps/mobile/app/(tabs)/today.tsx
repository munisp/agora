/**
 * Today — the field dashboard: my bookings for today plus the next
 * upcoming ones (GET /v1/bookings?mine=true — the server resolves the
 * caller's team member from the JWT email claim).
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

  const onRefresh = async () => {
    setRefreshing(true);
    await load();
    setRefreshing(false);
  };

  const now = new Date();
  const todays = bookings.filter((b) => isSameDay(new Date(b.starts_at), now));
  const upcoming = bookings.filter(
    (b) => new Date(b.starts_at).getTime() > now.getTime() && !isSameDay(new Date(b.starts_at), now),
  );
  const completedToday = todays.filter((b) => b.status === "completed").length;

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
            todays.map((b) => (
              <ListItem
                key={b.id}
                title={`${fmtTime(b.starts_at)} – ${fmtTime(b.ends_at)}`}
                subtitle={`Booking ${b.id.slice(0, 8)}… · source ${b.source}`}
                right={<Badge label={b.status} tone={statusTone(b.status)} />}
              />
            ))
          )}
        </Card>

        <Card title="Next up" description="Your next bookings after today">
          {upcoming.length === 0 ? (
            <EmptyState title="No upcoming bookings" />
          ) : (
            upcoming.slice(0, 10).map((b) => (
              <ListItem
                key={b.id}
                title={new Date(b.starts_at).toLocaleString("en-NG", {
                  weekday: "short",
                  month: "short",
                  day: "numeric",
                  hour: "2-digit",
                  minute: "2-digit",
                })}
                subtitle={`Booking ${b.id.slice(0, 8)}…`}
                right={<Badge label={b.status} tone={statusTone(b.status)} />}
              />
            ))
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
  footer: { alignItems: "center", marginTop: spacing.sm },
  footerText: { fontSize: 11, color: colors.mutedForeground },
});
