/**
 * Client-side API wrapper. Browser code calls the local BFF proxy
 * (`/api/[[...path]]/route.ts`), which attaches the Keycloak access token and
 * forwards to the APISIX gateway. Paths here therefore start with "/api/..."
 * exactly as the gateway exposes them (e.g. "/api/bookings/v1/bookings").
 */

export class ApiError extends Error {
  status: number;
  body: unknown;

  constructor(status: number, body: unknown) {
    const message =
      typeof body === "object" && body !== null && "message" in body
        ? String((body as { message: unknown }).message)
        : typeof body === "object" && body !== null && "error" in body
          ? String((body as { error: unknown }).error)
          : `Request failed with status ${status}`;
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.body = body;
  }
}

type Query = Record<string, string | number | boolean | undefined | null>;

function withQuery(path: string, query?: Query): string {
  if (!query) return path;
  const params = new URLSearchParams();
  for (const [k, v] of Object.entries(query)) {
    if (v === undefined || v === null || v === "") continue;
    params.set(k, String(v));
  }
  const qs = params.toString();
  return qs ? `${path}?${qs}` : path;
}

async function request<T>(
  method: string,
  path: string,
  opts: {
    query?: Query;
    body?: unknown;
    signal?: AbortSignal;
    /** extra request headers, e.g. { "x-tenant-id": tenantId } for the billing-engine contract */
    headers?: Record<string, string>;
  } = {},
): Promise<T> {
  const headers: Record<string, string> = { ...(opts.headers ?? {}) };
  if (opts.body !== undefined) headers["content-type"] = "application/json";
  // booking-service resolves tenants via the X-Tenant-Slug header; the
  // ?tenant= query param is kept for other services that still read it.
  const tenant = opts.query?.tenant;
  if (tenant !== undefined && tenant !== null && tenant !== "") {
    headers["x-tenant-slug"] = String(tenant);
  }

  const res = await fetch(withQuery(path, opts.query), {
    method,
    headers,
    body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
    signal: opts.signal,
    cache: "no-store",
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
  // Session expired (the BFF answers 401 {error:"session_expired"} only for a dead dashboard session): go back through
  // sign-in instead of showing empty pages and "Request failed with status 401". Other 401s are left to the page.
  if (res.status === 401 && typeof window !== "undefined" &&
      (data as { error?: unknown } | null)?.error === "session_expired") {
    window.location.href = `/sign-in?callbackUrl=${encodeURIComponent(window.location.pathname)}`;
  }
  if (!res.ok) throw new ApiError(res.status, data);
  return data as T;
}

export const api = {
  get: <T>(
    path: string,
    query?: Query,
    signal?: AbortSignal,
    headers?: Record<string, string>,
  ) => request<T>("GET", path, { query, signal, headers }),
  post: <T>(
    path: string,
    body?: unknown,
    query?: Query,
    headers?: Record<string, string>,
  ) => request<T>("POST", path, { body, query, headers }),
  put: <T>(
    path: string,
    body?: unknown,
    query?: Query,
    headers?: Record<string, string>,
  ) => request<T>("PUT", path, { body, query, headers }),
  patch: <T>(
    path: string,
    body?: unknown,
    query?: Query,
    headers?: Record<string, string>,
  ) => request<T>("PATCH", path, { body, query, headers }),
  delete: <T>(path: string, query?: Query) =>
    request<T>("DELETE", path, { query }),
};
