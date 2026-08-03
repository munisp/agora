"use client";

import * as React from "react";

/**
 * Agent-driven UI actions — iframe bridge (SPEC-W9 Part B3).
 *
 * The receptionist can invoke UI action tools (navigate / highlight /
 * prefill_booking). The voice runtime attaches the *server-validated*
 * actions to the chat payload: buffered replies carry `ui_actions: [...]`
 * and SSE streams emit `data: {"ui_action": {...}}` frames before `done`.
 *
 * The chat itself lives in components/chat-widget.tsx (shared with the
 * /p/ pages), so this bridge taps window.fetch PASSIVELY: responses to
 * /voice/chat are cloned and scanned for actions without disturbing the
 * widget's own parsing. Actions are then:
 *   navigate        -> forwarded to the host page via postMessage (the
 *                      embed.js loader applies its same-origin guard and
 *                      navigates the top page); standalone fallback:
 *                      navigate this frame after the same guard
 *   highlight       -> querySelector + smooth scrollIntoView + 2s terracotta
 *                      outline pulse in THIS document; forwarded to the host
 *                      page when the selector matches nothing here
 *   prefill_booking -> CustomEvent('opendesk:prefill', {detail:{offering_id}})
 *                      dispatched here AND the matching offering card is
 *                      selected (follows the booking form's existing
 *                      offering-selection click path); also forwarded
 *
 * Safety: the tap is namespaced (a second embed on the page cannot
 * double-wrap fetch), the original window.fetch is restored on unmount,
 * every action is re-validated client-side and wrapped in try/catch —
 * the widget never breaks because of an action.
 *
 * Widget GPS capture (SPEC-W11 Part D): when the host page's embed script
 * carries data-location-consent="true", embed.js requests geolocation once
 * and postMessages it here as {type:"opendesk:location", location}. The
 * fetch tap then merges it as client_location {lat,lng,accuracy} into the
 * JSON body of every /voice/chat request (additive key — the server
 * tolerates unknown keys). Request bodies without a location fix (or
 * without consent) pass through byte-identical.
 *
 * Attribution (SPEC-W13, Agent E): embed.js also forwards the host page's
 * first-touch URL attribution as {type:"opendesk:attribution",
 * attribution:{utm?, promo_code?, ref?}}. The same fetch tap merges it as
 * `attribution` into /voice/chat JSON bodies, exactly like client_location
 * (additive key; the server tolerates unknown keys). Precedence
 * (promo_code > UTM > QR slug > channel_of_first_touch) and the
 * first-touch-never-overwritten rule are enforced server-side
 * (SPEC-W13 §3/§6) — the widget only forwards. Bodies pass through
 * byte-identical when no attribution was received.
 */

/** GPS fix forwarded by the embed.js loader (host page, consent-gated). */
interface ClientLocation {
  lat: number;
  lng: number;
  accuracy: number;
}

/** Latest consented location fix; null until the host page forwards one. */
let clientLocation: ClientLocation | null = null;

/** Validate an opendesk:location payload; returns null when unusable. */
function sanitizeLocation(raw: unknown): ClientLocation | null {
  if (!raw || typeof raw !== "object") return null;
  const loc = raw as Record<string, unknown>;
  const { lat, lng, accuracy } = loc;
  if (
    typeof lat !== "number" ||
    typeof lng !== "number" ||
    !Number.isFinite(lat) ||
    !Number.isFinite(lng) ||
    lat < -90 ||
    lat > 90 ||
    lng < -180 ||
    lng > 180
  ) {
    return null;
  }
  const acc =
    typeof accuracy === "number" && Number.isFinite(accuracy) && accuracy >= 0
      ? accuracy
      : 0;
  return { lat, lng, accuracy: acc };
}

/**
 * Merge client_location into a /voice/chat request body (SPEC-W11 Part D).
 * Returns the original init untouched when there is no fix, no JSON string
 * body, or the body is not a plain JSON object — never throws.
 */
function withClientLocation(init?: RequestInit): RequestInit | undefined {
  if (!clientLocation || !init || typeof init.body !== "string") return init;
  try {
    const body = JSON.parse(init.body) as unknown;
    if (!body || typeof body !== "object" || Array.isArray(body)) return init;
    return {
      ...init,
      body: JSON.stringify({ ...(body as Record<string, unknown>), client_location: clientLocation }),
    };
  } catch {
    return init;
  }
}

/** UTM triplet captured from the host page URL (SPEC-W13). */
interface AttributionUtm {
  source?: string;
  medium?: string;
  campaign?: string;
}

/** First-touch attribution forwarded by the embed.js loader (SPEC-W13). */
interface Attribution {
  utm?: AttributionUtm;
  promo_code?: string;
  ref?: string;
}

/** Latest attribution payload; null until the host page forwards one. */
let attribution: Attribution | null = null;

const ATTR_MAX_LEN = 120;

/** Trim + length-cap an attribution string; null when unusable. */
function cleanAttrString(raw: unknown): string | undefined {
  if (typeof raw !== "string") return undefined;
  const v = raw.trim();
  return v && v.length <= ATTR_MAX_LEN ? v : undefined;
}

/** Validate an opendesk:attribution payload; returns null when unusable. */
function sanitizeAttribution(raw: unknown): Attribution | null {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
  const a = raw as Record<string, unknown>;
  const out: Attribution = {};
  if (a.utm && typeof a.utm === "object" && !Array.isArray(a.utm)) {
    const u = a.utm as Record<string, unknown>;
    const utm: AttributionUtm = {};
    const source = cleanAttrString(u.source);
    const medium = cleanAttrString(u.medium);
    const campaign = cleanAttrString(u.campaign);
    if (source) utm.source = source;
    if (medium) utm.medium = medium;
    if (campaign) utm.campaign = campaign;
    if (utm.source || utm.medium || utm.campaign) out.utm = utm;
  }
  const promo = cleanAttrString(a.promo_code);
  if (promo) out.promo_code = promo;
  const ref = cleanAttrString(a.ref);
  if (ref) out.ref = ref;
  return out.utm || out.promo_code || out.ref ? out : null;
}

/**
 * Merge attribution into a /voice/chat request body (SPEC-W13) — same
 * additive-key contract as withClientLocation. Returns the original init
 * untouched when there is no attribution, no JSON string body, or the body
 * is not a plain JSON object — never throws.
 */
function withAttribution(init?: RequestInit): RequestInit | undefined {
  if (!attribution || !init || typeof init.body !== "string") return init;
  try {
    const body = JSON.parse(init.body) as unknown;
    if (!body || typeof body !== "object" || Array.isArray(body)) return init;
    return {
      ...init,
      body: JSON.stringify({ ...(body as Record<string, unknown>), attribution }),
    };
  } catch {
    return init;
  }
}

interface UiAction {
  type: string;
  path?: string;
  selector?: string;
  offering_id?: string;
}

export interface BridgeOffering {
  id: string;
  name: string;
}

const TAP_KEY = "__opendeskUiActionsFetchTap";
const HIGHLIGHT_CLASS = "opendesk-ui-highlight";
const HIGHLIGHT_MS = 2000;
const HIGHLIGHT_RGB = "199, 91, 57"; // terracotta
const SELECTOR_RE = /^[a-zA-Z0-9\-_#. :\[\]="'>]{1,120}$/;
const UUID_RE =
  /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

type ActionListener = (action: UiAction) => void;

interface FetchTap {
  original: typeof fetch;
  wrapped: typeof fetch;
  listeners: Set<ActionListener>;
  users: number;
}

/** Validate + normalize; returns null when the action must be dropped. */
function sanitize(action: unknown): UiAction | null {
  if (!action || typeof action !== "object") return null;
  const a = action as Record<string, unknown>;
  if (a.type === "navigate") {
    const path = a.path;
    if (
      typeof path !== "string" ||
      !path.startsWith("/") ||
      path.startsWith("//") ||
      path.includes("://")
    ) {
      return null;
    }
    return { type: "navigate", path };
  }
  if (a.type === "highlight") {
    const selector = a.selector;
    if (typeof selector !== "string" || !SELECTOR_RE.test(selector)) {
      return null;
    }
    return { type: "highlight", selector };
  }
  if (a.type === "prefill_booking") {
    const offeringId = a.offering_id;
    if (typeof offeringId !== "string" || !UUID_RE.test(offeringId)) {
      return null;
    }
    return { type: "prefill_booking", offering_id: offeringId.toLowerCase() };
  }
  return null;
}

function requestUrl(input: RequestInfo | URL): string | null {
  try {
    if (typeof input === "string") return input;
    if (input instanceof URL) return input.href;
    return input.url;
  } catch {
    return null;
  }
}

/** Scan a cloned /voice/chat response for UI actions; never throws. */
async function scanResponse(res: Response, emit: (a: UiAction) => void) {
  const contentType = res.headers.get("content-type") ?? "";
  if (contentType.includes("text/event-stream")) {
    if (!res.body) return;
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    const handleLine = (line: string) => {
      const trimmed = line.trim();
      if (!trimmed.startsWith("data:")) return;
      try {
        const evt = JSON.parse(trimmed.slice(5).trim()) as { ui_action?: unknown };
        if (evt.ui_action) {
          const action = sanitize(evt.ui_action);
          if (action) emit(action);
        }
      } catch {
        /* malformed frame — ignore */
      }
    };
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split("\n");
      buffer = lines.pop() ?? "";
      lines.forEach(handleLine);
    }
    handleLine(buffer);
    return;
  }
  if (contentType.includes("application/json")) {
    const data = (await res.json()) as { ui_actions?: unknown };
    if (Array.isArray(data.ui_actions)) {
      for (const raw of data.ui_actions) {
        const action = sanitize(raw);
        if (action) emit(action);
      }
    }
  }
}

/**
 * Install (or reuse) the namespaced fetch tap. Returns a release function
 * that removes this listener and restores the original window.fetch when
 * the last user goes away.
 */
function acquireFetchTap(listener: ActionListener): () => void {
  const w = window as unknown as Record<string, FetchTap | undefined>;
  let tap = w[TAP_KEY];
  if (!tap) {
    const original = window.fetch.bind(window);
    const listeners = new Set<ActionListener>();
    const wrapped: typeof fetch = async (input, init) => {
      let effectiveInit = init;
      try {
        const chatUrl = requestUrl(input);
        if (chatUrl && new URL(chatUrl, window.location.href).pathname.endsWith("/voice/chat")) {
          // SPEC-W11 Part D: merge the consented GPS fix (if any) into the
          // chat payload; bodies pass through untouched without a fix.
          // SPEC-W13: then merge first-touch attribution (if any) the same
          // way. Each merger returns init untouched when it has nothing to
          // add, so composition stays byte-identical in the empty case.
          effectiveInit = withAttribution(withClientLocation(init));
        }
      } catch {
        /* never rewrite on error */
      }
      const res = await original(input, effectiveInit);
      try {
        const url = requestUrl(input);
        if (url && new URL(url, window.location.href).pathname.endsWith("/voice/chat")) {
          // Scan a clone in the background; the widget gets the untouched
          // original response either way.
          void scanResponse(res.clone(), (a) =>
            listeners.forEach((fn) => {
              try {
                fn(a);
              } catch {
                /* listener must not break fetch */
              }
            }),
          ).catch(() => {});
        }
      } catch {
        /* never break the page's fetch */
      }
      return res;
    };
    tap = { original, wrapped, listeners, users: 0 };
    w[TAP_KEY] = tap;
    window.fetch = wrapped;
  }
  tap.listeners.add(listener);
  tap.users += 1;
  return () => {
    tap.listeners.delete(listener);
    tap.users -= 1;
    if (tap.users <= 0) {
      // Clean restore: only unwrap if our wrapper is still on top.
      if (window.fetch === tap.wrapped) {
        window.fetch = tap.original;
      }
      const w2 = window as unknown as Record<string, FetchTap | undefined>;
      if (w2[TAP_KEY] === tap) delete w2[TAP_KEY];
    }
  };
}

function injectHighlightStyle(doc: Document) {
  if (doc.getElementById("opendesk-ui-highlight-style")) return;
  const style = doc.createElement("style");
  style.id = "opendesk-ui-highlight-style";
  style.textContent =
    `@keyframes opendesk-highlight-pulse {` +
    `0% { box-shadow: 0 0 0 0 rgba(${HIGHLIGHT_RGB}, 0.45); }` +
    `70% { box-shadow: 0 0 0 12px rgba(${HIGHLIGHT_RGB}, 0); }` +
    `100% { box-shadow: 0 0 0 0 rgba(${HIGHLIGHT_RGB}, 0); }` +
    `}` +
    `.${HIGHLIGHT_CLASS} {` +
    `outline: 3px solid rgba(${HIGHLIGHT_RGB}, 0.95) !important;` +
    `outline-offset: 2px;` +
    `animation: opendesk-highlight-pulse 1s ease-out 2;` +
    `}`;
  doc.head.appendChild(style);
}

function highlightIn(doc: Document, selector: string): boolean {
  const el = doc.querySelector(selector);
  if (!el) return false;
  injectHighlightStyle(doc);
  el.scrollIntoView({ behavior: "smooth", block: "center" });
  el.classList.add(HIGHLIGHT_CLASS);
  window.setTimeout(() => {
    try {
      el.classList.remove(HIGHLIGHT_CLASS);
    } catch {
      /* element gone — fine */
    }
  }, HIGHLIGHT_MS);
  return true;
}

/**
 * Pre-select an offering in the booking form, following the form's own
 * selection path: the offering cards in the "Choose a service" step are
 * buttons whose title paragraph holds the offering name — clicking the
 * matching card runs the exact same handler as a visitor click.
 */
function preselectOffering(offering: BridgeOffering): boolean {
  const buttons = Array.from(document.querySelectorAll<HTMLElement>("main button"));
  const card = buttons.find((btn) =>
    Array.from(btn.querySelectorAll("p")).some(
      (p) => p.textContent?.trim() === offering.name,
    ),
  );
  if (!card) return false;
  card.scrollIntoView({ behavior: "smooth", block: "center" });
  card.click();
  return true;
}

export function UiActionsBridge({ offerings }: { offerings: BridgeOffering[] }) {
  // Latest offerings list for the (stable) listener closure.
  const offeringsRef = React.useRef(offerings);
  offeringsRef.current = offerings;

  React.useEffect(() => {
    const forwardToHost = (action: UiAction) => {
      try {
        if (window.parent === window) return; // not embedded
        // document.referrer is the host page URL inside an iframe; fall
        // back to "*" only when it is unavailable (the loader re-checks
        // origin + source on receipt).
        const target = document.referrer
          ? new URL(document.referrer).origin
          : "*";
        window.parent.postMessage({ type: "opendesk:ui-action", action }, target);
      } catch {
        /* forwarding is best-effort */
      }
    };

    const execute = (action: UiAction) => {
      try {
        if (action.type === "navigate") {
          // The host page navigates (the widget iframe stays alive, so the
          // conversation survives). Standalone fallback navigates locally.
          if (window.parent !== window) {
            forwardToHost(action);
          } else {
            window.location.assign(action.path!);
          }
          return;
        }
        if (action.type === "highlight") {
          const handled = highlightIn(document, action.selector!);
          if (!handled) forwardToHost(action);
          return;
        }
        if (action.type === "prefill_booking") {
          const offeringId = action.offering_id!;
          document.dispatchEvent(
            new CustomEvent("opendesk:prefill", { detail: { offering_id: offeringId } }),
          );
          const offering = offeringsRef.current.find((o) => o.id === offeringId);
          const handled = offering ? preselectOffering(offering) : false;
          if (!handled) forwardToHost(action); // host page may listen for it
          return;
        }
      } catch {
        /* an action must never break the widget */
      }
    };

    // SPEC-W11 Part D: receive the consented GPS fix from the embed.js
    // loader on the host page. Only messages coming from the direct parent
    // frame are honored, with an origin check against the referrer (the
    // host page) when it is available.
    const onLocationMessage = (event: MessageEvent) => {
      try {
        if (window.parent === window || event.source !== window.parent) return;
        const data = event.data as { type?: unknown; location?: unknown } | null;
        if (!data || data.type !== "opendesk:location") return;
        if (document.referrer) {
          if (event.origin !== new URL(document.referrer).origin) return;
        }
        const loc = sanitizeLocation(data.location);
        if (loc) clientLocation = loc;
      } catch {
        /* location capture is best-effort */
      }
    };
    window.addEventListener("message", onLocationMessage);

    // SPEC-W13: receive first-touch URL attribution from the embed.js
    // loader on the host page. Same trust rules as the location message:
    // direct parent frame only, origin-checked against the referrer when
    // available. The latest payload wins locally; the backend enforces the
    // first-touch-never-overwritten rule on the lead record itself.
    const onAttributionMessage = (event: MessageEvent) => {
      try {
        if (window.parent === window || event.source !== window.parent) return;
        const data = event.data as { type?: unknown; attribution?: unknown } | null;
        if (!data || data.type !== "opendesk:attribution") return;
        if (document.referrer) {
          if (event.origin !== new URL(document.referrer).origin) return;
        }
        const attr = sanitizeAttribution(data.attribution);
        if (attr) attribution = attr;
      } catch {
        /* attribution capture is best-effort */
      }
    };
    window.addEventListener("message", onAttributionMessage);

    const release = acquireFetchTap(execute);
    return () => {
      window.removeEventListener("message", onLocationMessage);
      window.removeEventListener("message", onAttributionMessage);
      release();
    };
  }, []);

  return null;
}
