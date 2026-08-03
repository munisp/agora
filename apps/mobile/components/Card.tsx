/**
 * Card — flat warm surface (no shadows/gradients, matching the admin-web
 * "reception desk" feel) with an optional title/description header.
 */
import React from "react";
import { View, Text, StyleSheet } from "react-native";
import { colors, radius, spacing } from "../src/theme";

export function Card({
  title,
  description,
  children,
}: {
  title?: string;
  description?: string;
  children: React.ReactNode;
}) {
  return (
    <View style={styles.card}>
      {title ? (
        <View style={styles.header}>
          <Text style={styles.title}>{title}</Text>
          {description ? <Text style={styles.description}>{description}</Text> : null}
        </View>
      ) : null}
      <View style={styles.content}>{children}</View>
    </View>
  );
}

const styles = StyleSheet.create({
  card: {
    backgroundColor: colors.card,
    borderWidth: 1,
    borderColor: colors.border,
    borderRadius: radius.lg,
    marginBottom: spacing.md,
  },
  header: {
    paddingHorizontal: spacing.lg,
    paddingTop: spacing.md,
    paddingBottom: spacing.sm,
  },
  title: {
    fontSize: 15,
    fontWeight: "600",
    color: colors.cardForeground,
  },
  description: {
    marginTop: 2,
    fontSize: 12,
    color: colors.mutedForeground,
  },
  content: {
    paddingHorizontal: spacing.lg,
    paddingVertical: spacing.md,
  },
});
