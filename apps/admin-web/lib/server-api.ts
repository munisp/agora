import "server-only";
import { auth } from "@/lib/auth";
import { ApiError } from "@/lib/api";

/**
 * Server-side API wrapper for Server Components and route handlers.
 * Talks to the APISIX gateway directly with the session's access token.
 * For unauthenticated (public) calls it simply omits the Authorization header.
 */

const API_BASE =
  process.env.API_BASE_URL ??
  process.env.NEXT_PUBLIC_API_BASE ??
  "http://localhost:9080";

type Query = Record<string, string | number | boolean | undefined | null>;

function buildUrl(path: string, query?: Query): string {
  const url = new URL(`${API_BASE}${path.startsWith("/") ? path : `/${path}`}`);
  if (query) {
    for (const [k, v] of Object.entries(query)) {
      if (v === undefined || v === null || v === "") continue;
      url.searchParams.set(k, String(v));
    }
  }
  return url.toString();
}

export async function serverApi<T>(
  path: string,
  opts: {
    method?: string;
    query?: Query;
    body?: unknown;
    /** skip auth even if a session exists (public endpoints) */
    anonymous?: boolean;
    /**
     * ISR cache TTL in seconds (SPEC-W46 AW-3). Only for anonymous GETs of
     * semi-static public data — the fetch becomes cacheable in the Next.js
     * Data Cache instead of `no-store`. Never use on authed or per-user
     * responses.
     */
    revalidate?: number;
  } = {},
): Promise<T> {
  const headers: Record<string, string> = { accept: "application/json" };
  if (opts.body !== undefined) headers["content-type"] = "application/json";
  // booking-service resolves tenants via the X-Tenant-Slug header.
  const tenant = opts.query?.tenant;
  if (tenant !== undefined && tenant !== null && tenant !== "") {
    headers["x-tenant-slug"] = String(tenant);
  }

  if (!opts.anonymous) {
    const session = await auth();
    if (session?.accessToken) {
      headers.authorization = `Bearer ${session.accessToken}`;
    }
  }

  const res = await fetch(buildUrl(path, opts.query), {
    method: opts.method ?? "GET",
    headers,
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
    // AW-3: revalidate (public semi-static GETs) opts into the Data Cache;
    // everything else keeps the existing no-store behavior.
    cache: opts.revalidate ? undefined : "no-store",
    next: opts.revalidate ? { revalidate: opts.revalidate } : undefined,
  });

  const text = await res.text();
  let data: unknown = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      data = text;
    }
  }
  if (!res.ok) throw new ApiError(res.status, data);
  return data as T;
}

/** Unwrap list responses that may be a bare array or a { items } envelope. */
export function unwrapList<T>(data: unknown): T[] {
  if (Array.isArray(data)) return data as T[];
  if (
    typeof data === "object" &&
    data !== null &&
    Array.isArray((data as { items?: unknown }).items)
  ) {
    return (data as { items: T[] }).items;
  }
  return [];
}
