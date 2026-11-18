/**
 * Last-good localStorage cache for offline-tolerant CRM reads (SPEC-W82
 * B1.3). This is a per-browser layer only — the service worker
 * (public/sw.js) is untouched and still never caches credentialed API
 * responses (SPEC-W46 AW-5).
 *
 * The serialize/deserialize/staleness helpers are PURE (no storage, no
 * clock of their own) so they can be unit-tested; the read/write/clear
 * wrappers guard every storage access (privacy mode, quota, SSR).
 */

/** Every key this layer writes carries this prefix (cleared on sign-out). */
export const CACHE_PREFIX = "od-cache:";

export interface CacheEnvelope<T> {
  /** epoch ms when the data was last fetched successfully */
  savedAt: number;
  data: T;
}

/** Serialize a payload with its fetch timestamp. Pure. */
export function serializeCache<T>(data: T, savedAt: number): string {
  return JSON.stringify({ savedAt, data } satisfies CacheEnvelope<T>);
}

/**
 * Parse a cached envelope. Returns null for missing/corrupt payloads or
 * wrong shapes — callers treat null as "no cache".
 */
export function deserializeCache<T>(raw: string | null): CacheEnvelope<T> | null {
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as Partial<CacheEnvelope<T>>;
    if (
      typeof parsed !== "object" ||
      parsed === null ||
      typeof parsed.savedAt !== "number" ||
      !Number.isFinite(parsed.savedAt) ||
      !("data" in parsed)
    ) {
      return null;
    }
    return { savedAt: parsed.savedAt, data: parsed.data as T };
  } catch {
    return null;
  }
}

/** Staleness check. Pure: both timestamps are passed in. */
export function isStale(savedAt: number, now: number, maxAgeMs: number): boolean {
  return now - savedAt > maxAgeMs;
}

/** "Cached as of HH:MM" label time (24-hour, local). Pure. */
export function formatCachedAt(savedAt: number): string {
  const d = new Date(savedAt);
  const hh = d.getHours().toString().padStart(2, "0");
  const mm = d.getMinutes().toString().padStart(2, "0");
  return `${hh}:${mm}`;
}

/**
 * Cache key shape: tenant slug + scope + id, so two tenants on one browser
 * never see each other's data (mirrors x-tenant-slug scoping).
 */
export function cacheKey(tenant: string, scope: string, id: string): string {
  return `${CACHE_PREFIX}${tenant}:${scope}:${id}`;
}

function storage(): Storage | null {
  if (typeof window === "undefined") return null;
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

/** Read + parse one cache entry; null on miss/corruption. */
export function readCache<T>(key: string): CacheEnvelope<T> | null {
  const store = storage();
  if (!store) return null;
  try {
    return deserializeCache<T>(store.getItem(key));
  } catch {
    return null;
  }
}

/** Write one cache entry; failures (quota, privacy mode) are swallowed. */
export function writeCache<T>(key: string, data: T): void {
  const store = storage();
  if (!store) return;
  try {
    store.setItem(key, serializeCache(data, Date.now()));
  } catch {
    // Quota or privacy-mode failure: caching is best-effort, never fatal.
  }
}

/**
 * Remove every entry written by this layer (all tenants). Hooked next to
 * signOut in the org layout so no tenant data survives a sign-out.
 */
export function clearLocalCache(): void {
  const store = storage();
  if (!store) return;
  try {
    const keys: string[] = [];
    for (let i = 0; i < store.length; i++) {
      const k = store.key(i);
      if (k && k.startsWith(CACHE_PREFIX)) keys.push(k);
    }
    for (const k of keys) store.removeItem(k);
  } catch {
    // Best-effort cleanup.
  }
}
