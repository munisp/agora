/*
 * OpenDesk Admin service worker (SPEC-W16 §3).
 *
 * Strategy:
 *  - App shell (/, /offline, manifest, icons, Next static assets): cache-first.
 *  - /api/* (BFF proxy): network-first with a 3s timeout, offline fallback
 *    JSON when the network and cache both miss.
 *  - NEVER cached: /voice/* (LiveKit/session traffic), /webhooks/*, and the
 *    auth callbacks (/api/auth/*) — always straight to the network.
 *
 * Bump OPENDESK_SW_V on every sw.js change: it versions the cache names so
 * old caches are purged on activate (cache busting).
 */
"use strict";

const OPENDESK_SW_V = "admin-web-v2";
const SHELL_CACHE = `opendesk-shell-${OPENDESK_SW_V}`;
const RUNTIME_CACHE = `opendesk-runtime-${OPENDESK_SW_V}`;

const API_TIMEOUT_MS = 3000;

const PRECACHE_URLS = [
  "/offline",
  "/manifest.webmanifest",
  "/icons/agora-icon.svg",
];

/** Paths that must never be cached or intercepted with fallbacks. */
function isNeverCache(pathname) {
  return (
    pathname.startsWith("/voice") ||
    pathname.startsWith("/webhooks") ||
    pathname.startsWith("/api/auth")
  );
}

function isApi(pathname) {
  return pathname.startsWith("/api/");
}

function offlineJson() {
  return new Response(
    JSON.stringify({
      error: "offline",
      message:
        "You are offline and this data is not cached. Reconnect and try again.",
    }),
    {
      status: 503,
      headers: { "content-type": "application/json", "cache-control": "no-store" },
    },
  );
}

/** fetch() that rejects after ms (works around missing AbortSignal.timeout). */
function fetchWithTimeout(request, ms) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), ms);
  return fetch(request, { signal: controller.signal }).finally(() =>
    clearTimeout(timer),
  );
}

async function networkFirstApi(request) {
  try {
    const response = await fetchWithTimeout(request, API_TIMEOUT_MS);
    // Cache successful GET API responses as a best-effort offline read cache.
    if (request.method === "GET" && response.ok) {
      const cache = await caches.open(RUNTIME_CACHE);
      cache.put(request, response.clone());
    }
    return response;
  } catch (err) {
    const cached = await caches.match(request);
    return cached || offlineJson();
  }
}

async function cacheFirst(request) {
  const cached = await caches.match(request);
  if (cached) return cached;
  try {
    const response = await fetch(request);
    if (response.ok) {
      const cache = await caches.open(SHELL_CACHE);
      cache.put(request, response.clone());
    }
    return response;
  } catch (err) {
    return cached || Response.error();
  }
}

async function navigation(request) {
  try {
    const response = await fetch(request);
    if (response.ok) {
      const cache = await caches.open(RUNTIME_CACHE);
      cache.put(request, response.clone());
    }
    return response;
  } catch (err) {
    const cached =
      (await caches.match(request)) || (await caches.match("/offline"));
    return cached || Response.error();
  }
}

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(SHELL_CACHE)
      .then((cache) => cache.addAll(PRECACHE_URLS))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    (async () => {
      const keep = new Set([SHELL_CACHE, RUNTIME_CACHE]);
      const names = await caches.keys();
      await Promise.all(
        names
          .filter((name) => name.startsWith("opendesk-") && !keep.has(name))
          .map((name) => caches.delete(name)),
      );
      await self.clients.claim();
    })(),
  );
});

self.addEventListener("fetch", (event) => {
  const { request } = event;
  if (request.method !== "GET") return; // never intercept writes

  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return; // cross-origin: network

  const path = url.pathname;

  if (isNeverCache(path)) return; // straight to network, never cached

  if (isApi(path)) {
    event.respondWith(networkFirstApi(request));
    return;
  }

  if (request.mode === "navigate") {
    event.respondWith(navigation(request));
    return;
  }

  // Static app-shell assets (/_next/static, /icons, /manifest…): cache-first.
  event.respondWith(cacheFirst(request));
});
