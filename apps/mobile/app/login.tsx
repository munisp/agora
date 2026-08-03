/**
 * Login: tenant slug + Keycloak PKCE sign-in (expo-auth-session).
 *
 * The tenant slug is sent as X-Tenant-Slug on every BFF call and validated
 * server-side against the JWT by booking-service's tenantMiddleware — the
 * app never trusts it locally beyond scoping requests.
 */
import React from "react";
import { View, Text, StyleSheet, KeyboardAvoidingView, Platform } from "react-native";
import { useRouter } from "expo-router";
import { useSession } from "../src/auth/useSession";
import { Button, Field, ErrorBox } from "../components/ui";
import { colors, radius, spacing } from "../src/theme";

export default function LoginScreen() {
  const { signIn } = useSession();
  const router = useRouter();
  const [tenantSlug, setTenantSlug] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);

  const onSignIn = async () => {
    if (!tenantSlug.trim()) {
      setError("Enter your tenant slug (the subdomain of your OpenDesk site).");
      return;
    }
    setError(null);
    setBusy(true);
    try {
      await signIn(tenantSlug);
      router.replace("/(tabs)/today");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <KeyboardAvoidingView
      style={styles.root}
      behavior={Platform.OS === "ios" ? "padding" : undefined}
    >
      <View style={styles.card}>
        <Text style={styles.logo}>OpenDesk</Text>
        <Text style={styles.tagline}>Field app — leads, referrals, incidents</Text>
        {error ? <ErrorBox message={error} /> : null}
        <Field
          label="Tenant slug"
          value={tenantSlug}
          onChangeText={setTenantSlug}
          placeholder="e.g. glowhaven"
          autoCapitalize="none"
        />
        <Button title="Sign in with Keycloak" onPress={onSignIn} loading={busy} />
        <Text style={styles.hint}>
          Opens your organisation's Keycloak login in the system browser
          (authorization code + PKCE — no password is stored on this device).
        </Text>
      </View>
    </KeyboardAvoidingView>
  );
}

const styles = StyleSheet.create({
  root: {
    flex: 1,
    backgroundColor: colors.background,
    justifyContent: "center",
    padding: spacing.lg,
  },
  card: {
    backgroundColor: colors.card,
    borderWidth: 1,
    borderColor: colors.border,
    borderRadius: radius.xl,
    padding: spacing.xl,
  },
  logo: {
    fontSize: 28,
    fontWeight: "700",
    color: colors.primary,
  },
  tagline: {
    fontSize: 13,
    color: colors.mutedForeground,
    marginTop: spacing.xs,
    marginBottom: spacing.lg,
  },
  hint: {
    marginTop: spacing.md,
    fontSize: 12,
    color: colors.mutedForeground,
  },
});
