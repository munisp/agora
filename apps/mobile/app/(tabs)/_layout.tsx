/**
 * Tab shell: Today / Leads / Growth / Incidents (SPEC-W16 §5). Text tabs
 * keep the app icon-free (no extra dependency); the active tab gets the
 * umber primary tint.
 */
import React from "react";
import { Text } from "react-native";
import { Tabs } from "expo-router";
import { colors } from "../../src/theme";

function tabIcon(label: string) {
  return function TabIcon({ focused }: { focused: boolean }) {
    return (
      <Text
        style={{
          fontSize: 16,
          fontWeight: "700",
          color: focused ? colors.primary : colors.mutedForeground,
        }}
      >
        {label}
      </Text>
    );
  };
}

export default function TabsLayout() {
  return (
    <Tabs
      screenOptions={{
        headerShown: false,
        tabBarActiveTintColor: colors.primary,
        tabBarInactiveTintColor: colors.mutedForeground,
        tabBarStyle: {
          backgroundColor: colors.card,
          borderTopColor: colors.border,
        },
      }}
    >
      <Tabs.Screen
        name="today"
        options={{ title: "Today", tabBarIcon: tabIcon("◷") }}
      />
      <Tabs.Screen
        name="leads"
        options={{ title: "Leads", tabBarIcon: tabIcon("✦") }}
      />
      <Tabs.Screen
        name="growth"
        options={{ title: "Growth", tabBarIcon: tabIcon("↗") }}
      />
      <Tabs.Screen
        name="incidents"
        options={{ title: "Incidents", tabBarIcon: tabIcon("⚑") }}
      />
    </Tabs>
  );
}
