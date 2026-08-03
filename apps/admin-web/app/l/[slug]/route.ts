import { NextRequest, NextResponse } from "next/server";

/**
 * QR landing redirect (SPEC-W13, Agent E).
 *
 * Printed QR codes (flyers, shop windows, field-agent cards) point at
 *   https://<host>/l/{slug}
 * where {slug} is the tenant's public site slug (the same slug used by the
 * public booking page /p/{siteSlug}). This endpoint 302-redirects to the
 * tenant booking page with first-touch QR attribution appended:
 *   /p/{slug}?utm_source=qr&utm_medium=offline&utm_campaign={slug}
 * From there the standard widget/embed attribution path (embed.js →
 * opendesk:attribution → /voice/chat merge) carries the UTM triplet to the
 * backend, which enforces precedence (promo_code > UTM > QR slug >
 * channel_of_first_touch) and first-touch-never-overwritten per SPEC-W13 §3.
 *
 * A best-effort server-side funnel ping is fired for every scan so QR scans
 * are countable even when the visitor bounces before the widget loads.
 * There is currently NO HTTP analytics-ingest endpoint in the platform
 * (analytics-pipeline consumes Kafka bronze topics and only serves
 * read-only REST), so the ping target is opt-in: set QR_FUNNEL_PING_URL to
 * any HTTP endpoint that accepts a JSON POST (e.g. a future ingest route or
 * the BFF /api/analytics proxy) and the ping is posted fire-and-forget.
 * Without it, a structured JSON line is logged instead — see
 * docs/qr-attribution.md. No backend was changed for this.
 *
 * The route is intentionally minimal: it does not look up the site (the
 * booking page itself 404s unpublished/unknown slugs), keeps no state and
 * never blocks the redirect on the ping. Public by design — middleware.ts
 * only guards /app/*.
 */

/** Same charset as public site slugs; rejects path traversal and junk. */
const SLUG_RE = /^[a-z0-9][a-z0-9-]{0,62}$/i;

const FUNNEL_PING_URL = process.env.QR_FUNNEL_PING_URL;
const PING_TIMEOUT_MS = 1500;

/** Fire-and-forget funnel ping; resolves silently on any failure. */
function pingFunnel(payload: Record<string, unknown>): void {
  if (!FUNNEL_PING_URL) {
    // Structured log fallback — scrapeable by the platform log pipeline.
    console.log(JSON.stringify({ msg: "qr_landing", ...payload }));
    return;
  }
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), PING_TIMEOUT_MS);
  void fetch(FUNNEL_PING_URL, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(payload),
    cache: "no-store",
    signal: controller.signal,
  })
    .catch(() => {
      /* best-effort — a failed ping never affects the redirect */
    })
    .finally(() => clearTimeout(timer));
}

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ slug: string }> },
) {
  const { slug } = await params;
  if (!SLUG_RE.test(slug)) {
    return NextResponse.json({ error: "invalid_slug" }, { status: 404 });
  }

  const target = new URL(`/p/${encodeURIComponent(slug)}`, req.nextUrl.origin);
  target.searchParams.set("utm_source", "qr");
  target.searchParams.set("utm_medium", "offline");
  target.searchParams.set("utm_campaign", slug);

  pingFunnel({
    event_name: "qr_landing",
    channel: "qr",
    campaign: slug,
    tenant_site_slug: slug,
    event_ts: new Date().toISOString(),
    idempotency_key: `qr:${slug}:${Date.now()}`,
  });

  return NextResponse.redirect(target, 302);
}

export const dynamic = "force-dynamic";
