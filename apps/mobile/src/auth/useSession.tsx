/**
 * Session context: restores the stored session on launch, exposes
 * signIn/signOut to screens, and refreshes expiring access tokens through
 * Keycloak before API calls fail.
 */
import React from "react";
import {
  loadSession,
  saveSession,
  updateTokens,
  clearSession,
  type StoredSession,
} from "./session";
import {
  signInWithKeycloak,
  refreshAccessToken,
  revokeTokens,
} from "./keycloak";
import {
  registerForPushNotifications,
  unregisterPushNotifications,
} from "../push/register";
import { setSessionExpiredHandler } from "../api/client";

export interface SessionState {
  /** null while the stored session is being restored. */
  ready: boolean;
  session: StoredSession | null;
  signIn: (tenantSlug: string) => Promise<void>;
  signOut: () => Promise<void>;
}

const SessionContext = React.createContext<SessionState>({
  ready: false,
  session: null,
  signIn: async () => {},
  signOut: async () => {},
});

export function SessionProvider({ children }: { children: React.ReactNode }) {
  const [ready, setReady] = React.useState(false);
  const [session, setSession] = React.useState<StoredSession | null>(null);

  React.useEffect(() => {
    let cancelled = false;
    (async () => {
      // MB-4 (SPEC-W46): restore from SecureStore and unblock first paint
      // IMMEDIATELY — never await a network round-trip here. A cold start
      // with a near-expiry token used to stall on a Keycloak refresh RTT
      // before any screen could render.
      const stored = await loadSession();
      if (cancelled) return;
      setSession(stored);
      setReady(true);

      // Proactive refresh continues in the BACKGROUND. If the access token
      // is expired (or close to it) and we hold a refresh token, refresh
      // now so the first API calls don't pay the 401→refresh dance; any
      // calls that do race ahead are covered by the client's single-flight
      // 401→refresh→retry (MB-2). Auth semantics unchanged: a dead refresh
      // grant still clears the session (→ /login via AuthGate).
      if (
        stored &&
        stored.expiresAt !== null &&
        stored.expiresAt - 60 < Math.floor(Date.now() / 1000) &&
        stored.refreshToken
      ) {
        const refreshed = await refreshAccessToken(stored.refreshToken);
        if (cancelled) return;
        if (refreshed) {
          await updateTokens(refreshed);
          const next = await loadSession();
          if (!cancelled) setSession(next);
        } else {
          await clearSession();
          if (!cancelled) setSession(null);
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  // MB-2: when the API client's single-flight refresh fails (dead grant),
  // it clears the stored session and invokes this hook so React state
  // drops too — the root AuthGate then routes to /login (full logout).
  React.useEffect(() => {
    setSessionExpiredHandler(() => setSession(null));
    return () => setSessionExpiredHandler(null);
  }, []);

  const signIn = React.useCallback(async (tenantSlug: string) => {
    const result = await signInWithKeycloak();
    await saveSession({
      accessToken: result.accessToken,
      refreshToken: result.refreshToken,
      idToken: result.idToken,
      expiresIn: result.expiresIn,
      tenantSlug: tenantSlug.trim(),
      email: result.email,
    });
    const stored = await loadSession();
    setSession(stored);
    // Push registration is best-effort and must not block sign-in
    // (SPEC-W16 §1: POST /v1/devices after permission grant).
    void registerForPushNotifications();
  }, []);

  const signOut = React.useCallback(async () => {
    const stored = await loadSession();
    await unregisterPushNotifications();
    await revokeTokens([stored?.accessToken ?? null, stored?.refreshToken ?? null]);
    await clearSession();
    setSession(null);
  }, []);

  const value = React.useMemo(
    () => ({ ready, session, signIn, signOut }),
    [ready, session, signIn, signOut],
  );

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionState {
  return React.useContext(SessionContext);
}
