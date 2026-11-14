/* OpenDesk Field service worker (SPEC-W16 §3/§4; W68 G-SW1/2/3/4 guards).
 *
 * - App shell (index, app.js, manifest, icons): cache-first.
 * - API calls (any same-origin /api/* path, incl. /v1/*) and cross-origin
 *   token endpoints: network only — the fetch handler returns early for
 *   every /api/ GET (G-SW2), so tenant-private data (e.g. /api/bookings)
 *   is never stored in or served from the Cache API. Writes are queued in
 *   IndexedDB by the page, never replayed from cache.
 * - Requests with cache:"no-store" (e.g. the W63 gazetteer snapshot, whose
 *   ETag/304 conditional GET must always reach geo-service) bypass both
 *   cache-match and cache-put (G-SW1). Defense-in-depth: responses whose
 *   Cache-Control contains "no-store" are never written to the cache even
 *   if a caller forgot the request flag (G-SW3).
 * - Background Sync ("od-field-flush"): asks any open page to flush the
 *   IndexedDB outbox. Tokens are session-only and never reach this worker
 *   (TS-003), so the authenticated flush always runs in a page context.
 *
 * Bump OPENDESK_SW_V on every change (cache busting).
 */
"use strict";

var OPENDESK_SW_V = "field-pwa-v6"; // SPEC-W68: mandatory bump (G-SW4; activation evicts poisoned v5 caches: no-store/API guard for the cache-first GET handler)
var SHELL = "opendesk-field-" + OPENDESK_SW_V;

var PRECACHE = [
  "./",
  "index.html",
  "app.js",
  "manifest.webmanifest",
  "icons/agora-icon.svg",
];

self.addEventListener("install", function (event) {
  event.waitUntil(
    caches.open(SHELL).then(function (c) { return c.addAll(PRECACHE); })
      .then(function () { return self.skipWaiting(); })
  );
});

self.addEventListener("activate", function (event) {
  event.waitUntil(
    caches.keys().then(function (names) {
      return Promise.all(names.map(function (n) {
        if (n.indexOf("opendesk-field-") === 0 && n !== SHELL) return caches.delete(n);
      }));
    }).then(function () { return self.clients.claim(); })
  );
});

self.addEventListener("fetch", function (event) {
  var req = event.request;
  if (req.method !== "GET") return; // never intercept writes
  var url = new URL(req.url);
  if (url.origin !== self.location.origin) return; // Keycloak/API: network

  // W68 G-SW1: never cache-match or cache-put a no-store request (the W63
  // gazetteer snapshot's If-None-Match/304 conditional must reach the network).
  if (req.cache === "no-store") return;
  // W68 G-SW2: API is network only (header contract above) — tenant-private
  // GETs like /api/bookings/v1/bookings must not be cached under a shared-origin URL.
  if (url.pathname.indexOf("/api/") === 0) return;

  if (req.mode === "navigate") {
    event.respondWith(
      fetch(req).catch(function () {
        return caches.match("index.html").then(function (m) { return m || Response.error(); });
      })
    );
    return;
  }

  event.respondWith(
    caches.match(req).then(function (cached) {
      if (cached) return cached;
      return fetch(req).then(function (res) {
        if (res.ok) {
          // W68 G-SW3 defense-in-depth: honor response Cache-Control: no-store.
          var cc = res.headers.get("cache-control");
          if (!cc || cc.toLowerCase().indexOf("no-store") === -1) {
            var clone = res.clone();
            caches.open(SHELL).then(function (c) { c.put(req, clone); });
          }
        }
        return res;
      });
    })
  );
});

/* ---- Background Sync flush (SPEC-W16 §4, TS-003 posture) ----
 *
 * Bearer/refresh tokens are session-only (in-memory + sessionStorage in the
 * page) and are NEVER persisted to IndexedDB or visible to this worker, so
 * the SW cannot attach Authorization when no page is open. On a sync event
 * we delegate: any open client is told to run the authenticated flush
 * itself (it holds the session tokens); if no page is open the outbox
 * stays queued — durable and non-secret — until the next page load, whose
 * boot path flushes it. */
function flushOutbox() {
  return self.clients.matchAll().then(function (cs) {
    cs.forEach(function (c) { c.postMessage("flush"); });
  });
}

self.addEventListener("sync", function (event) {
  if (event.tag === "od-field-flush") event.waitUntil(flushOutbox());
});
