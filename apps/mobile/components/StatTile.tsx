/**
 * StatTile — big-number tile for the Today dashboard (count + label +
 * optional hint). Warm muted surface, umber number.
 */
import React from "react";
import { View, Text, StyleSheet } from "react-native";
import { colors, radius, spacing } from "../src/theme";

export function StatTile({
  value,
  label,
  hint,
}: {
  value: string | number;
  label: string;
  hint?: string;
}) {
  return (
    <View style={styles.tile}>
      <Text style={styles.value}>{value}</Text>
      <Text style={styles.label}>{label}</Text>
      {hint ? <Text style={styles.hint}>{hint}</Text> : null}
    </View>
  );
}

/** Horizontal wrap for a row of tiles (flex gap substitute). */
export function StatTileRow({ children }: { children: React.ReactNode }) {
  return <View style={styles.row}>{children}</View>;
}

const styles = StyleSheet.create({
  row: {
    flexDirection: "row",
    marginBottom: spacing.md,
  },
  tile: {
    flex: 1,
    backgroundColor: colors.muted,
    borderRadius: radius.lg,
    paddingVertical: spacing.md,
    paddingHorizontal: spacing.md,
    marginHorizontal: spacing.xs,
  },
  value: {
    fontSize: 24,
    fontWeight: "700",
    color: colors.primary,
  },
  label: {
    marginTop: 2,
    fontSize: 12,
    fontWeight: "600",
    color: colors.secondaryForeground,
  },
  hint: {
    marginTop: 2,
    fontSize: 11,
    color: colors.mutedForeground,
  },
});
