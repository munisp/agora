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
  cfg: "od.field.cfg",
  tokens: "od.field.tokens",
  mode: "od.field.mode", // "live" | "demo" | null
};

var DEFAULTS = {
  issuer: "http://localhost:8080/realms/opendesk",
  clientId: "admin-web",
  apiBase: "/api/bookings", // APISIX: /api/bookings/* -> booking-service
};

function lsGet(k) { try { return localStorage.getItem(k); } catch (e) { return null; } }
function lsSet(k, v) { try { localStorage.setItem(k, v); } catch (e) {} }
function lsDel(k) { try { localStorage.removeItem(k); } catch (e) {} }

function cfg() {
  var c = {};
  try { c = JSON.parse(lsGet(LS.cfg) || "{}"); } catch (e) {}
  return {
    issuer: c.issuer || DEFAULTS.issuer,
    clientId: c.clientId || DEFAULTS.clientId,
    apiBase: c.apiBase || DEFAULTS.apiBase,
  };
}
function saveCfg(c) { lsSet(LS.cfg, JSON.stringify(c)); syncMeta(); }

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
    var req = indexedDB.open("opendesk-field", 1);
    req.onupgradeneeded = function () {
      var db = req.result;
      if (!db.objectStoreNames.contains("outbox")) {
        db.createObjectStore("outbox", { keyPath: "id" });
      }
      if (!db.objectStoreNames.contains("meta")) {
        db.createObjectStore("meta", { keyPath: "k" });
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
  var c = {
    issuer: document.getElementById("f-issuer").value.trim() || DEFAULTS.issuer,
    clientId: document.getElementById("f-client").value.trim() || DEFAULTS.clientId,
    apiBase: document.getElementById("f-api").value.trim() || DEFAULTS.apiBase,
  };
  saveCfg(c);
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

function enqueue(kind, payload, gps) {
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
    flush(); // best effort; failures stay queued
  });
}

function captureLead() {
  var name = document.getElementById("f-name").value.trim();
  var phone = document.getElementById("f-phone").value.trim();
  var notes = document.getElementById("f-notes").value.trim();
  if (!name && !phone) { toast("Add at least a name or phone."); return; }
  var wantGps = document.getElementById("f-gps").checked;
  (wantGps ? getGps() : Promise.resolve(null)).then(function (gps) {
    if (wantGps && !gps) toast("Location unavailable — saved without it.");
    return enqueue("lead_capture", { name: name, phone: phone, notes: notes }, gps).then(function () {
      document.getElementById("f-name").value = "";
      document.getElementById("f-phone").value = "";
      document.getElementById("f-notes").value = "";
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

/* ---------------- sync ---------------- */

var flushing = false;

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
      var batchId = crypto.randomUUID();
      return fetch(cfg().apiBase.replace(/\/+$/, "") + "/v1/field/capture", {
        method: "POST",
        headers: {
          "content-type": "application/json",
          authorization: "Bearer " + t.access_token,
          "x-tenant-slug": lsGet(LS.slug) || "",
          "idempotency-key": "field_capture:" + batchId,
        },
        body: JSON.stringify({
          batch_id: batchId,
          items: items.map(function (i) {
            return { client_id: i.id, kind: i.kind, payload: i.payload, captured_at: i.captured_at, gps: i.gps };
          }),
        }),
      }).then(function (res) {
        if (res.status === 401 || res.status === 403) {
          saveTokens(null); setMode("demo");
          authMsg("Session rejected (" + res.status + ") — signed out to demo mode.");
          render();
          return;
        }
        if (!res.ok) throw new Error("HTTP " + res.status);
        return Promise.all(items.map(function (i) { return outboxDel(i.id); })).then(function () {
          toast("Synced " + items.length + " item" + (items.length > 1 ? "s" : "") + ".");
        });
      }).catch(function (err) {
        return Promise.all(items.map(function (i) {
          i.status = "failed: " + err.message;
          return outboxPut(i);
        }));
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
  outboxAll().then(function (items) {
    var ul = $("outbox-list");
    $("outbox-count").textContent = items.length ? "(" + items.length + ")" : "";
    if (!items.length) { ul.innerHTML = '<li class="muted">Nothing queued.</li>'; return; }
    ul.innerHTML = "";
    items.sort(function (a, b) { return a.captured_at < b.captured_at ? -1 : 1; });
    items.forEach(function (i) {
      var li = document.createElement("li");
      var kind = document.createElement("span");
      kind.className = "kind" + (i.kind === "checkin" ? " checkin" : "");
      kind.textContent = i.kind === "checkin" ? "check-in" : "lead";
      var body = document.createElement("span");
      var when = new Date(i.captured_at);
      var summary = i.kind === "checkin"
        ? (i.gps ? i.gps.lat.toFixed(4) + ", " + i.gps.lng.toFixed(4) : "no gps")
        : ((i.payload.name || "—") + (i.payload.phone ? " · " + i.payload.phone : ""));
      body.textContent = summary + " · " + when.toLocaleString();
      var st = document.createElement("span");
      st.className = "st" + (i.status && i.status.indexOf("failed") === 0 ? " failed" : "");
      st.textContent = syncingLabel || (i.status === "queued" ? "queued" : i.status || "queued");
      var del = document.createElement("button");
      del.className = "del";
      del.title = "Discard";
      del.textContent = "✕";
      del.onclick = function () { outboxDel(i.id).then(renderOutbox); };
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
  var c = cfg();
  $("f-issuer").value = c.issuer;
  $("f-client").value = c.clientId;
  $("f-api").value = c.apiBase;
}

/* ---------------- boot ---------------- */

function boot() {
  $("btn-login").onclick = startLogin;
  $("btn-demo").onclick = function () {
    var slug = $("f-slug").value.trim();
    if (slug) lsSet(LS.slug, slug);
    saveCfg({
      issuer: $("f-issuer").value.trim() || DEFAULTS.issuer,
      clientId: $("f-client").value.trim() || DEFAULTS.clientId,
      apiBase: $("f-api").value.trim() || DEFAULTS.apiBase,
    });
    setMode("demo");
    render();
  };
  $("btn-logout").onclick = logout;
  $("btn-capture").onclick = captureLead;
  $("btn-checkin").onclick = checkin;
  $("btn-sync").onclick = function () { flush().then(function () { toast(navigator.onLine ? "Sync attempted." : "Still offline."); }); };

  window.addEventListener("online", function () { renderNet(); flush(); });
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
    flush();
  });
}

document.addEventListener("DOMContentLoaded", boot);
