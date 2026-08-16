/* OpenDesk Field service worker (SPEC-W16 §3/§4).
 *
 * - App shell (index, app.js, manifest, icons): cache-first.
 * - API calls (/v1/*, token endpoints): network only — writes are queued in
 *   IndexedDB by the page, never replayed from cache.
 * - Background Sync ("od-field-flush"): asks any open page to flush the
 *   IndexedDB outbox. Tokens are session-only and never reach this worker
 *   (TS-003), so the authenticated flush always runs in a page context.
 *
 * Bump OPENDESK_SW_V on every change (cache busting).
 */
"use strict";

var OPENDESK_SW_V = "field-pwa-v3";
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
          var clone = res.clone();
          caches.open(SHELL).then(function (c) { c.put(req, clone); });
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
