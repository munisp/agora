/* OpenDesk Field service worker (SPEC-W16 §3/§4).
 *
 * - App shell (index, app.js, manifest, icons): cache-first.
 * - API calls (/v1/*, token endpoints): network only — writes are queued in
 *   IndexedDB by the page, never replayed from cache.
 * - Background Sync ("od-field-flush"): flushes the IndexedDB outbox itself
 *   when no page is open, then notifies open clients to re-render.
 *
 * Bump OPENDESK_SW_V on every change (cache busting).
 */
"use strict";

var OPENDESK_SW_V = "field-pwa-v1";
var SHELL = "opendesk-field-" + OPENDESK_SW_V;

var PRECACHE = [
  "./",
  "index.html",
  "app.js",
  "manifest.webmanifest",
  "icons/icon-192.png",
  "icons/icon-512.png",
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

/* ---- Background Sync flush (SPEC-W16 §4) ---- */

function openDb() {
  return new Promise(function (resolve, reject) {
    var req = indexedDB.open("opendesk-field", 1);
    req.onsuccess = function () { resolve(req.result); };
    req.onerror = function () { reject(req.error); };
  });
}

function idbGetAll(db, store) {
  return new Promise(function (resolve, reject) {
    var req = db.transaction(store).objectStore(store).getAll();
    req.onsuccess = function () { resolve(req.result || []); };
    req.onerror = function () { reject(req.error); };
  });
}

function idbDel(db, store, key) {
  return new Promise(function (resolve, reject) {
    var t = db.transaction(store, "readwrite");
    t.objectStore(store).delete(key);
    t.oncomplete = resolve;
    t.onerror = function () { reject(t.error); };
  });
}

function flushOutbox() {
  return openDb().then(function (db) {
    return Promise.all([idbGetAll(db, "outbox"), idbGetAll(db, "meta")]).then(function (r) {
      var items = r[0];
      var meta = null;
      r[1].forEach(function (m) { if (m.k === "ctx") meta = m; });
      if (!items.length || !meta || meta.mode !== "live" || !meta.tokens || !meta.slug) return;
      if (meta.tokens.expires_at && meta.tokens.expires_at - 30000 < Date.now()) return; // page must refresh
      var batchId = crypto.randomUUID();
      return fetch(meta.cfg.apiBase.replace(/\/+$/, "") + "/v1/field/capture", {
        method: "POST",
        headers: {
          "content-type": "application/json",
          authorization: "Bearer " + meta.tokens.access_token,
          "x-tenant-slug": meta.slug,
          "idempotency-key": "field_capture:" + batchId,
        },
        body: JSON.stringify({
          batch_id: batchId,
          items: items.map(function (i) {
            return { client_id: i.id, kind: i.kind, payload: i.payload, captured_at: i.captured_at, gps: i.gps };
          }),
        }),
      }).then(function (res) {
        if (!res.ok) throw new Error("HTTP " + res.status);
        return Promise.all(items.map(function (i) { return idbDel(db, "outbox", i.id); }));
      }).then(function () {
        return self.clients.matchAll().then(function (cs) {
          cs.forEach(function (c) { c.postMessage("flushed"); });
        });
      });
    });
  });
}

self.addEventListener("sync", function (event) {
  if (event.tag === "od-field-flush") event.waitUntil(flushOutbox());
});
