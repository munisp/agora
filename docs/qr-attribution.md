# QR / `?ref=` attribution — embed.js & public landing (SPEC-W13 §3, Agent E)

Offline-to-online attribution for QR campaigns: a flyer/standee QR encodes the
public site URL with a `?ref=<slug>` query param; the landing preserves the tag
through navigation and chat, and the backend applies first-touch attribution
(promo → UTM → QR ref → bare channel, per SPEC-W13 §3) when the visitor's
details reach `POST /v1/leads`.

## 1. What a QR code should encode

```
https://<your-gateway-or-site-host>/p/<siteSlug>?ref=<campaign-slug>
```

Example: `https://bookings.glowhaven.example/p/glowhaven?ref=lekki-flyer-march`.
Any slug is accepted (print a different one per placement — `mall-standee`,
`church-flyer`, …); no pre-registration is required. `?ref=` survives alongside
other params (e.g. `?ref=lekki-flyer-march&utm_source=instagram` — both are
captured; UTM keeps precedence over `ref` per the precedence list).

## 2. Public booking site capture (apps/admin-web `app/p/[siteSlug]`)

Server-side, before any redirect or client JS:

1. `buildAttributionSearchParams(searchParams)` — allowlist-parses the incoming
   query: `ref` (slug pattern `[a-z0-9][a-z0-9._-]{0,63}`) + the five standard
   `utm_*` params; everything else is dropped.
2. If any tag is present the page issues `redirect(307)` to a **canonical URL**
   `/p/<siteSlug>?ref=…&utm_…` — a stable, lowercase, sorted URL that is safe
   to share/bookmark and that analytics tools see once.
3. The client (`PublicSitePage`) reads the canonical tag with a tiny
   `useSearchParams` helper (`lib/attribution-client.ts`) and:
   - renders a dismissalable "via _slug_" pill (honest UX: the visitor can
     see and dismiss the attribution);
   - emits one best-effort beacon `POST /api/attribution {site_slug, ref,
     utm, landing_path}` (fire-and-forget, 1 s timeout — see §5);
   - passes `ref`+`utm` to the chat widget.

## 3. Chat-widget propagation (embed.js host path)

`apps/admin-web/public/embed.js`:

- On init it snapshots `window.location.search` (the **landing page's** URL —
  for embed.js hosts this is the only attribution source) with the same
  allowlist parser. Explicit `data-ref` / `data-utm-*` attributes on the
  `<script>` tag override the landing query.
- The tags ride the iframe URL as query params (`/embed/<slug>?ref=…`) so the
  embedded app sees them identically to the public page.
- On the `opendesk:lead` postMessage (chat lead capture) it rewrites the
  message data to `{phone, channel, ref, utm}` **before** handing it to the
  page's `onLeadCaptured` callback. `data.attribution_source` is set to
  `"landing"` (or `"script"` when only script attrs were present); a
  server-provided `attribution_source` in the payload always wins.
- When a page-level `data-api-base` is configured it also beacons
  `POST {apiBase}/v1/leads` with that body (202-tolerant).

## 4. Chat API metadata (additive, optional)

The widget bridge (`embed/ui-actions-bridge.tsx`) and the public chat widget
include attribution in the **existing** `meta` field of `POST /voice/chat`
(non-breaking — the voice runtime echoes `meta` through):

```json
{
  "session_id": "…", "site_slug": "glowhaven", "text": "…",
  "meta": {
    "attribution": {
      "ref": "lekki-flyer-march",
      "utm": {"utm_source": "qr", "utm_medium": "offline",
              "utm_campaign": "lekki-flyer-march"},
      "landing_path": "/p/glowhaven"
    }
  }
}
```

A `ref` with no explicit UTM is surfaced as the derived triple
`utm_source=qr / utm_medium=offline / utm_campaign=<slug>` (documented
synthesis — the same mapping the backend applies when only `ref` is given).

## 5. The attribution beacon (honest scope)

`apps/admin-web/app/api/attribution/route.ts` is a tiny BFF route that
accepts the beacon and answers `202` immediately. **Scope, explicitly:**
first-touch lead attribution happens **server-side in booking-service** when
the lead is actually created (chat contact form, public booking POST, promo
redeem — see `docs/leads-attribution.md` §3). There is no
`POST /v1/leads/attribution` write endpoint in Wave 13, so the route does not
forward upstream; it is the intake point for raw landing telemetry (which
landing paths carry which tags) ahead of any such endpoint. It validates the
body (slug pattern for `site_slug` and `ref`, utm map of strings), and returns
`400` on garbage. Visitors' tags are never trusted for anything else.

## 6. Files

| File | Change |
|---|---|
| `apps/admin-web/lib/attribution.ts` | allowlist parser, canonical URL builder, derived `utm_source=qr` triple (shared, tested) |
| `apps/admin-web/lib/attribution-client.ts` | tiny client hook reading canonical tags via `useSearchParams` |
| `apps/admin-web/app/p/[siteSlug]/page.tsx` | canonical-URL redirect + tags passed to client |
| `apps/admin-web/app/p/[siteSlug]/public-site-client.tsx` | "via _slug_" pill + beacon + chat handoff |
| `apps/admin-web/public/embed.js` | landing-query capture, script-attr overrides, iframe propagation, lead-callback rewrite |
| `apps/admin-web/app/embed/ui-actions-bridge.tsx` | `meta.attribution` on `/voice/chat` |
| `apps/admin-web/components/chat-widget.tsx` | `attribution` prop → `meta.attribution` |
| `apps/admin-web/app/api/attribution/route.ts` | beacon route (202/400) |
| `apps/admin-web/lib/__tests__/attribution.test.ts` | vitest: parser, canonical URL, derived triple |

## 7. Test

```bash
cd apps/admin-web && npx vitest run lib/__tests__/attribution.test.ts
```

Manual smoke: open `/p/acme?ref=standee-1&utm_source=qr` → URL canonicalises,
pill shows "via standee-1", `/api/attribution` receives the beacon (network
tab), chat posts carry `meta.attribution`. Print a QR pointing at the same URL
(e.g. `qrencode`) and scan with a phone to verify end-to-end.
