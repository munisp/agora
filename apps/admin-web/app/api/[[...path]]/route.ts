import { NextRequest, NextResponse } from "next/server";
import { auth } from "@/lib/auth";

/**
 * BFF proxy: forwards /api/* to the APISIX gateway with the caller's Keycloak
 * access token attached. The gateway routes /api/bookings/*, /api/payments/*,
 * /api/knowledge/*, /voice/* etc. to the backing services (SPEC §12).
 *
 * Requests without a session are forwarded anonymously, which is what the
 * public booking endpoints (/api/bookings/public/*) and /voice/chat expect.
 *
 * EXCEPTION (W44/F15-15): /api/analytics/* is forwarded DIRECTLY to the
 * analytics-pipeline service (not through APISIX, so no OIDC plugin runs
 * there). Those paths therefore require a NextAuth session here — 401 for
 * anonymous callers, mirroring the middleware.ts /app/* session check — and
 * the tenant is taken from the session's verified org claim
 * (`tenant_slugs`), never from the caller: a caller-supplied ?tenant= is
 * only honored when it is one of the session's tenant slugs (binding),
 * otherwise the session's first slug is used. The bound slug is injected as
 * X-Tenant-Slug and rewrites the ?tenant= query param, so a forged value can
 * never cross tenants.
 *
 * EXCEPTION (SPEC-W77 R3): /api/ocr-registry/* is forwarded DIRECTLY to the
 * ocr-service (W53 posture: /v1/internal/* routes stay in-cluster and are
 * NEVER added to APISIX). Exactly ONE upstream endpoint is exposed:
 *   GET /api/ocr-registry/registry-stats
 *     → GET ${OCR_BASE_URL}/v1/internal/ocr/registry-stats
 * the platform-scoped cross-tenant doc-fraud registry aggregate counts
 * (R1 count-not-identity: integers + posture fields ONLY — no registry
 * keys, no tenant ids, no hashes, no per-key rows). Gate: NextAuth session
 * required (401 anonymous) and the caller must hold the platform-admin
 * realm role — 403 otherwise; plain `admin` is NOT enough (R2, stricter
 * than the ops-alerts gate). OCR_INTERNAL_TOKEN is injected server-side as
 * x-internal-token and NEVER reaches the client bundle. GET only; no
 * tenant binding (platform scope).
 */

const API_BASE =
  process.env.API_BASE_URL ??
  process.env.NEXT_PUBLIC_API_BASE ??
  "http://localhost:9080";

/**
 * APISIX has no /api/analytics route (the analytics-pipeline service is not
 * exposed through the gateway). Browser code therefore calls
 * /api/analytics/<rest> and the BFF forwards it directly to the service
 * (docker network name `analytics`, port 7009) — see SPEC-W3 §5.
 */
const ANALYTICS_BASE =
  process.env.ANALYTICS_BASE_URL ?? "http://localhost:7009";

/**
 * SPEC-W77 R3: ocr-service is not exposed through the gateway (same posture
 * as analytics). The single registry-stats path handled below forwards to
 * `${OCR_BASE}/v1/internal/ocr/registry-stats`.
 */
const OCR_BASE = process.env.OCR_BASE_URL ?? "http://ocr:7020";

const HOP_BY_HOP = new Set([
  "connection",
  "keep-alive",
  "transfer-encoding",
  "upgrade",
  "host",
  "content-length",
]);

async function proxy(
  req: NextRequest,
  ctx: { params: Promise<{ path?: string[] }> },
): Promise<NextResponse> {
  const { path = [] } = await ctx.params;
  const directToAnalytics = path[0] === "analytics";
  const directToOcrRegistry = path[0] === "ocr-registry";

  const session = await auth();

  // SPEC-W77 R2/R3: the ocr-service registry-stats endpoint is internal-only
  // (X-Internal-Token), so it is only as protected as this handler makes it.
  if (directToOcrRegistry) {
    if (req.method !== "GET") {
      return NextResponse.json(
        { error: "method_not_allowed", message: "Registry stats is read-only (GET)." },
        { status: 405 },
      );
    }
    if (!session?.accessToken) {
      return NextResponse.json(
        { error: "authentication_required", message: "Sign-in required for registry stats." },
        { status: 401 },
      );
    }
    // platform-admin ONLY — plain `admin` must not see cross-tenant
    // aggregates (R2).
    if (!(session.realmRoles ?? []).includes("platform-admin")) {
      return NextResponse.json(
        {
          error: "platform_admin_required",
          message: "Registry stats require the platform-admin role.",
        },
        { status: 403 },
      );
    }
    // Zero-disclosure surface: only the aggregate-stats endpoint is exposed
    // through this exception; every other ocr-registry path 404s.
    if (path.join("/") !== "ocr-registry/registry-stats") {
      return NextResponse.json(
        { error: "not_found", message: "Unknown ocr-registry path." },
        { status: 404 },
      );
    }
    if (!process.env.OCR_INTERNAL_TOKEN) {
      // Fail-closed, mirroring the service's own posture: no token configured
      // here means the upstream call could never authenticate anyway.
      return NextResponse.json(
        {
          error: "ocr_internal_token_unset",
          message: "OCR_INTERNAL_TOKEN is not configured on admin-web.",
        },
        { status: 503 },
      );
    }
  }

  // W44/F15-15: the analytics service is NOT behind the gateway, so its
  // endpoints are only as protected as this handler makes them.
  let analyticsTenantSlug: string | null = null;
  if (directToAnalytics) {
    if (!session?.accessToken) {
      return NextResponse.json(
        { error: "authentication_required", message: "Sign-in required for analytics." },
        { status: 401 },
      );
    }
    // Tenant from the session org claim (tenant_slugs). A caller-supplied
    // ?tenant= is honored only when it is one of those slugs; anything else
    // (including a foreign tenant) falls back to the session's own slug.
    const requested = req.nextUrl.searchParams.get("tenant");
    const slugs = session.tenantSlugs ?? [];
    analyticsTenantSlug =
      requested && slugs.includes(requested) ? requested : (slugs[0] ?? null);
    if (!analyticsTenantSlug) {
      return NextResponse.json(
        {
          error: "no_tenant_membership",
          message: "Session carries no tenant_slugs claim; analytics unavailable.",
        },
        { status: 403 },
      );
    }
  }

  const target = new URL(
    directToAnalytics
      ? `${ANALYTICS_BASE}/${path.slice(1).join("/")}`
      : directToOcrRegistry
        ? `${OCR_BASE}/v1/internal/ocr/registry-stats`
        : `${API_BASE}/${path.join("/")}`,
  );
  target.search = req.nextUrl.search;
  if (analyticsTenantSlug) {
    // Caller-supplied tenant params are ignored: the query carries the
    // session-bound slug only (recommendations/metering read ?tenant=).
    target.searchParams.set("tenant", analyticsTenantSlug);
  }

  const headers = new Headers();
  req.headers.forEach((value, key) => {
    const lower = key.toLowerCase();
    if (HOP_BY_HOP.has(lower) || lower === "authorization" || lower === "cookie") return;
    // Never forward a caller-sent tenant header on analytics paths; the
    // session-bound value is injected below.
    if (directToAnalytics && lower === "x-tenant-slug") return;
    // Never forward a caller-sent internal token on ocr-registry paths; the
    // server-side token is injected below and must be the only one.
    if (directToOcrRegistry && lower === "x-internal-token") return;
    headers.set(key, value);
  });
  if (!headers.has("accept")) headers.set("accept", "application/json");
  if (analyticsTenantSlug) {
    headers.set("x-tenant-slug", analyticsTenantSlug);
  }
  if (directToOcrRegistry) {
    // Set above: the handler 503s before this point when the env is unset.
    headers.set("x-internal-token", process.env.OCR_INTERNAL_TOKEN as string);
  }

  if (session?.accessToken && !directToOcrRegistry) {
    headers.set("authorization", `Bearer ${session.accessToken}`);
  }

  const hasBody = !["GET", "HEAD"].includes(req.method);
  // W52-D: binary uploads (e.g. .gpkg/.zip boundary imports to
  // /api/geo/v1/geo/boundaries/import) must be forwarded as raw bytes —
  // req.text() decodes the payload as UTF-8 and corrupts non-UTF-8 bytes.
  // Known-textual content types keep the existing text() path; anything
  // else goes through arrayBuffer(). Content-Length is hop-by-hop (stripped
  // above) so fetch recomputes it from the forwarded body; Content-Type is
  // forwarded verbatim either way.
  const mediaType = (req.headers.get("content-type") ?? "")
    .split(";")[0]
    .trim()
    .toLowerCase();
  const isTextual =
    mediaType === "" ||
    mediaType.startsWith("text/") ||
    mediaType === "application/json" ||
    mediaType === "application/x-www-form-urlencoded";
  let upstream: Response;
  try {
    upstream = await fetch(target, {
      method: req.method,
      headers,
      body: hasBody
        ? isTextual
          ? await req.text()
          : await req.arrayBuffer()
        : undefined,
      cache: "no-store",
      // @ts-expect-error -- Node fetch supports duplex for streaming bodies
      duplex: hasBody ? "half" : undefined,
    });
  } catch (err) {
    return NextResponse.json(
      {
        error: "gateway_unreachable",
        message: `Could not reach ${
          directToAnalytics
            ? `analytics service at ${ANALYTICS_BASE}`
            : directToOcrRegistry
              ? `ocr service at ${OCR_BASE}`
              : `API gateway at ${API_BASE}`
        }: ${String(err)}`,
      },
      { status: 502 },
    );
  }

  const body = await upstream.arrayBuffer();
  const res = new NextResponse(body, { status: upstream.status });
  const contentType = upstream.headers.get("content-type");
  if (contentType) res.headers.set("content-type", contentType);
  return res;
}

export const GET = proxy;
export const POST = proxy;
export const PUT = proxy;
export const PATCH = proxy;
export const DELETE = proxy;
export const dynamic = "force-dynamic";
