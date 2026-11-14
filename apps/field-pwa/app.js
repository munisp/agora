/* OpenDesk Field — dependency-free field PWA (SPEC-W16 §4, Agent C).
 *
 * Features: tenant slug + Keycloak PKCE sign-in (real auth-code flow against
 * the realm's public client), lead capture with consented GPS attach, geo
 * check-in, IndexedDB offline outbox, flush on 'online' + Background Sync.
 *
 * Sync target: POST {apiBase}/v1/field/capture (booking-service, Agent B
 * contract) with {items:[{client_id, kind, payload, captured_at, gps}]};
 * the server dedupes on client_id (field_capture:{uuid}).
 */
"use strict";

/* ---------------- config & storage ---------------- */

var LS = {
  slug: "od.field.slug",
  cfg: "od.field.cfg", // legacy key — scrubbed below (SPEC-W45 K25)
  tokens: "od.field.tokens",
  mode: "od.field.mode", // "live" | "demo" | null
};

/* SPEC-W45 K25: endpoints are BUILD-TIME constants, not user-editable. A
 * deploy may inject window.__FIELD_CONFIG__ (e.g. a templated config script
 * served before app.js) to pin the issuer/apiBase for the environment; the
 * Keycloak client is always the dedicated public client `opendesk-field`
 * (never the admin-web client, and never overridable at runtime). The old
 * "Server settings" override UI was removed — a field device must not be
 * repointable at an attacker-controlled issuer/API by typing into the page.
 */
var BUILD_CONFIG =
  (typeof window !== "undefined" && window.__FIELD_CONFIG__) || {};

var DEFAULTS = {
  issuer: BUILD_CONFIG.issuer || "http://localhost:8080/realms/opendesk",
  clientId: "opendesk-field",
  apiBase: BUILD_CONFIG.apiBase || "/api/bookings", // APISIX: /api/bookings/* -> booking-service
  // SPEC-W63: geo-service rides the SAME APISIX pattern as api-bookings
  // (route `api-geo`: /api/geo/* -> geo-service, identical openid-connect
  // bearer_only config — RECON G2). Same-origin, derived exactly like apiBase.
  geoBase: BUILD_CONFIG.geoBase || "/api/geo", // APISIX: /api/geo/* -> geo-service
};

function lsGet(k) { try { return localStorage.getItem(k); } catch (e) { return null; } }
function lsSet(k, v) { try { localStorage.setItem(k, v); } catch (e) {} }
function lsDel(k) { try { localStorage.removeItem(k); } catch (e) {} }

function cfg() {
  return {
    issuer: DEFAULTS.issuer,
    clientId: DEFAULTS.clientId,
    apiBase: DEFAULTS.apiBase,
    geoBase: DEFAULTS.geoBase,
  };
}
/* One-time scrub of any pre-K25 user-supplied endpoint override. */
lsDel(LS.cfg);

/* Token storage posture (TS-003, mission-critical assurance): bearer and
 * refresh tokens live ONLY in this in-memory variable, mirrored into
 * sessionStorage so a page reload within the same tab session stays signed
 * in. They are NEVER written to localStorage or IndexedDB — both survive
 * logout and device loss/theft on shared field devices. Closing the tab
 * ends the session; logout clears everything immediately. IndexedDB keeps
 * only non-secret app data (the offline outbox and the slug/cfg/mode
 * context mirror below). */
var memTokens = null;
function tokens() {
  if (memTokens) return memTokens;
  try { memTokens = JSON.parse(sessionStorage.getItem(LS.tokens) || "null"); } catch (e) { memTokens = null; }
  return memTokens;
}
function saveTokens(t) {
  memTokens = t || null;
  try {
    if (t) sessionStorage.setItem(LS.tokens, JSON.stringify(t)); else sessionStorage.removeItem(LS.tokens);
  } catch (e) {}
  syncMeta();
}
/* One-time migration scrub: purge any token copy persisted by pre-TS-003
 * builds in localStorage (and the IDB meta mirror is overwritten without
 * tokens by syncMeta() on boot). */
lsDel(LS.tokens);
function mode() { return lsGet(LS.mode); }
function setMode(m) { if (m) lsSet(LS.mode, m); else lsDel(LS.mode); renderMode(); }

/* ---------------- IndexedDB outbox ---------------- */

function openDb() {
  return new Promise(function (resolve, reject) {
    // SPEC-W57 B2: v1 -> v2 adds the `receipts` store ONLY — existing
    // outbox/meta data is untouched by the upgrade.
    // SPEC-W63: v2 -> v3 adds the `gazetteer` store ONLY (offline copy of
    // the geo-service reference gazetteer: states + LGAs; etag lives in
    // `meta`). Every store is behind its own objectStoreNames guard, so the
    // upgrade is additive-only from ANY prior version (fresh v3, v1->v3 and
    // v2->v3 all converge on the same schema without touching existing data).
    var req = indexedDB.open("opendesk-field", 3);
    req.onupgradeneeded = function () {
      var db = req.result;
      if (!db.objectStoreNames.contains("outbox")) {
        db.createObjectStore("outbox", { keyPath: "id" });
      }
      if (!db.objectStoreNames.contains("meta")) {
        db.createObjectStore("meta", { keyPath: "k" });
      }
      if (!db.objectStoreNames.contains("receipts")) {
        // Out-of-line keys: outbox item id -> photo Blob. The photo is
        // non-secret business data; tokens never touch IDB (TS-003, B6).
        db.createObjectStore("receipts");
      }
      if (!db.objectStoreNames.contains("gazetteer")) {
        // Public reference geography (state/lga rows keyed by their stable
        // gazetteer `code`, e.g. NG-LAGOS / NG-LAGOS-IKEJA) — non-secret,
        // fully replaceable from the server snapshot.
        var gs = db.createObjectStore("gazetteer", { keyPath: "code" });
        gs.createIndex("kind", "kind", { unique: false });
        gs.createIndex("parent_code", "parent_code", { unique: false });
      }
    };
    req.onsuccess = function () { resolve(req.result); };
    req.onerror = function () { reject(req.error); };
  });
}

function tx(db, store, m, fn) {
  return new Promise(function (resolve, reject) {
    var t = db.transaction(store, m);
    var out = fn(t.objectStore(store));
    t.oncomplete = function () { resolve(out && out.result !== undefined ? out.result : out); };
    t.onerror = function () { reject(t.error); };
    t.onabort = function () { reject(t.error); };
  });
}

function outboxAll() {
  return openDb().then(function (db) {
    return new Promise(function (resolve, reject) {
      var req = db.transaction("outbox").objectStore("outbox").getAll();
      req.onsuccess = function () { resolve(req.result || []); };
      req.onerror = function () { reject(req.error); };
    });
  });
}
function outboxPut(item) {
  return openDb().then(function (db) {
    return tx(db, "outbox", "readwrite", function (s) { s.put(item); });
  });
}
function outboxDel(id) {
  return openDb().then(function (db) {
    return tx(db, "outbox", "readwrite", function (s) { s.delete(id); });
  });
}

/* SPEC-W57 B2: photo blobs for queued payment_receipt items, keyed by the
 * SAME outbox item id (one photo per client_id — P5). */
function receiptPut(id, blob) {
  return openDb().then(function (db) {
    return tx(db, "receipts", "readwrite", function (s) { s.put(blob, id); });
  });
}
function receiptGet(id) {
  return openDb().then(function (db) {
    return new Promise(function (resolve, reject) {
      var req = db.transaction("receipts").objectStore("receipts").get(id);
      req.onsuccess = function () { resolve(req.result || null); };
      req.onerror = function () { reject(req.error); };
    });
  }).catch(function () { return null; }); // unreadable store => treat as "no photo"
}
function receiptDel(id) {
  return openDb().then(function (db) {
    return tx(db, "receipts", "readwrite", function (s) { s.delete(id); });
  }).catch(function () {}); // discarding stays safe even if the store is absent
}
function receiptKeys() {
  return openDb().then(function (db) {
    return new Promise(function (resolve, reject) {
      var req = db.transaction("receipts").objectStore("receipts").getAllKeys();
      req.onsuccess = function () { resolve(req.result || []); };
      req.onerror = function () { reject(req.error); };
    });
  }).catch(function () { return []; });
}

/* Mirror NON-SECRET context (tenant slug, endpoints config, mode) into IDB
 * so the service worker knows the outbox target. Tokens are deliberately
 * NOT mirrored (TS-003 posture above): the SW cannot attach Authorization
 * on its own, so on a Background Sync event it asks an open page to run
 * the authenticated flush; with no page open the outbox stays queued
 * (durable, non-secret) until the next page load flushes it. */
function syncMeta() {
  openDb().then(function (db) {
    return tx(db, "meta", "readwrite", function (s) {
      s.put({ k: "ctx", slug: lsGet(LS.slug), cfg: cfg(), mode: mode() });
    });
  }).catch(function () {});
}

function metaGet(k) {
  return openDb().then(function (db) {
    return new Promise(function (resolve, reject) {
      var req = db.transaction("meta").objectStore("meta").get(k);
      req.onsuccess = function () { resolve(req.result || null); };
      req.onerror = function () { reject(req.error); };
    });
  });
}
function metaPut(obj) {
  return openDb().then(function (db) {
    return tx(db, "meta", "readwrite", function (s) { s.put(obj); });
  });
}

/* ---------------- PKCE auth (Keycloak public client) ---------------- */

function b64url(bytes) {
  var s = "";
  for (var i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function redirectUri() { return location.origin + location.pathname; }

function startLogin() {
  var slug = document.getElementById("f-slug").value.trim();
  if (!slug) { authMsg("Enter your tenant slug first."); return; }
  var c = cfg(); // pinned build-time endpoints (K25)
  lsSet(LS.slug, slug);

  var verifier = b64url(crypto.getRandomValues(new Uint8Array(32)));
  var state = b64url(crypto.getRandomValues(new Uint8Array(16)));
  sessionStorage.setItem("od.field.pkce", JSON.stringify({ verifier: verifier, state: state }));

  crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier)).then(function (hash) {
    var q = new URLSearchParams({
      client_id: c.clientId,
      redirect_uri: redirectUri(),
      response_type: "code",
      scope: "openid profile email",
      state: state,
      code_challenge: b64url(new Uint8Array(hash)),
      code_challenge_method: "S256",
    });
    location.href = c.issuer.replace(/\/+$/, "") + "/protocol/openid-connect/auth?" + q.toString();
  });
}

function handleCallback() {
  var q = new URLSearchParams(location.search);
  var code = q.get("code"), state = q.get("state");
  if (!code) return Promise.resolve(false);
  var pkce = {};
  try { pkce = JSON.parse(sessionStorage.getItem("od.field.pkce") || "{}"); } catch (e) {}
  sessionStorage.removeItem("od.field.pkce");
  history.replaceState(null, "", location.pathname);
  if (!pkce.verifier || pkce.state !== state) {
    authMsg("Sign-in state mismatch — please try again.");
    return Promise.resolve(true);
  }
  var c = cfg();
  return fetch(c.issuer.replace(/\/+$/, "") + "/protocol/openid-connect/token", {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "authorization_code",
      client_id: c.clientId,
      code: code,
      redirect_uri: redirectUri(),
      code_verifier: pkce.verifier,
    }),
  }).then(function (res) {
    return res.json().then(function (body) {
      if (!res.ok) throw new Error(body.error_description || body.error || ("HTTP " + res.status));
      saveTokens({
        access_token: body.access_token,
        refresh_token: body.refresh_token || null,
        expires_at: Date.now() + (body.expires_in || 300) * 1000,
      });
      setMode("live");
      toast("Signed in.");
    });
  }).catch(function (err) {
    authMsg("Sign-in failed: " + err.message);
  }).then(function () { return true; });
}

function refreshIfNeeded() {
  var t = tokens();
  if (!t) return Promise.resolve(null);
  if (t.expires_at - 30000 > Date.now()) return Promise.resolve(t);
  if (!t.refresh_token) { saveTokens(null); return Promise.resolve(null); }
  var c = cfg();
  return fetch(c.issuer.replace(/\/+$/, "") + "/protocol/openid-connect/token", {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "refresh_token",
      client_id: c.clientId,
      refresh_token: t.refresh_token,
    }),
  }).then(function (res) {
    return res.json().then(function (body) {
      if (!res.ok) throw new Error(body.error || ("HTTP " + res.status));
      saveTokens({
        access_token: body.access_token,
        refresh_token: body.refresh_token || t.refresh_token,
        expires_at: Date.now() + (body.expires_in || 300) * 1000,
      });
      return tokens();
    });
  }).catch(function () {
    saveTokens(null);
    return null;
  });
}

function logout() {
  saveTokens(null);
  setMode(null);
  lsDel(LS.slug);
  render();
}

/* ---------------- GPS ---------------- */

function getGps() {
  return new Promise(function (resolve) {
    if (!("geolocation" in navigator)) return resolve(null);
    navigator.geolocation.getCurrentPosition(
      function (p) {
        resolve({ lat: p.coords.latitude, lng: p.coords.longitude, accuracy: p.coords.accuracy });
      },
      function () { resolve(null); },
      { enableHighAccuracy: false, timeout: 8000, maximumAge: 60000 }
    );
  });
}

/* ---------------- capture & queue ---------------- */

/* Resolves the queued item (callers like captureReceipt need its id to key
 * sidecar data). opts.deferFlush skips the immediate best-effort flush so a
 * caller can finish staging sidecar data first (P5: a receipt photo must be
 * stashed BEFORE the anchor can sync, or the ack path would find no blob). */
function enqueue(kind, payload, gps, opts) {
  var item = {
    id: crypto.randomUUID(),
    kind: kind,
    payload: payload,
    captured_at: new Date().toISOString(),
    gps: gps || null,
    status: "queued",
  };
  return outboxPut(item).then(function () {
    renderOutbox();
    registerBgSync();
    if (!(opts && opts.deferFlush)) flush(); // best effort; failures stay queued
    return item;
  });
}

/* UIMOB-02 (SPEC-W47): the server LeadCapturePayload REQUIRES phone_e164
 * (fieldcapture/service.go — "phone_e164 is required" → per-item error,
 * retried forever). Normalize the typed number to E.164 client-side and
 * refuse to enqueue without one (a name-only capture can never apply).
 * Normalization is conservative: strip separators, upgrade a leading
 * international "00" to "+"; anything else must already be +<country><n>. */
function normalizeE164(raw) {
  var s = String(raw || "").trim().replace(/[\s().\-/]/g, "");
  if (s.indexOf("00") === 0) s = "+" + s.slice(2);
  return /^\+[1-9]\d{6,14}$/.test(s) ? s : null;
}

function captureLead() {
  var name = document.getElementById("f-name").value.trim();
  var phoneRaw = document.getElementById("f-phone").value;
  var notes = document.getElementById("f-notes").value.trim();
  var phoneE164 = normalizeE164(phoneRaw);
  if (!phoneE164) {
    toast("Phone is required in international format, e.g. +2348012345678.");
    return;
  }
  var wantGps = document.getElementById("f-gps").checked;
  (wantGps ? getGps() : Promise.resolve(null)).then(function (gps) {
    if (wantGps && !gps) toast("Location unavailable — saved without it.");
    // `phone` rides along for backward compatibility with older readers of
    // the outbox/anchor payload; the server only consumes phone_e164.
    var payload = { name: name, phone_e164: phoneE164, phone: phoneE164, notes: notes };
    // SPEC-W63: optional gazetteer place names (state/lga as selected).
    // R3 spike: fieldcapture decodes the payload with encoding/json into
    // LeadCapturePayload — unknown keys are ignored, and the raw payload is
    // preserved verbatim on the field_captures anchor row (JSONB) — so new
    // keys are additive and safe. lga_id is deliberately NOT wired (it
    // points into the synthetic cac gazetteer).
    var place = selectedPlace();
    if (place.state) payload.state = place.state;
    if (place.lga) payload.lga = place.lga;
    return enqueue("lead_capture", payload, gps).then(function () {
      document.getElementById("f-name").value = "";
      document.getElementById("f-phone").value = "";
      document.getElementById("f-notes").value = "";
      resetPlaceFields();
      toast("Lead queued" + (navigator.onLine ? " — syncing…" : " (offline)."));
    });
  });
}

function checkin() {
  toast("Locating…");
  getGps().then(function (gps) {
    if (!gps) { toast("Location unavailable — check-in not recorded."); return; }
    return enqueue("checkin", {}, gps).then(function () {
      toast("Check-in queued (±" + Math.round(gps.accuracy) + "m).");
    });
  });
}

/* ---------------- payment receipt (SPEC-W57 Scope B) ---------------- */

/* Client-side mirror of the server's receipt mime allow-list
 * (fieldcapture/receipt.go): a deterministic 415 should be caught before
 * enqueue, not discovered after the bytes upload. */
var RECEIPT_MIMES = { "image/jpeg": 1, "image/png": 1, "image/webp": 1 };

/* B2: GET the caller's own bookings (mine=true, ±7 days) for the picker,
 * with the SAME Bearer + x-tenant-slug headers as flushChunk. 403 means the
 * caller is authenticated but matches no team member of this tenant — the
 * picker is disabled with an honest message and an unlinked receipt stays
 * legal. payment_status only arrives with the W57 server build, so it is
 * read defensively and the badge omitted when absent. */
function loadMyBookings() {
  var sel = $("f-receipt-booking"), msg = $("receipt-booking-msg");
  if (mode() !== "live") {
    msg.textContent = "Sign in to load your bookings — you can still save an unlinked receipt.";
    return;
  }
  msg.textContent = "Loading…";
  refreshIfNeeded().then(function (t) {
    if (!t) { msg.textContent = "Session expired — sign in again to load bookings."; return; }
    var day = 24 * 3600 * 1000, now = Date.now();
    var q = new URLSearchParams({
      mine: "true",
      from: new Date(now - 7 * day).toISOString(),
      to: new Date(now + 7 * day).toISOString(),
    });
    return fetch(cfg().apiBase.replace(/\/+$/, "") + "/v1/bookings?" + q.toString(), {
      headers: {
        authorization: "Bearer " + t.access_token,
        "x-tenant-slug": lsGet(LS.slug) || "",
      },
    }).then(function (res) {
      if (res.status === 403) {
        sel.disabled = true;
        msg.textContent = "Your account is not linked to a team member of this tenant, so your bookings cannot be listed. You can still save an unlinked receipt.";
        return;
      }
      if (!res.ok) throw new Error("HTTP " + res.status);
      return res.json().then(function (body) {
        var list = body && Array.isArray(body.bookings) ? body.bookings : [];
        sel.length = 1; // keep only the "unlinked" placeholder option
        list.forEach(function (b) {
          var opt = document.createElement("option");
          opt.value = b.id;
          var when = b.starts_at ? new Date(b.starts_at).toLocaleString() : "";
          var badge = b.payment_status ? " · " + b.payment_status : "";
          opt.textContent = (b.public_ref || b.id) + (when ? " · " + when : "") + badge;
          sel.appendChild(opt);
        });
        msg.textContent = list.length ? "" : "No bookings of yours within the past/next 7 days.";
      });
    }).catch(function (err) {
      msg.textContent = "Could not load bookings (" + err.message + ") — you can still save an unlinked receipt.";
    });
  });
}

/* B2/B3: one photo per client_id (P5) — every submit creates a NEW capture
 * item with a fresh id, never reuses one. The photo blob is stashed in the
 * IDB `receipts` store under that same id BEFORE any flush can ack the
 * anchor (deferFlush), and uploads only after the applied/deduped ack. */
function captureReceipt() {
  var fileEl = $("f-receipt-photo");
  var photo = fileEl.files && fileEl.files[0];
  if (!photo) { toast("Take or choose a receipt photo first."); return; }
  if (photo.type && !RECEIPT_MIMES[photo.type]) {
    toast("Receipt photo must be JPEG, PNG or WebP.");
    return;
  }
  var bookingId = $("f-receipt-booking").value || "";
  var note = $("f-receipt-note").value.trim();
  var payload = {};
  if (bookingId) payload.booking_id = bookingId;
  if (note) payload.note = note;
  enqueue("payment_receipt", payload, null, { deferFlush: true }).then(function (item) {
    return receiptPut(item.id, photo).catch(function (err) {
      // Photo could not be stashed — do not leave an anchor-only receipt
      // queued behind the user's back: roll the item back and say so.
      return outboxDel(item.id).then(function () { throw err; });
    });
  }).then(function () {
    fileEl.value = "";
    $("f-receipt-note").value = "";
    renderOutbox();
    flush(); // staged completely now — safe to sync
    toast("Receipt queued" + (navigator.onLine ? " — syncing…" : " (offline)."));
  }, function () {
    renderOutbox();
    toast("Could not store the photo on this device — receipt NOT queued.");
  });
}

/* ---------------- gazetteer (SPEC-W63 Scope C) ----------------
 *
 * Offline copy of the geo-service reference gazetteer (states + LGAs) in
 * the IDB `gazetteer` store; the snapshot ETag rides in `meta` under
 * GAZ_ETAG_KEY. The download runs ONLY in page context (boot + 'online'
 * event): the SW holds no tokens and cannot attach Authorization (TS-003),
 * and the snapshot must NOT be replayed from the SW cache (R4), hence
 * cache:"no-store" on the fetch. Posture is fail-open at every step — a
 * sync failure keeps the existing local copy and never blocks capture.
 */
var GAZ_ETAG_KEY = "gazetteer_etag";

function byName(a, b) { return a.name < b.name ? -1 : (a.name > b.name ? 1 : 0); }

/* Rows read through the `kind` / `parent_code` indexes; an unreadable or
 * absent store degrades to [] (callers then show the free-text fallback). */
function gazetteerByKind(kind) {
  return openDb().then(function (db) {
    return new Promise(function (resolve, reject) {
      var req = db.transaction("gazetteer").objectStore("gazetteer")
        .index("kind").getAll(kind);
      req.onsuccess = function () { resolve(req.result || []); };
      req.onerror = function () { reject(req.error); };
    });
  }).catch(function () { return []; });
}
function gazetteerChildren(parentCode) {
  return openDb().then(function (db) {
    return new Promise(function (resolve, reject) {
      var req = db.transaction("gazetteer").objectStore("gazetteer")
        .index("parent_code").getAll(parentCode);
      req.onsuccess = function () { resolve(req.result || []); };
      req.onerror = function () { reject(req.error); };
    });
  }).catch(function () { return []; });
}

/* Full-snapshot replace in ONE transaction: clear + bulk put, so a reader
 * never sees a half-swapped gazetteer. */
function gazetteerReplace(rows) {
  return openDb().then(function (db) {
    return new Promise(function (resolve, reject) {
      var t = db.transaction("gazetteer", "readwrite");
      var s = t.objectStore("gazetteer");
      s.clear();
      rows.forEach(function (r) { s.put(r); });
      t.oncomplete = function () { resolve(rows.length); };
      t.onerror = function () { reject(t.error); };
      t.onabort = function () { reject(t.error); };
    });
  });
}

/* The snapshot body's envelope is consumed defensively (bare array, or
 * places/items/rows array property) and every row is validated — the store
 * keyPath is `code`, so a malformed row would abort the whole replace tx. */
function gazetteerRows(body) {
  var rows = Array.isArray(body) ? body
    : (body && (body.places || body.items || body.rows)) || [];
  var out = [];
  rows.forEach(function (r) {
    if (r && typeof r.code === "string" && r.code &&
        (r.kind === "state" || r.kind === "lga" || r.kind === "locality") &&
        typeof r.name === "string" && r.name) {
      out.push({
        code: r.code,
        kind: r.kind,
        name: r.name,
        parent_code: typeof r.parent_code === "string" ? r.parent_code : null,
      });
    }
  });
  return out;
}

/* Boot/'online' sync: conditional GET {geoBase}/v1/geo/gazetteer/snapshot
 * with the stored etag. 304 => keep the local copy; 200 => one-tx replace +
 * save the new etag; anything else (offline, 401/403, 5xx, parse error) =>
 * keep the existing copy and resolve false. Never rejects, never blocks. */
function syncGazetteer() {
  if (mode() !== "live") return Promise.resolve(false); // demo: no token
  return refreshIfNeeded().then(function (t) {
    if (!t) return false;
    return metaGet(GAZ_ETAG_KEY).then(function (cur) {
      var headers = {
        authorization: "Bearer " + t.access_token,
        "x-tenant-slug": lsGet(LS.slug) || "",
      };
      if (cur && cur.v) headers["if-none-match"] = cur.v;
      return fetch(cfg().geoBase.replace(/\/+$/, "") + "/v1/geo/gazetteer/snapshot?levels=state,lga", {
        headers: headers,
        cache: "no-store", // R4: never serve the snapshot from cache layers
      }).then(function (res) {
        if (res.status === 304) return "unchanged";
        if (!res.ok) throw new Error("HTTP " + res.status);
        var etag = res.headers.get("ETag") || null;
        return res.json().then(function (body) {
          var rows = gazetteerRows(body);
          return gazetteerReplace(rows).then(function () {
            return etag ? metaPut({ k: GAZ_ETAG_KEY, v: etag }) : undefined;
          }).then(function () { return "updated"; });
        });
      });
    });
  }).then(function (outcome) {
    renderPlaceFields(); // reflect the freshest local copy
    return outcome;
  }, function () {
    return false; // fail-open: keep the existing copy
  });
}

/* Cascading State -> LGA selects on the lead form, populated from the IDB
 * gazetteer store. With no snapshot on this device the free-text fallback
 * shows instead — capture is NEVER blocked by a missing gazetteer. */
function renderPlaceFields() {
  var selWrap = $("f-place-selects"), txtWrap = $("f-place-freetext");
  if (!selWrap || !txtWrap) return;
  gazetteerByKind("state").then(function (states) {
    if (!states.length) {
      selWrap.classList.add("hidden");
      txtWrap.classList.remove("hidden");
      return;
    }
    var sel = $("f-state");
    var cur = sel.value;
    sel.length = 1; // keep the placeholder option
    states.sort(byName).forEach(function (s) {
      var opt = document.createElement("option");
      opt.value = s.code;
      opt.textContent = s.name;
      opt.setAttribute("data-name", s.name);
      sel.appendChild(opt);
    });
    sel.value = cur; // reselect if the code is still present
    txtWrap.classList.add("hidden");
    selWrap.classList.remove("hidden");
    renderLgaOptions();
  });
}

function renderLgaOptions() {
  var stateSel = $("f-state"), lgaSel = $("f-lga");
  var code = stateSel.value, cur = lgaSel.value;
  lgaSel.length = 1;
  lgaSel.disabled = !code;
  if (!code) return;
  gazetteerChildren(code).then(function (lgas) {
    lgas.sort(byName).forEach(function (l) {
      var opt = document.createElement("option");
      opt.value = l.code;
      opt.textContent = l.name;
      opt.setAttribute("data-name", l.name);
      lgaSel.appendChild(opt);
    });
    lgaSel.value = cur;
  });
}

/* The place as the user expressed it: NAMES (not codes) from the cascade
 * when the snapshot is present, free text otherwise. Keys are omitted when
 * empty so the payload stays additive-only. */
function selectedPlace() {
  var out = {};
  var selWrap = $("f-place-selects");
  if (selWrap && !selWrap.classList.contains("hidden")) {
    var si = $("f-state").selectedIndex, li = $("f-lga").selectedIndex;
    if ($("f-state").value && si >= 0) {
      out.state = $("f-state").options[si].getAttribute("data-name") || $("f-state").options[si].textContent;
    }
    if ($("f-lga").value && li >= 0) {
      out.lga = $("f-lga").options[li].getAttribute("data-name") || $("f-lga").options[li].textContent;
    }
  } else {
    var st = $("f-state-text").value.trim(), lg = $("f-lga-text").value.trim();
    if (st) out.state = st;
    if (lg) out.lga = lg;
  }
  return out;
}

function resetPlaceFields() {
  $("f-state").value = "";
  renderLgaOptions();
  $("f-state-text").value = "";
  $("f-lga-text").value = "";
}

/* ---------------- sync ---------------- */

var flushing = false;

/* FW-1 (SPEC-W46): bounded flush requests and per-batch failure handling.
 * Previously the ENTIRE outbox went out as one POST with no timeout: one
 * poison item (or one huge queue) failed/marked every item, and a dead
 * network hung the flush until the OS gave up. Now: chunks of
 * FLUSH_CHUNK items, each fetch bounded by FETCH_TIMEOUT_MS, and a failed
 * chunk keeps ONLY its own items queued while the rest still sync. */
var FLUSH_CHUNK = 50; // server batch cap is 100 — 50 stays well under body limits
var FETCH_TIMEOUT_MS = 15000;

/* fetch bounded by FETCH_TIMEOUT_MS; rejects with Error("timed out…"). */
function fetchWithTimeout(url, opts) {
  var ctrl = new AbortController();
  var timer = setTimeout(function () { ctrl.abort(); }, FETCH_TIMEOUT_MS);
  opts = opts || {};
  opts.signal = ctrl.signal;
  return fetch(url, opts).then(function (res) {
    clearTimeout(timer);
    return res;
  }, function (err) {
    clearTimeout(timer);
    if (ctrl.signal.aborted) throw new Error("request timed out after " + (FETCH_TIMEOUT_MS / 1000) + "s");
    throw err;
  });
}

/* SPEC-W57 P5/R8: the receipt upload gets its OWN ~180s timeout — OCR
 * extraction can take up to ~150s server-side and the route is exempt from
 * the 60s middleware, so the 15s flush budget (FETCH_TIMEOUT_MS) cannot
 * cover it. This fetch is deliberately NOT part of flushChunk's budget. */
var RECEIPT_TIMEOUT_MS = 180000;

/* POST the photo bytes to the capture anchor
 * ({apiBase}/v1/field/captures/{client_id}/receipt) with the same auth
 * headers as flushChunk. Resolves an outcome object, never rejects for an
 * expected HTTP/network outcome:
 *   {ok:true, amount} — 200: bytes stored + OCR fields extracted
 *   {ok:false, status:"receipt upload pending (…)"} — transient; retry
 *   {ok:false, status:"failed: …"} — deterministic; user may discard
 * Retry is safe: AttachReceipt is an unconditional UPDATE keyed by the same
 * client_id, so re-POSTing the same bytes overwrites, never duplicates. */
function uploadReceipt(item, blob, accessToken) {
  var ctrl = new AbortController();
  var timer = setTimeout(function () { ctrl.abort(); }, RECEIPT_TIMEOUT_MS);
  var url = cfg().apiBase.replace(/\/+$/, "") + "/v1/field/captures/" +
    encodeURIComponent(item.id) + "/receipt";
  return fetch(url, {
    method: "POST",
    headers: {
      "content-type": blob.type || "application/octet-stream",
      authorization: "Bearer " + accessToken,
      "x-tenant-slug": lsGet(LS.slug) || "",
    },
    body: blob,
    signal: ctrl.signal,
  }).then(function (res) {
    clearTimeout(timer);
    if (res.status === 200) {
      return res.json().catch(function () { return null; }).then(function (body) {
        var ef = body && body.extracted_fields;
        var amount = ef && ef.amount != null && ef.amount !== ""
          ? String(ef.amount) + (ef.currency ? " " + ef.currency : "")
          : null;
        return { ok: true, amount: amount };
      });
    }
    if (res.status === 415) {
      return { ok: false, status: "failed: receipt photo rejected (HTTP 415 unsupported media) — discard or retake" };
    }
    if (res.status === 404) {
      // Anchor race: the ack said applied/deduped yet the anchor is missing.
      // Treat as failed and keep BOTH queued — the retry reports 404 again
      // honestly rather than dropping anything.
      return { ok: false, status: "failed: capture anchor missing on server (HTTP 404)" };
    }
    if (res.status === 401 || res.status === 403) {
      // Session problem: keep everything queued; the next flush's capture
      // fetch hits the same status and runs the inherited auth-failure path.
      return { ok: false, status: "receipt upload pending (HTTP " + res.status + " — sign in again)" };
    }
    // 502/503/other transient: keep BOTH item and blob queued.
    return { ok: false, status: "receipt upload pending (HTTP " + res.status + ")" };
  }, function (err) {
    clearTimeout(timer);
    var why = ctrl.signal.aborted
      ? "timed out after " + (RECEIPT_TIMEOUT_MS / 1000) + "s"
      : (err && err.message ? err.message : "network error");
    return { ok: false, status: "receipt upload pending (" + why + ")" };
  });
}

/* P5 two-phase sequencing: invoked ONLY after the server acked this item
 * applied/deduped (or accepted its whole batch). A payment_receipt item
 * with a stashed photo uploads its bytes FIRST; the outbox entry is removed
 * only after a 200. Resolves true when the item was removed, false when it
 * stays queued with an honest status. Never rejects for an expected
 * outcome. */
function settleAppliedItem(item, accessToken) {
  if (item.kind !== "payment_receipt") {
    return outboxDel(item.id).then(function () { return true; });
  }
  return receiptGet(item.id).then(function (blob) {
    if (!blob) {
      // No photo stashed on this device (captured by a pre-W57 build, or
      // site data was partially cleared): nothing left to upload — the
      // anchor is durable server-side, so remove the queue entry.
      return outboxDel(item.id).then(function () { return true; });
    }
    return uploadReceipt(item, blob, accessToken).then(function (outcome) {
      if (outcome.ok) {
        return receiptDel(item.id).then(function () { return outboxDel(item.id); }).then(function () {
          toast("Receipt uploaded" + (outcome.amount ? " — extracted " + outcome.amount : "") + ".");
          return true;
        });
      }
      item.status = outcome.status; // keep BOTH item and blob queued
      return outboxPut(item).then(function () { return false; });
    });
  });
}

/* One chunk flush. Resolves {delivered, rejected, authFailed} — items are
 * removed ONLY on a positive per-item (or whole-batch) acknowledgement;
 * anything else stays queued with an honest "failed: …" status. On a
 * network/timeout/5xx failure the chunk's items are marked failed and the
 * error is rethrown so the caller can decide whether to continue. */
function flushChunk(chunk, accessToken) {
  var batchId = crypto.randomUUID();
  return fetchWithTimeout(cfg().apiBase.replace(/\/+$/, "") + "/v1/field/capture", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      authorization: "Bearer " + accessToken,
      "x-tenant-slug": lsGet(LS.slug) || "",
      "idempotency-key": "field_capture:" + batchId,
    },
    body: JSON.stringify({
      batch_id: batchId,
      items: chunk.map(function (i) {
        return { client_id: i.id, kind: i.kind, payload: i.payload, captured_at: i.captured_at, gps: i.gps };
      }),
    }),
  }).then(function (res) {
    if (res.status === 401 || res.status === 403) {
      var authErr = new Error("HTTP " + res.status);
      authErr.authFailed = true;
      authErr.status = res.status;
      throw authErr;
    }
    if (!res.ok) throw new Error("HTTP " + res.status);
    return res.json().catch(function () { return null; }).then(function (body) {
      var results = body && body.results;
      var delivered = 0, rejected = 0;
      if (!Array.isArray(results)) {
        // No per-item detail but the batch was accepted (HTTP 200): every
        // item in this chunk was processed — remove just this chunk
        // (receipt items still upload their stashed photo FIRST, P5).
        return Promise.all(chunk.map(function (i) {
          return settleAppliedItem(i, accessToken).then(function (removed) {
            if (removed) delivered += 1; else rejected += 1;
          });
        })).then(function () { return { delivered: delivered, rejected: rejected }; });
      }
      // Per-item outcomes (fieldcapture.ItemResult): applied | deduped are
      // exactly-once successes → remove (a payment_receipt with a stashed
      // photo uploads its bytes first and is removed only on a 200 — P5);
      // error is a deterministic server rejection → keep queued with the
      // server's reason, never silently dropped.
      var byId = {};
      results.forEach(function (r) { byId[r.client_id] = r; });
      return Promise.all(chunk.map(function (i) {
        var r = byId[i.id];
        if (r && (r.status === "applied" || r.status === "deduped")) {
          return settleAppliedItem(i, accessToken).then(function (removed) {
            if (removed) delivered += 1; else rejected += 1;
          });
        }
        rejected += 1;
        i.status = "failed: " + (r && r.error ? r.error : (r ? r.status : "not acknowledged"));
        return outboxPut(i);
      })).then(function () { return { delivered: delivered, rejected: rejected }; });
    });
  }).then(null, function (err) {
    // Network/timeout/HTTP/auth failure (thrown anywhere above): this
    // chunk's items stay queued with an honest status; other chunks are
    // unaffected. (Chained rejection handler — NOT the 2nd arg of the
    // response .then — so HTTP errors raised while handling the response
    // are caught here too.)
    return Promise.all(chunk.map(function (i) {
      i.status = "failed: " + err.message;
      return outboxPut(i);
    })).then(function () { throw err; });
  });
}

function flush() {
  if (flushing) return Promise.resolve();
  if (!navigator.onLine) return Promise.resolve();
  return outboxAll().then(function (items) {
    if (!items.length) return;
    if (mode() !== "live") { renderOutbox(); return; } // demo: stays queued
    return refreshIfNeeded().then(function (t) {
      if (!t) { setMode("demo"); authMsg("Session expired — signed out to demo mode."); render(); return; }
      flushing = true;
      renderOutbox("syncing");
      // Stable capture order; the server applies items in array order.
      items.sort(function (a, b) { return a.captured_at < b.captured_at ? -1 : 1; });
      var chunks = [];
      for (var off = 0; off < items.length; off += FLUSH_CHUNK) {
        chunks.push(items.slice(off, off + FLUSH_CHUNK));
      }
      var totals = { delivered: 0, rejected: 0, failed: 0 };
      // Sequential chunks: a failed chunk marks only its own items and the
      // flush CONTINUES with the next chunk (one poison batch can no
      // longer block the whole outbox). An auth failure aborts the flush.
      var chain = Promise.resolve();
      chunks.forEach(function (chunk) {
        chain = chain.then(function () {
          return flushChunk(chunk, t.access_token).then(function (r) {
            totals.delivered += r.delivered;
            totals.rejected += r.rejected;
          }, function (err) {
            if (err && err.authFailed) throw err; // abort: session is dead
            totals.failed += chunk.length;        // chunk stays queued
          });
        });
      });
      return chain.then(function () {
        if (totals.delivered > 0) {
          var msg = "Synced " + totals.delivered + " item" + (totals.delivered > 1 ? "s" : "") + ".";
          var left = totals.rejected + totals.failed;
          if (left > 0) msg += " " + left + " still queued (not lost).";
          toast(msg);
        } else if (totals.failed > 0) {
          toast("Sync failed — " + totals.failed + " item" + (totals.failed > 1 ? "s" : "") + " stay queued.");
        }
      }, function (err) {
        if (err && err.authFailed) {
          saveTokens(null); setMode("demo");
          authMsg("Session rejected (" + err.status + ") — signed out to demo mode.");
          render();
        }
      }).then(function () {
        flushing = false;
        renderOutbox();
      });
    });
  });
}

function registerBgSync() {
  if (!("serviceWorker" in navigator)) return;
  navigator.serviceWorker.ready.then(function (reg) {
    if (reg.sync && reg.sync.register) return reg.sync.register("od-field-flush");
  }).catch(function () {});
}

/* ---------------- rendering ---------------- */

function $(id) { return document.getElementById(id); }

function toast(msg) {
  var el = $("toast");
  el.textContent = msg;
  el.classList.add("show");
  clearTimeout(toast._t);
  toast._t = setTimeout(function () { el.classList.remove("show"); }, 2600);
}

function authMsg(msg) { $("auth-msg").textContent = msg || ""; }

function renderNet() {
  var el = $("net");
  var on = navigator.onLine;
  el.textContent = on ? "online" : "offline";
  el.className = on ? "" : "off";
}

function renderMode() {
  var m = mode();
  $("mode-badge").classList.toggle("hidden", m !== "demo");
  $("live-badge").classList.toggle("hidden", m !== "live");
}

function renderOutbox(syncingLabel) {
  // B4: receipt photo presence is read alongside the outbox so a
  // payment_receipt row can honestly show "photo attached" vs "photo
  // pending" (receiptKeys degrades to [] if the store is unreadable).
  Promise.all([outboxAll(), receiptKeys()]).then(function (both) {
    var items = both[0], photoIds = {};
    both[1].forEach(function (k) { photoIds[k] = true; });
    var ul = $("outbox-list");
    $("outbox-count").textContent = items.length ? "(" + items.length + ")" : "";
    if (!items.length) { ul.innerHTML = '<li class="muted">Nothing queued.</li>'; return; }
    ul.innerHTML = "";
    items.sort(function (a, b) { return a.captured_at < b.captured_at ? -1 : 1; });
    items.forEach(function (i) {
      var li = document.createElement("li");
      var isReceipt = i.kind === "payment_receipt";
      var kind = document.createElement("span");
      kind.className = "kind" + (i.kind === "checkin" ? " checkin" : "");
      kind.textContent = i.kind === "checkin" ? "check-in" : isReceipt ? "receipt" : "lead";
      var body = document.createElement("span");
      var when = new Date(i.captured_at);
      var summary;
      if (i.kind === "checkin") {
        summary = i.gps ? i.gps.lat.toFixed(4) + ", " + i.gps.lng.toFixed(4) : "no gps";
      } else if (isReceipt) {
        summary = (i.payload && i.payload.note ? i.payload.note : "payment receipt") +
          (i.payload && i.payload.booking_id
            ? " · booking " + String(i.payload.booking_id).slice(0, 8) + "…"
            : " · unlinked") +
          (photoIds[i.id] ? " · photo attached" : " · photo pending");
      } else {
        summary = (i.payload.name || "—") +
          (i.payload.phone_e164 || i.payload.phone
            ? " · " + (i.payload.phone_e164 || i.payload.phone)
            : "");
      }
      body.textContent = summary + " · " + when.toLocaleString();
      var st = document.createElement("span");
      st.className = "st" + (i.status && i.status.indexOf("failed") === 0 ? " failed" : "");
      st.textContent = syncingLabel || (i.status === "queued" ? "queued" : i.status || "queued");
      var del = document.createElement("button");
      del.className = "del";
      del.title = "Discard";
      del.textContent = "✕";
      // Discarding a receipt also drops its stashed photo (the deterministic
      // 415 "let the user discard" path of P5) — never leaves an orphan blob.
      del.onclick = function () {
        receiptDel(i.id).then(function () { return outboxDel(i.id); }).then(renderOutbox);
      };
      li.appendChild(kind); li.appendChild(body); li.appendChild(st); li.appendChild(del);
      ul.appendChild(li);
    });
  });
}

function render() {
  var m = mode();
  $("view-auth").classList.toggle("hidden", !!m);
  $("view-main").classList.toggle("hidden", !m);
  renderMode();
  renderNet();
  if (m) renderOutbox();
  // prefill auth form
  $("f-slug").value = lsGet(LS.slug) || "";
}

/* ---------------- boot ---------------- */

function boot() {
  $("btn-login").onclick = startLogin;
  $("btn-demo").onclick = function () {
    var slug = $("f-slug").value.trim();
    if (slug) lsSet(LS.slug, slug);
    setMode("demo");
    render();
  };
  $("btn-logout").onclick = logout;
  $("btn-capture").onclick = captureLead;
  $("btn-checkin").onclick = checkin;
  $("btn-receipt").onclick = captureReceipt;
  $("btn-load-bookings").onclick = loadMyBookings;
  $("btn-sync").onclick = function () { flush().then(function () { toast(navigator.onLine ? "Sync attempted." : "Still offline."); }); };
  if ($("f-state")) $("f-state").onchange = renderLgaOptions;

  window.addEventListener("online", function () { renderNet(); flush(); syncGazetteer(); });
  window.addEventListener("offline", renderNet);

  if ("serviceWorker" in navigator) {
    navigator.serviceWorker.register("sw.js").catch(function () {});
    navigator.serviceWorker.addEventListener("message", function (ev) {
      if (ev.data === "flushed") renderOutbox();
      // Background Sync fired: the SW holds no tokens (TS-003), so the
      // authenticated flush runs here in the page session.
      if (ev.data === "flush") flush();
    });
  }

  handleCallback().then(function () {
    if (mode() === "live" && !tokens()) setMode("demo");
    syncMeta(); // rewrites the IDB ctx mirror token-free (TS-003 scrub)
    render();
    renderPlaceFields(); // from the local IDB copy — works fully offline
    syncGazetteer();     // best-effort refresh; re-renders when done
    flush();
  });
}

document.addEventListener("DOMContentLoaded", boot);
