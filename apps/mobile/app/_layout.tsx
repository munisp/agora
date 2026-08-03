/**
 * Root layout: session provider + auth gate + the route stack.
 * The lead-capture screen is a modal over the tabs (SPEC-W16 §5).
 */
import React from "react";
import { Stack, useRouter } from "expo-router";
import { StatusBar } from "expo-status-bar";
import { SafeAreaProvider } from "react-native-safe-area-context";
import { SessionProvider, useSession } from "../src/auth/useSession";
import { colors } from "../src/theme";

function AuthGate() {
  const { ready, session } = useSession();
  const router = useRouter();
  React.useEffect(() => {
    if (ready && !session) router.replace("/login");
  }, [ready, session, router]);
  return null;
}

export default function RootLayout() {
  return (
    <SafeAreaProvider>
      <SessionProvider>
        <StatusBar style="dark" />
        <AuthGate />
        <Stack
          screenOptions={{
            headerShown: false,
            contentStyle: { backgroundColor: colors.background },
          }}
        >
          <Stack.Screen name="index" />
          <Stack.Screen name="login" />
          <Stack.Screen name="(tabs)" />
          <Stack.Screen
            name="lead-capture"
            options={{ presentation: "modal" }}
          />
        </Stack>
      </SessionProvider>
    </SafeAreaProvider>
  );
}
