/*
 * OpenDesk Admin service worker (SPEC-W16 §3).
 *
 * Strategy:
 *  - App shell (/, /offline, manifest, icons, Next static assets): cache-first.
 *  - /api/* (BFF proxy): network-first with an 8s timeout and one retry,
 *    offline fallback JSON when the network and cache both miss.
 *    Responses to requests carrying credentials (Authorization header or
 *    session Cookie — the BFF attaches the token server-side, so the Cookie
 *    is the browser-side credential signal) are NEVER cached or served from
 *    cache: no stale data after mutations, no cross-user exposure on shared
 *    browsers (SPEC-W46 AW-5).
 *  - NEVER cached: /voice/* (LiveKit/session traffic), /webhooks/*, and the
 *    auth callbacks (/api/auth/*) — always straight to the network.
 *
 * Bump OPENDESK_SW_V on every sw.js change: it versions the cache names so
 * old caches are purged on activate (cache busting).
 */
"use strict";

const OPENDESK_SW_V = "admin-web-v3";
const SHELL_CACHE = `opendesk-shell-${OPENDESK_SW_V}`;
const RUNTIME_CACHE = `opendesk-runtime-${OPENDESK_SW_V}`;

// SPEC-W46 AW-6: 3s aborted slow-but-healthy gateway calls (false "offline");
// 8s + one retry tolerates loaded gateways without hanging the UI forever.
const API_TIMEOUT_MS = 8000;
const API_MAX_ATTEMPTS = 2;

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

/**
 * SPEC-W46 AW-5: a request is credentialed when it carries an Authorization
 * header or a session Cookie (browser→BFF calls authenticate via cookie; the
 * BFF attaches the bearer token upstream). Credentialed API traffic is
 * network-only: never written to and never served from the runtime cache.
 */
function isCredentialed(request) {
  return request.headers.has("authorization") || request.headers.has("cookie");
}

async function networkFirstApi(request) {
  const credentialed = isCredentialed(request);
  for (let attempt = 0; attempt < API_MAX_ATTEMPTS; attempt++) {
    try {
      const response = await fetchWithTimeout(request, API_TIMEOUT_MS);
      // Cache successful GET API responses as a best-effort offline read
      // cache — anonymous traffic only (AW-5).
      if (!credentialed && request.method === "GET" && response.ok) {
        const cache = await caches.open(RUNTIME_CACHE);
        cache.put(request, response.clone());
      }
      return response;
    } catch {
      // timeout/abort or network failure — fall through to the single retry
    }
  }
  const cached = credentialed ? undefined : await caches.match(request);
  return cached || offlineJson();
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
