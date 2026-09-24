/**
 * Session persistence: Keycloak tokens + tenant slug in expo-secure-store
 * (Keychain on iOS, EncryptedSharedPreferences on Android — never
 * AsyncStorage for credentials).
 */
import * as SecureStore from "expo-secure-store";

const KEY_ACCESS_TOKEN = "opendesk.access_token";
const KEY_REFRESH_TOKEN = "opendesk.refresh_token";
const KEY_ID_TOKEN = "opendesk.id_token";
const KEY_EXPIRES_AT = "opendesk.expires_at"; // epoch seconds
const KEY_TENANT_SLUG = "opendesk.tenant_slug";
const KEY_USER_EMAIL = "opendesk.user_email";
const KEY_DEVICE_TOKEN = "opendesk.device_token"; // expo push token we registered

/**
 * MB-3 (SPEC-W46): in-memory cache of the hot-path credentials
 * (access token, refresh token, tenant slug). Every API call used to pay
 * two SecureStore reads across the native bridge (Keychain /
 * EncryptedSharedPreferences); now the first read populates this cache
 * and subsequent calls are in-memory. The cache is INVALIDATED on every
 * mutation (saveSession / updateTokens / clearSession) and primed by
 * loadSession, so it can never serve a stale token after a write.
 * SecureStore remains the source of truth (app restart, process death).
 */
interface CredentialCache {
  accessToken: string | null;
  refreshToken: string | null;
  tenantSlug: string | null;
}
let credentialCache: CredentialCache | null = null;
let credentialRead: Promise<CredentialCache> | null = null;
/** Bumped on every invalidation so an in-flight read started BEFORE a
 * write can never repopulate the cache with pre-write values. */
let credentialGeneration = 0;

async function readCredentials(): Promise<CredentialCache> {
  if (credentialCache) return credentialCache;
  if (!credentialRead) {
    const gen = credentialGeneration;
    credentialRead = Promise.all([
      SecureStore.getItemAsync(KEY_ACCESS_TOKEN),
      SecureStore.getItemAsync(KEY_REFRESH_TOKEN),
      SecureStore.getItemAsync(KEY_TENANT_SLUG),
    ]).then(
      ([accessToken, refreshToken, tenantSlug]) => {
        credentialRead = null;
        const fresh: CredentialCache = { accessToken, refreshToken, tenantSlug };
        if (gen === credentialGeneration) credentialCache = fresh;
        return fresh;
      },
      (err) => {
        credentialRead = null;
        throw err;
      },
    );
  }
  return credentialRead;
}

function invalidateCredentialCache(): void {
  credentialGeneration++;
  credentialCache = null;
}

export interface StoredSession {
  accessToken: string;
  refreshToken: string | null;
  idToken: string | null;
  /** epoch seconds at which the access token expires; null = unknown. */
  expiresAt: number | null;
  tenantSlug: string;
  email: string | null;
}

export async function saveSession(s: {
  accessToken: string;
  refreshToken?: string | null;
  idToken?: string | null;
  expiresIn?: number | null;
  tenantSlug: string;
  email?: string | null;
}): Promise<void> {
  await SecureStore.setItemAsync(KEY_ACCESS_TOKEN, s.accessToken);
  if (s.refreshToken) await SecureStore.setItemAsync(KEY_REFRESH_TOKEN, s.refreshToken);
  if (s.idToken) await SecureStore.setItemAsync(KEY_ID_TOKEN, s.idToken);
  if (typeof s.expiresIn === "number" && s.expiresIn > 0) {
    const at = Math.floor(Date.now() / 1000) + s.expiresIn;
    await SecureStore.setItemAsync(KEY_EXPIRES_AT, String(at));
  }
  await SecureStore.setItemAsync(KEY_TENANT_SLUG, s.tenantSlug);
  if (s.email) await SecureStore.setItemAsync(KEY_USER_EMAIL, s.email);
  invalidateCredentialCache();
}

export async function loadSession(): Promise<StoredSession | null> {
  const [accessToken, refreshToken, idToken, expiresAtRaw, tenantSlug, email] =
    await Promise.all([
      SecureStore.getItemAsync(KEY_ACCESS_TOKEN),
      SecureStore.getItemAsync(KEY_REFRESH_TOKEN),
      SecureStore.getItemAsync(KEY_ID_TOKEN),
      SecureStore.getItemAsync(KEY_EXPIRES_AT),
      SecureStore.getItemAsync(KEY_TENANT_SLUG),
      SecureStore.getItemAsync(KEY_USER_EMAIL),
    ]);
  if (!accessToken || !tenantSlug) return null;
  // Prime the MB-3 credential cache from the reads we already paid for.
  credentialCache = { accessToken, refreshToken, tenantSlug };
  const expiresAt = expiresAtRaw ? Number(expiresAtRaw) : null;
  return {
    accessToken,
    refreshToken,
    idToken,
    expiresAt: expiresAt !== null && Number.isFinite(expiresAt) ? expiresAt : null,
    tenantSlug,
    email,
  };
}

export async function updateTokens(t: {
  accessToken: string;
  refreshToken?: string | null;
  expiresIn?: number | null;
}): Promise<void> {
  await SecureStore.setItemAsync(KEY_ACCESS_TOKEN, t.accessToken);
  if (t.refreshToken) await SecureStore.setItemAsync(KEY_REFRESH_TOKEN, t.refreshToken);
  if (typeof t.expiresIn === "number" && t.expiresIn > 0) {
    const at = Math.floor(Date.now() / 1000) + t.expiresIn;
    await SecureStore.setItemAsync(KEY_EXPIRES_AT, String(at));
  }
  invalidateCredentialCache();
}

export async function getAccessToken(): Promise<string | null> {
  return (await readCredentials()).accessToken;
}

export async function getTenantSlug(): Promise<string | null> {
  return (await readCredentials()).tenantSlug;
}

/** Refresh token for the single-flight 401→refresh path (MB-2). */
export async function getRefreshToken(): Promise<string | null> {
  return (await readCredentials()).refreshToken;
}

/** The push token we most recently registered with POST /v1/devices. */
export async function getRegisteredDeviceToken(): Promise<string | null> {
  return SecureStore.getItemAsync(KEY_DEVICE_TOKEN);
}

export async function setRegisteredDeviceToken(token: string | null): Promise<void> {
  if (token === null) {
    await SecureStore.deleteItemAsync(KEY_DEVICE_TOKEN);
  } else {
    await SecureStore.setItemAsync(KEY_DEVICE_TOKEN, token);
  }
}

/** Full sign-out cleanup (device-token row is deleted server-side first). */
export async function clearSession(): Promise<void> {
  await Promise.all([
    SecureStore.deleteItemAsync(KEY_ACCESS_TOKEN),
    SecureStore.deleteItemAsync(KEY_REFRESH_TOKEN),
    SecureStore.deleteItemAsync(KEY_ID_TOKEN),
    SecureStore.deleteItemAsync(KEY_EXPIRES_AT),
    SecureStore.deleteItemAsync(KEY_TENANT_SLUG),
    SecureStore.deleteItemAsync(KEY_USER_EMAIL),
    SecureStore.deleteItemAsync(KEY_DEVICE_TOKEN),
  ]);
  invalidateCredentialCache();
}
