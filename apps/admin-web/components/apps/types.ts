/**
 * Shared apps-portal helpers (SPEC-W18 Agent B). Nothing here touches the
 * network — safe to import anywhere. The row types themselves live in
 * lib/types.ts (shared types file, additive SPEC-W18 block).
 */

/**
 * Tolerant list unwrap — the W13 envelope convention, reused verbatim from
 * app/app/[orgSlug]/cac/cac-client.tsx: list endpoints may answer with a
 * keyed envelope ({"apps":[...]}), a generic {items:[...]} envelope or a
 * bare array — accept all of them by taking the first own-property value
 * that is an array; anything else yields [].
 */
export function unwrap<T>(data: unknown): T[] {
  if (Array.isArray(data)) return data as T[];
  if (typeof data === "object" && data !== null) {
    for (const value of Object.values(data)) {
      if (Array.isArray(value)) return value as T[];
    }
  }
  return [];
}
