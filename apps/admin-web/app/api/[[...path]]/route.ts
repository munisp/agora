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

  const session = await auth();

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
    headers.set(key, value);
  });
  if (!headers.has("accept")) headers.set("accept", "application/json");
  if (analyticsTenantSlug) {
    headers.set("x-tenant-slug", analyticsTenantSlug);
  }

  if (session?.accessToken) {
    headers.set("authorization", `Bearer ${session.accessToken}`);
  }

  const hasBody = !["GET", "HEAD"].includes(req.method);
  let upstream: Response;
  try {
    upstream = await fetch(target, {
      method: req.method,
      headers,
      body: hasBody ? await req.text() : undefined,
      cache: "no-store",
      // @ts-expect-error -- Node fetch supports duplex for streaming bodies
      duplex: hasBody ? "half" : undefined,
    });
  } catch (err) {
    return NextResponse.json(
      {
        error: "gateway_unreachable",
        message: `Could not reach ${directToAnalytics ? `analytics service at ${ANALYTICS_BASE}` : `API gateway at ${API_BASE}`}: ${String(err)}`,
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
