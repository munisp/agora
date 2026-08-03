# QR / promo landing attribution (SPEC-W13, Agent E)

How a QR scan, UTM link or `?promo=` share link becomes first-touch lead
attribution — entirely on the frontend/edge side. Backend lead storage,
precedence and dedupe are owned by booking-service (Agent A, SPEC-W13 §1/§3/§6);
the frontend **only forwards** what it observes.

## Contract bindings

- **§3 precedence**: `promo_code` > UTM (`utm_source/medium/campaign`) >
  QR slug > `channel_of_first_touch`. First-touch wins and is **never
  overwritten** — enforced server-side. The widget may forward a newer
  payload on a later visit; the backend ignores it for attribution.
- **§6 promo redemption**: `?promo=` captured here is forwarded as
  `attribution.promo_code`. Actual redemption (`POST /v1/promo/redeem`,
  idempotent per code+phone) is a booking-service endpoint and unchanged.

## Pieces

### 1. `apps/admin-web/public/embed.js` — host-page URL capture

On widget init (after iframe load, one-shot) the loader reads the **host
page** URL and, if any of these are present, postMessages them into the
widget:

```
{type:"opendesk:attribution",
 attribution:{utm:{source?, medium?, campaign?}, promo_code?, ref?}}
```

| URL param      | Forwarded as                  |
| -------------- | ----------------------------- |
| `utm_source`   | `attribution.utm.source`      |
| `utm_medium`   | `attribution.utm.medium`      |
| `utm_campaign` | `attribution.utm.campaign`    |
| `promo`        | `attribution.promo_code`      |
| `ref`          | `attribution.ref` (QR slug)   |

Framework-free, dependency-free, silent no-op when the URL carries none of
them (nothing is sent). Values are trimmed and capped at 120 chars. No PII
beyond URL params the host page already exposes. Mirrors the Wave 11
`opendesk:location` pattern; failures never break the host page or widget.

### 2. `apps/admin-web/app/embed/ui-actions-bridge.tsx` — fetch-tap merge

The bridge listens for `opendesk:attribution` with the same trust rules as
the Wave 11 location message (direct parent frame only, origin-checked
against `document.referrer` when available), sanitizes it, and the existing
namespaced fetch tap merges it as an additive `attribution` key into the
JSON body of every `/voice/chat` request — **exactly like
`client_location`** (Wave 11). The two mergers compose:
`withAttribution(withClientLocation(init))`; each returns the init
untouched when it has nothing to add, so bodies pass through byte-identical
when neither is present. The server tolerates unknown keys.

Wave 9 `ui_actions` response scanning and Wave 11 `client_location` merge
are untouched: the tap's URL matching, clone-scanning, listener fan-out and
fetch restore logic are unchanged; attribution only adds one message
listener and one body-merge step.

### 3. `apps/admin-web/app/l/[slug]/route.ts` — QR landing redirect

Printed QR codes point at `https://<host>/l/{slug}` where `{slug}` is the
tenant's **public site slug** (same slug as `/p/{siteSlug}`). The route:

1. Validates the slug (`^[a-z0-9][a-z0-9-]{0,62}$`, else 404 `invalid_slug`).
2. 302-redirects to the tenant booking page with first-touch QR attribution:
   `/p/{slug}?utm_source=qr&utm_medium=offline&utm_campaign={slug}`.
3. Fires a **best-effort server-side funnel ping** so scans are countable
   even if the visitor bounces before any widget loads.

The route does not look up the site (the booking page itself 404s unknown
or unpublished slugs), keeps no state, and never blocks the redirect on the
ping. It is public — `middleware.ts` only guards `/app/*`.

> **Slug assumption**: there is no QR-slug registry table in the platform
> today, so the QR slug IS the public site slug. If booking-service later
> adds a slug→tenant alias table, only this route's target resolution
> changes; the redirect and ping shape stay the same.

#### Funnel ping configuration

The platform currently has **no HTTP analytics-ingest endpoint** —
analytics-pipeline consumes Kafka bronze topics and serves only read-only
REST (`/healthz`, `/metrics`, `/v1/recommendations`, `/v1/metering`). Per
SPEC-W13 scope ("do not invent backend changes"), the ping target is
opt-in:

- `QR_FUNNEL_PING_URL` set → `POST <url>` with JSON body, fire-and-forget,
  1.5 s abort timeout, failures swallowed (the redirect is never affected).
  Point it at a future ingest route or the BFF `/api/analytics/*` proxy
  (`ANALYTICS_BASE_URL`) once Agent B's service exposes event ingest.
- unset → a structured JSON line is logged
  (`{"msg":"qr_landing", ...}`), scrapeable by the log pipeline.

Ping payload (field names follow the §2 funnel envelope where applicable):

```json
{
  "event_name": "qr_landing",
  "channel": "qr",
  "campaign": "<slug>",
  "tenant_site_slug": "<slug>",
  "event_ts": "2025-01-01T00:00:00.000Z",
  "idempotency_key": "qr:<slug>:<epoch-ms>"
}
```

This is a scan-count signal, not a lead: lead creation and the §2
`cac.events` FunnelEvent emission happen backend-side when the visitor
actually engages (chat/booking/promo redeem).

## End-to-end flow

```
QR print ──scan──> GET /l/{slug}
                     ├─ 302 /p/{slug}?utm_source=qr&utm_medium=offline
                     │           &utm_campaign={slug}
                     └─ fire-and-forget funnel ping (or structured log)

Embed host page ──> embed.js reads utm_*/promo/ref from host URL
                     └─ postMessage opendesk:attribution ──> widget

Widget chat ──> fetch tap merges {attribution:{...}} (+ client_location)
                into POST /voice/chat body ──> backend applies §3
                precedence, first-touch never overwritten
```

## Verification

- `node --check apps/admin-web/public/embed.js` — syntax clean.
- `npx tsc --noEmit` (repo tsconfig) — clean, no new dependencies.
- Regression reasoning: embed.js additions are a new section + one
  `load` listener; the Wave 9 action listener and Wave 11 GPS capture are
  untouched. The bridge diff is additive-only around the two Wave 9/11
  merge/listen points; the fetch tap's structure (namespacing, ref-counted
  restore, clone-scanning) is unchanged.
