# W68 e2e — field-pwa service-worker no-store / API guards (manual script)

Manual end-to-end verification for SPEC-W68 WS1 (G-SW1/2/3/4). The bug
(RECON §WS1): the SW's same-origin GET handler was cache-first with no
`req.cache` check, so it (a) stored the W63 gazetteer snapshot fetched with
`cache:"no-store"` and served it without the ETag/304 conditional ever
reaching geo-service, and (b) cached tenant-private
`/api/bookings/v1/bookings` GETs under a shared-origin URL. The fix is in
`apps/field-pwa/sw.js` only (`app.js` is byte-identical): two request-side
early-returns before `respondWith`, a response-side `Cache-Control:
no-store` skip in the populate branch, and the mandatory
`field-pwa-v5` → `field-pwa-v6` version bump (activation evicts the
poisoned v5 caches).

Conventions: `PWA` = the origin serving the field PWA (e.g.
`https://field.example` or `http://localhost:8080` when hosted statically),
`GEO` reachable at `$PWA/api/geo`, `BOOK` at `$PWA/api/bookings`. Steps are
written for Chrome/Edge DevTools (Application → Service Workers / Cache
Storage, Network panel with "Disable cache" OFF — we are testing the SW's
cache, not the browser HTTP cache). `sw.js` must pass `node --check
apps/field-pwa/sw.js` before starting.

## 0. Prerequisites

1. Stack up (docker compose) with geo-service and booking-service behind
   the gateway so `$PWA/api/geo/...` and `$PWA/api/bookings/...` resolve;
   the PWA static files (index.html, app.js, sw.js, manifest) served from
   the same origin.
2. A Keycloak session for a field user in tenant `$T` (the PWA login flow).
3. Hard-reset the SW state: DevTools → Application → Service Workers →
   Unregister; Application → Cache Storage → delete every `opendesk-field-*`
   entry; then reload once so the NEW `sw.js` (v6) installs.

## 1. Static gate (before any browser run)

```bash
node --check apps/field-pwa/sw.js && echo "syntax OK"
grep -n 'OPENDESK_SW_V = "field-pwa-v6"' apps/field-pwa/sw.js   # exactly one hit
grep -n 'req.cache === "no-store"' apps/field-pwa/sw.js         # guard present, before respondWith
grep -n '"/api/"' apps/field-pwa/sw.js                          # API guard present
grep -n 'cache-control' apps/field-pwa/sw.js                    # response-side no-store skip
md5sum apps/field-pwa/app.js                                    # must equal the pristine/base md5 (byte-identical)
```

## 2. First fetch: no-store snapshot is NOT stored (G-SW1)

1. Open the PWA, log in as the `$T` field user, open DevTools → Application
   → Cache Storage → `opendesk-field-field-pwa-v6`.
2. Trigger the gazetteer sync (app boot path; `app.js` issues
   `fetch(geoBase + "/v1/geo/gazetteer/snapshot?levels=state,lga", { cache: "no-store" })`).
3. Expected:
   - Network panel: the snapshot request shows `(ServiceWorker)` in the
     Size column is NOT required — it may pass straight through; what
     matters is the request HIT THE NETWORK (status 200 on first sync).
   - Cache Storage `opendesk-field-field-pwa-v6` contains ONLY the precache
     entries (`./`, `index.html`, `app.js`, `manifest.webmanifest`,
     `icons/agora-icon.svg`). There is **NO** entry whose URL contains
     `gazetteer/snapshot`. (Pre-fix behavior: the snapshot response WAS
     listed here.)

## 3. Later syncs: ETag/304 conditional reaches the network (G-SW1)

1. Trigger the gazetteer sync a second time (reload the page or re-run the
   sync action) WITHOUT clearing anything.
2. Expected in the Network panel: a NEW network request to
   `.../v1/geo/gazetteer/snapshot?levels=state,lga` WITH the
   `If-None-Match` request header carrying the ETag from step 2, and a
   `304 Not Modified` response (geo-service `gazetteer.rs` ETag flow).
3. Cache Storage still has no `gazetteer/snapshot` entry.
   (Pre-fix behavior: ZERO network requests on later syncs — the SW served
   the cached v5 copy and the conditional GET never left the device.)

## 4. /api/bookings GET is never cached or served stale (G-SW2)

1. As the `$T` user, open the bookings view (`loadMyBookings` issues
   `GET $PWA/api/bookings/v1/bookings?mine=true...` with Authorization +
   X-Tenant-Slug).
2. Expected:
   - Network panel: the bookings GET hits the network every time the view
     loads (no `(from ServiceWorker cache)` result without a network
     round-trip; a reload shows a fresh request).
   - Cache Storage `opendesk-field-field-pwa-v6` gains **NO** entry whose
     path starts with `/api/`.
3. Cross-user check (the original leak shape): log out, log in as a
   DIFFERENT user (same or another tenant) on the same device/profile, open
   the bookings view. Expected: the second user sees ONLY their own
   bookings served from the network — never the first user's cached list.
   (Pre-fix behavior: the first user's bookings response was served from
   the Cache API because the cache key is the URL only.)

## 5. Activation evicts poisoned v5 caches (G-SW4)

1. Simulate a poisoned device: in DevTools → Application → Cache Storage,
   create a cache named `opendesk-field-field-pwa-v5` (Console:
   `caches.open("opendesk-field-field-pwa-v5").then(c => c.put("/fake", new Response("x")))`)
   plus a bogus `gazetteer/snapshot` entry inside it.
2. Force re-activation of the v6 worker: Application → Service Workers →
   "skipWaiting"/Update, or close and reopen the tab.
3. Expected: Cache Storage lists ONLY `opendesk-field-field-pwa-v6`; every
   other `opendesk-field-*` cache (including the v5 one with its poisoned
   entries) is deleted by the activate handler. Caches NOT matching the
   `opendesk-field-` prefix are untouched.

## 6. Offline navigation still works (regression — untouched path)

1. With the app loaded once (precache populated), DevTools → Network →
   Offline, then reload / navigate to `$PWA/`.
2. Expected: the shell loads offline (navigation is network-first with the
   `index.html` fallback, unchanged by W68); no console errors from the SW.
3. Re-enable the network and reload: a fresh navigation request hits the
   network (network-first freshness preserved).

## 7. IndexedDB queues unaffected (regression — untouched)

1. Go offline; perform an action that enqueues a write (field report /
   receipt capture). Expected: the entry lands in the IndexedDB outbox
   (Application → IndexedDB), no SW interaction.
2. Go online; the page boot path (or the `od-field-flush` Background Sync
   message) flushes the queue. Expected: the queued POST reaches the API
   and the IDB outbox drains. (The SW still never sees tokens and only
   postMessages `"flush"` — TS-003 posture unchanged.)

## 8. Pass criteria

- §1 static gate: all greps hit exactly as written; `app.js` md5 identical
  to pristine; `node --check` clean.
- §2/§3: snapshot never appears in Cache Storage; every sync issues a
  network request; the conditional GET carries `If-None-Match` and gets 304
  when unchanged.
- §4: no `/api/` entry in Cache Storage at any point; second user never
  sees the first user's data.
- §5: activation leaves only `opendesk-field-field-pwa-v6` under the
  `opendesk-field-` prefix.
- §6/§7: offline shell, navigation fallback, and IDB outbox/flush all
  behave exactly as pre-W68.
