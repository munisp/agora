/**
 * Incidents inbox (SPEC-W11 Part B): GET /v1/incidents with status filter;
 * manage_bookings holders can re-dispatch an incident to its configured
 * webhook endpoints (POST /v1/incidents/{id}/dispatch) and see the
 * per-endpoint delivery ledger result.
 */
import React from "react";
import { View, Text, Pressable, FlatList, RefreshControl, StyleSheet } from "react-native";
import { useFocusEffect } from "expo-router";
import {
  listIncidents,
  dispatchIncident,
  ApiError,
  NotAuthenticatedError,
} from "../../src/api/client";
import type { Incident, IncidentStatus } from "../../src/api/types";
import { Screen } from "../../components/Screen";
import { ListItem } from "../../components/ListItem";
import { Badge, EmptyState, ErrorBox } from "../../components/ui";
import { colors, radius, spacing } from "../../src/theme";

const FILTERS: Array<IncidentStatus | "all"> = [
  "all",
  "new",
  "dispatched",
  "acknowledged",
  "closed",
];

function severityTone(sev: string): "success" | "warning" | "info" | "secondary" | "destructive" {
  switch (sev) {
    case "critical":
    case "high":
      return "destructive";
    case "medium":
      return "warning";
    default:
      return "info";
  }
}

function statusTone(status: string): "success" | "warning" | "info" | "secondary" | "destructive" {
  switch (status) {
    case "closed":
      return "success";
    case "acknowledged":
    case "dispatched":
      return "info";
    case "new":
      return "warning";
    default:
      return "secondary";
  }
}

export default function IncidentsScreen() {
  const [filter, setFilter] = React.useState<IncidentStatus | "all">("all");
  const [incidents, setIncidents] = React.useState<Incident[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [notice, setNotice] = React.useState<string | null>(null);
  const [refreshing, setRefreshing] = React.useState(false);

  const load = React.useCallback(async () => {
    try {
      setError(null);
      const rows = await listIncidents(filter === "all" ? {} : { status: filter });
      setIncidents(rows);
    } catch (e) {
      if (!(e instanceof NotAuthenticatedError)) {
        setError(e instanceof Error ? e.message : String(e));
      }
    }
  }, [filter]);

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

  const onDispatch = async (incident: Incident) => {
    setNotice(null);
    try {
      const deliveries = await dispatchIncident(incident.id);
      setNotice(
        `${incident.reference_number}: dispatched to ${deliveries.length} endpoint(s).`,
      );
      await load();
    } catch (e) {
      if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
        setNotice("Not permitted — dispatch needs manage_bookings on this tenant.");
      } else {
        setNotice(e instanceof Error ? e.message : String(e));
      }
    }
  };

  return (
    <Screen title="Incidents" subtitle="Alert inbox across configured sources">
      <View style={styles.chips}>
        {FILTERS.map((f) => (
          <Pressable
            key={f}
            onPress={() => setFilter(f)}
            style={[styles.chip, filter === f ? styles.chipActive : null]}
          >
            <Text style={[styles.chipText, filter === f ? styles.chipTextActive : null]}>
              {f}
            </Text>
          </Pressable>
        ))}
      </View>

      {error ? <ErrorBox message={error} /> : null}
      {notice ? (
        <View style={styles.noticeBox}>
          <Text style={styles.noticeText}>{notice}</Text>
        </View>
      ) : null}

      <FlatList
        data={incidents}
        keyExtractor={(i: Incident) => i.id}
        refreshControl={
          <RefreshControl refreshing={refreshing} onRefresh={onRefresh} tintColor={colors.primary} />
        }
        ListEmptyComponent={
          <EmptyState title="No incidents" hint="Ingested alerts will appear here." />
        }
        renderItem={({ item }: { item: Incident }) => (
          <View style={styles.incidentCard}>
            <ListItem
              title={item.reference_number || item.id.slice(0, 8)}
              subtitle={`${item.incident_type} · severity ${item.severity}`}
              meta={new Date(item.created_at).toLocaleString("en-NG")}
              right={
                <View style={styles.badges}>
                  <Badge label={item.severity} tone={severityTone(item.severity)} />
                  <View style={{ height: spacing.xs }} />
                  <Badge label={item.status} tone={statusTone(item.status)} />
                </View>
              }
            />
            {item.status === "new" ? (
              <Pressable onPress={() => void onDispatch(item)} style={styles.dispatch}>
                <Text style={styles.dispatchText}>Dispatch to endpoints</Text>
              </Pressable>
            ) : null}
          </View>
        )}
      />
    </Screen>
  );
}

const styles = StyleSheet.create({
  chips: {
    flexDirection: "row",
    flexWrap: "wrap",
    marginBottom: spacing.md,
  },
  chip: {
    borderRadius: radius.md,
    backgroundColor: colors.secondary,
    paddingHorizontal: spacing.md,
    paddingVertical: spacing.xs,
    marginRight: spacing.sm,
    marginBottom: spacing.sm,
  },
  chipActive: { backgroundColor: colors.primary },
  chipText: { fontSize: 12, fontWeight: "600", color: colors.secondaryForeground },
  chipTextActive: { color: colors.primaryForeground },
  noticeBox: {
    backgroundColor: colors.infoSoft,
    borderRadius: radius.md,
    padding: spacing.md,
    marginBottom: spacing.md,
  },
  noticeText: { fontSize: 13, color: colors.info },
  incidentCard: {
    backgroundColor: colors.card,
    borderWidth: 1,
    borderColor: colors.border,
    borderRadius: radius.lg,
    paddingHorizontal: spacing.md,
    paddingVertical: spacing.sm,
    marginBottom: spacing.sm,
  },
  badges: { alignItems: "flex-end" },
  dispatch: {
    alignSelf: "flex-start",
    paddingVertical: spacing.xs,
    paddingHorizontal: spacing.sm,
    borderRadius: radius.sm,
    backgroundColor: colors.accent,
    marginBottom: spacing.xs,
  },
  dispatchText: { fontSize: 12, fontWeight: "600", color: colors.accentForeground },
});
