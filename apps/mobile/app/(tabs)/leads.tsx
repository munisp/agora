/**
 * Leads — tenant lead inbox (GET /v1/leads, SPEC-W13 contract) with
 * status filter chips and a "Capture lead" button that opens the
 * lead-capture modal. Field-captured leads use channel "field".
 */
import React from "react";
import { View, Text, Pressable, FlatList, RefreshControl, StyleSheet } from "react-native";
import { useFocusEffect, useRouter } from "expo-router";
import { listLeads, transitionLead, NotAuthenticatedError } from "../../src/api/client";
import type { Lead, LeadStatus } from "../../src/api/types";
import { Screen } from "../../components/Screen";
import { ListItem } from "../../components/ListItem";
import { Badge, Button, EmptyState, ErrorBox } from "../../components/ui";
import { colors, radius, spacing } from "../../src/theme";

const FILTERS: Array<LeadStatus | "all"> = [
  "all",
  "new",
  "contacted",
  "qualified",
  "converted",
  "lost",
];

function statusTone(status: string): "success" | "warning" | "info" | "secondary" | "destructive" {
  switch (status) {
    case "converted":
      return "success";
    case "qualified":
      return "info";
    case "new":
      return "warning";
    case "lost":
      return "destructive";
    default:
      return "secondary";
  }
}

export default function LeadsScreen() {
  const router = useRouter();
  const [filter, setFilter] = React.useState<LeadStatus | "all">("all");
  const [leads, setLeads] = React.useState<Lead[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [refreshing, setRefreshing] = React.useState(false);

  const load = React.useCallback(async () => {
    try {
      setError(null);
      const rows = await listLeads(filter === "all" ? {} : { status: filter });
      setLeads(rows);
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

  /** Advance a lead one step down the funnel (POST /v1/leads/{id}/status). */
  const advance = async (lead: Lead) => {
    const next: Partial<Record<string, LeadStatus>> = {
      new: "contacted",
      contacted: "qualified",
      qualified: "converted",
    };
    const target = next[lead.status];
    if (!target) return;
    try {
      await transitionLead(lead.lead_id, target);
      await load();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <Screen
      title="Leads"
      subtitle="First-touch leads across every channel"
      right={<Button title="+ Capture" onPress={() => router.push("/lead-capture")} />}
    >
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

      <FlatList
        data={leads}
        keyExtractor={(l: Lead) => l.lead_id}
        refreshControl={
          <RefreshControl refreshing={refreshing} onRefresh={onRefresh} tintColor={colors.primary} />
        }
        ListEmptyComponent={
          <EmptyState
            title="No leads"
            hint="Capture one in the field with the + Capture button."
          />
        }
        renderItem={({ item }: { item: Lead }) => (
          <View style={styles.leadCard}>
            <ListItem
              title={item.phone_e164}
              subtitle={`${item.channel_of_first_touch}${
                item.promo_code ? ` · promo ${item.promo_code}` : ""
              }`}
              meta={new Date(item.created_at).toLocaleString("en-NG")}
              right={<Badge label={item.status} tone={statusTone(item.status)} />}
            />
            {item.status !== "converted" && item.status !== "lost" ? (
              <Pressable onPress={() => void advance(item)} style={styles.advance}>
                <Text style={styles.advanceText}>
                  Mark {item.status === "new" ? "contacted" : item.status === "contacted" ? "qualified" : "converted"}
                </Text>
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
  leadCard: {
    backgroundColor: colors.card,
    borderWidth: 1,
    borderColor: colors.border,
    borderRadius: radius.lg,
    paddingHorizontal: spacing.md,
    paddingVertical: spacing.sm,
    marginBottom: spacing.sm,
  },
  advance: {
    alignSelf: "flex-start",
    paddingVertical: spacing.xs,
    paddingHorizontal: spacing.sm,
    borderRadius: radius.sm,
    backgroundColor: colors.accent,
    marginBottom: spacing.xs,
  },
  advanceText: { fontSize: 12, fontWeight: "600", color: colors.accentForeground },
});
