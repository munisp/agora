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
      let stored = await loadSession();
      // Proactive refresh: if the access token is expired (or close to it)
      // and we hold a refresh token, refresh before first render.
      if (
        stored &&
        stored.expiresAt !== null &&
        stored.expiresAt - 60 < Math.floor(Date.now() / 1000) &&
        stored.refreshToken
      ) {
        const refreshed = await refreshAccessToken(stored.refreshToken);
        if (refreshed) {
          await updateTokens(refreshed);
          stored = await loadSession();
        } else {
          await clearSession();
          stored = null;
        }
      }
      if (!cancelled) {
        setSession(stored);
        setReady(true);
      }
    })();
    return () => {
      cancelled = true;
    };
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
