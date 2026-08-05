/**
 * Entry route: bounce to the dashboard or login depending on the restored
 * session. Renders a warm splash while SecureStore is read.
 */
import React from "react";
import { View, Text, ActivityIndicator, StyleSheet } from "react-native";
import { Redirect } from "expo-router";
import { useSession } from "../src/auth/useSession";
import { colors } from "../src/theme";

export default function Index() {
  const { ready, session } = useSession();
  if (!ready) {
    return (
      <View style={styles.root}>
        <Text style={styles.logo}>Agora</Text>
        <ActivityIndicator size="large" color={colors.primary} />
      </View>
    );
  }
  return <Redirect href={session ? "/(tabs)/today" : "/login"} />;
}

const styles = StyleSheet.create({
  root: {
    flex: 1,
    alignItems: "center",
    justifyContent: "center",
    backgroundColor: colors.background,
  },
  logo: {
    fontSize: 28,
    fontWeight: "700",
    color: colors.primary,
    marginBottom: 16,
  },
});
