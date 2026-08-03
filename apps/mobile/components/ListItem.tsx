/**
 * ListItem — dense row: title / subtitle / optional right accessory
 * (badge, chevron text, action). Used by every list screen.
 */
import React from "react";
import { View, Text, Pressable, StyleSheet } from "react-native";
import { colors, radius, spacing } from "../src/theme";

export function ListItem({
  title,
  subtitle,
  meta,
  right,
  onPress,
}: {
  title: string;
  subtitle?: string;
  /** Small muted line under the subtitle (ids, timestamps). */
  meta?: string;
  right?: React.ReactNode;
  onPress?: () => void;
}) {
  const inner = (
    <View style={styles.row}>
      <View style={styles.text}>
        <Text style={styles.title} numberOfLines={1}>
          {title}
        </Text>
        {subtitle ? (
          <Text style={styles.subtitle} numberOfLines={2}>
            {subtitle}
          </Text>
        ) : null}
        {meta ? (
          <Text style={styles.meta} numberOfLines={1}>
            {meta}
          </Text>
        ) : null}
      </View>
      {right ? <View style={styles.right}>{right}</View> : null}
    </View>
  );
  if (onPress) {
    return (
      <Pressable
        onPress={onPress}
        style={({ pressed }: { pressed: boolean }) => [
          styles.pressable,
          pressed ? styles.pressed : null,
        ]}
      >
        {inner}
      </Pressable>
    );
  }
  return <View style={styles.pressable}>{inner}</View>;
}

const styles = StyleSheet.create({
  pressable: {
    borderRadius: radius.md,
  },
  pressed: {
    backgroundColor: colors.muted,
  },
  row: {
    flexDirection: "row",
    alignItems: "center",
    paddingVertical: spacing.sm,
  },
  text: { flex: 1 },
  title: {
    fontSize: 14,
    fontWeight: "600",
    color: colors.foreground,
  },
  subtitle: {
    marginTop: 1,
    fontSize: 12,
    color: colors.mutedForeground,
  },
  meta: {
    marginTop: 2,
    fontSize: 11,
    color: colors.mutedForeground,
  },
  right: { marginLeft: spacing.md },
});
