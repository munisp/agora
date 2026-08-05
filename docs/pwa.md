# PWA — admin-web & field-pwa (SPEC-W16, Agent C)

Two installable Progressive Web Apps:

| App | Kind | Path |
| --- | --- | --- |
| Agora Admin | Next.js 15 app + PWA layer | `apps/admin-web` |
| Agora Field | Dependency-free static PWA (no framework) | `apps/field-pwa` |

## Architecture

### admin-web (Next.js + PWA)

Additive PWA layer on the existing app — no new dependencies,
`package.json`/`package-lock.json` untouched.

- `public/manifest.webmanifest` — `name: "OpenDesk"`,
  `short_name: "OpenDesk Admin"`, `display: standalone`, theme/background
  colors taken from the admin-web design tokens in `app/globals.css`
  (`--color-primary: #7c5b3e`, `--color-background: #faf7f1`).
- `public/sw.js` — service worker (strategy below). Versioned by the
  `OPENDESK_SW_V` constant; bump it on every change to bust caches.
- `public/icons/icon-{192,512}.png` — branded placeholders, maskable-safe
  (see "Regenerating icons").
- `components/pwa-register.tsx` — client component that registers `/sw.js`
  on `load`. **Skipped in development** (`NODE_ENV !== "production"`) so HMR
  is never shadowed by a stale cache.
- `components/pwa-install-prompt.tsx` — captures `beforeinstallprompt`,
  shows a dismissible warm-styled banner; dismissing snoozes it for
  **14 days** (`localStorage` key `opendesk.pwa-install-snoozed-until`);
  `appinstalled` hides it permanently.
- `app/offline/page.tsx` — offline fallback served by the SW for uncached
  navigations.
- `app/layout.tsx` — wires `manifest`, `viewport.themeColor`, and the two
  client components into the root layout (additive).

### field-pwa (static, no build step)

`index.html` + `app.js` + `sw.js` + `manifest.webmanifest` + `icons/` —
**~42KB total**, no framework, no dependencies, 2G-tolerant. Serve the
directory with any static file server (e.g. `python3 -m http.server`,
nginx, or the marketing-site static host). Warm low-saturation palette
matching `apps/marketing/styles.css` (cream `#faf5ec`, terracotta
`#b8552f`, olive `#6d7454`).

Features:

- **Auth** — tenant slug + "Sign in with Keycloak": a real OIDC
  Authorization Code + PKCE (S256) flow against the realm's public
  `admin-web` client (token/authorize endpoints under
  `{issuer}/protocol/openid-connect/*`). Tokens are stored in
  `localStorage` and refreshed via `refresh_token`. Server settings
  (issuer, client ID, API base) are editable on the sign-in screen.
  See "Honest gaps" for the current limitations.
- **Lead capture** — name / phone / notes + optional GPS auto-attach.
  GPS is opt-in per lead with consent copy ("Attach my current location…
  used only to route the lead to the nearest team member"); without
  consent no geolocation request is made.
- **Geo check-in** — one-tap button; queues a `checkin` item with
  `{lat, lng, accuracy}` (no payload). Refused if location unavailable.
- **Offline queue** (SPEC-W16 §4) — IndexedDB `opendesk-field.outbox`,
  items `{id, kind: "lead_capture"|"checkin", payload, captured_at,
  gps: {lat, lng, accuracy} | null}` (`id` = `crypto.randomUUID()`).
  Outbox list UI with per-item status (`queued` / `failed: …`) and
  discard. Flush triggers: `online` event, app start, manual "Sync now",
  and Background Sync (`od-field-flush`) where available — the service
  worker flushes the outbox itself when no page is open (auth context is
  mirrored into an IDB `meta` store for this), then messages clients to
  re-render.
- **Sync contract** — batched `POST {apiBase}/v1/field/capture`
  (default `apiBase` = `/api/bookings`, i.e. through the APISIX
  `api-bookings` route, JWT required) with headers
  `authorization: Bearer …`, `x-tenant-slug: <slug>`,
  `idempotency-key: field_capture:<batch-uuid>` and body
  `{batch_id, items: [{client_id, kind, payload, captured_at, gps}]}`.
  `client_id` is the outbox `id`; the server dedupes on it
  (`field_capture:{uuid}`, SPEC-W16 §4 / Agent B contract), so retries
  after partial failures are safe. On 2xx all sent items are deleted from
  the outbox; on 401/403 the session is dropped to demo mode; other
  failures keep items queued and mark them `failed`.

## Service worker strategy (both apps, SPEC-W16 §3)

| Request | admin-web `sw.js` | field-pwa `sw.js` |
| --- | --- | --- |
| App shell (`/`, `/offline`, `index.html`, JS/CSS, manifest, icons) | cache-first | cache-first |
| Navigations | network-first → cache → `/offline` | network-first → cached `index.html` |
| `/api/*` (GET) | network-first, **3s timeout** → cache → offline JSON `503 {error:"offline"}` | n/a (page calls API direct; writes never intercepted) |
| Non-GET | never intercepted | never intercepted |
| `/voice/*`, `/webhooks/*`, `/api/auth/*` | **never cached**, straight to network | n/a |
| Cross-origin | network only | network only |

Both workers version their caches with an `OPENDESK_SW_V` constant and
purge old `opendesk-*` caches on `activate`.

## Regenerating icons

```bash
bash scripts/gen-pwa-icons.sh
```

Uses python3 + Pillow only (no SVG toolchain). Draws simple branded
placeholders — warm-paper tile, primary disc, ring accent, desk monogram
(admin) / location pin (field) — into:

- `apps/admin-web/public/icons/icon-{192,512}.png` (primary `#7c5b3e`)
- `apps/field-pwa/icons/icon-{192,512}.png` (terracotta `#b8552f`)

All artwork stays inside the central 80% safe zone on a full-bleed square
background, so the same PNGs are valid for both `any` and `maskable`
purposes. The generated PNGs are committed; rerun the script after brand
token changes.

## Install behavior

- **admin-web** — installable once served over HTTPS (or localhost) in
  production mode; the banner appears when Chromium fires
  `beforeinstallprompt` and respects the 14-day snooze. iOS Safari has no
  `beforeinstallprompt`: use Share → Add to Home Screen (apple-touch-icon
  is wired).
- **field-pwa** — installable from any static host; portrait, standalone.

## field-pwa usage

1. Serve `apps/field-pwa/` over HTTP(S).
2. Enter the tenant slug, adjust server settings if needed, **Sign in
   with Keycloak** (staff account) — or **Continue in demo mode**.
3. Capture leads / check in. Offline items sit in the Outbox and flush
   automatically when connectivity returns.

## Honest gaps

1. **No staff-PIN endpoint exists.** Inspected booking-service
   (`internal/httpapi/server.go`): the only non-Keycloak auth surfaces are
   the customer portal magic-code flow (`/public/sites/{slug}/portal/*`,
   contact-scoped — not staff) and public booking endpoints. There is no
   tenant-slug + staff-PIN endpoint anywhere in the repo. The field PWA
   therefore implements the **real** Keycloak Authorization Code + PKCE
   flow (the same realm/client the admin-web uses) and gates everything
   else behind an honestly-labelled **demo mode** (offline queue only,
   sync disabled, amber `demo` badge).
2. **Keycloak redirect URIs (foreign file, FLAGGED).**
   `infra/keycloak/realm-opendesk.json` allows only
   `http://localhost:3001/*` and `http://localhost:9080/*` for the
   `admin-web` client. For Keycloak sign-in to work, the field-pwa's
   deployed origin must be added to `redirectUris`/`webOrigins` (or a
   dedicated `field-pwa` public client created). That file is outside
   Agent C's ownership and was not modified.
3. **`POST /v1/field/capture` is a concurrent-delivery contract.** At
   authoring time the endpoint (Agent B, SPEC-W16) is not yet in the repo;
   the client codes to the SPEC-W16 §4 contract. ASSUMPTION: the batch
   envelope `{batch_id, items:[…]}` and the `idempotency-key` header are
   additive to Agent B's per-item `client_id` dedupe; adjust
   `flush()`/`flushOutbox()` if Agent B's final shape differs.
4. **Demo-mode captures never sync** until the user signs in live; they
   remain in the outbox and flush on the first successful live session
   (same tenant slug assumed).
5. Background Sync is Chromium-only; Safari/Firefox rely on the
   `online`-event + app-start flush (per SPEC "where available").
