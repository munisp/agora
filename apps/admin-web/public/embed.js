/*!
 * OpenDesk embed loader — injects the booking + chat widget as an iframe.
 *
 * Usage:
 *   <script src="https://<your-opendesk-host>/embed.js" data-site="acme" async></script>
 *
 * Optional data attributes:
 *   data-site     (required) public site slug, e.g. "acme"
 *   data-height   iframe height, default "640px"
 *   data-width    iframe width,  default "100%"
 *   data-target   CSS selector of the element to mount into; default: the
 *                 script tag is replaced in place.
 *   data-location-consent  "true" enables one-shot GPS capture (SPEC-W11
 *                 Part D): after the iframe loads, the host page requests
 *                 navigator.geolocation ONCE and forwards the fix to the
 *                 widget via postMessage ("opendesk:location"); the widget
 *                 then includes it as client_location {lat,lng,accuracy}
 *                 in every /voice/chat request it originates. Denial,
 *                 timeout or missing geolocation support are silent — chat
 *                 is never blocked and no location is sent without consent.
 *
 * The loader is dependency-free and lazy: it only creates the iframe once
 * the host page has finished parsing.
 *
 * Agent-driven UI actions (SPEC-W9 Part B): when the receptionist invokes a
 * UI action tool (navigate / highlight / prefill_booking), the widget inside
 * the iframe forwards the server-validated action here via postMessage and
 * the loader applies it to the HOST page:
 *   navigate        -> window.location.assign(path) after a same-origin guard
 *   highlight       -> querySelector + smooth scrollIntoView + 2s terracotta
 *                      outline pulse (tiny CSS injected once)
 *   prefill_booking -> CustomEvent('opendesk:prefill', {detail:{offering_id}})
 *                      re-dispatched on the host document
 * Every action runs inside try/catch — a bad action never breaks the host
 * page or the widget. Actions are validated twice: server-side
 * (voice-agent-runtime app/ui_actions.py) and again here before execution.
 */
(function () {
  "use strict";

  // ---------------------------------------------------------------------
  // UI action execution on the host page (SPEC-W9 Part B3)
  // ---------------------------------------------------------------------
  var TAP_FLAG = "__opendeskUiActionsHost"; // namespace guard: install once
  var HIGHLIGHT_CLASS = "opendesk-ui-highlight";
  var HIGHLIGHT_MS = 2000;
  // Terracotta pulse (warm, matches the OpenDesk palette).
  var HIGHLIGHT_RGB = "199, 91, 57";
  // Same charset as the server-side sanitizer (app/ui_actions.py).
  var SELECTOR_RE = /^[a-zA-Z0-9\-_#. :\[\]="'>]{1,120}$/;
  var UUID_RE =
    /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

  function injectHighlightStyle(doc) {
    if (doc.getElementById("opendesk-ui-highlight-style")) return;
    var style = doc.createElement("style");
    style.id = "opendesk-ui-highlight-style";
    style.textContent =
      "@keyframes opendesk-highlight-pulse {" +
      "0% { box-shadow: 0 0 0 0 rgba(" + HIGHLIGHT_RGB + ", 0.45); }" +
      "70% { box-shadow: 0 0 0 12px rgba(" + HIGHLIGHT_RGB + ", 0); }" +
      "100% { box-shadow: 0 0 0 0 rgba(" + HIGHLIGHT_RGB + ", 0); }" +
      "}" +
      "." + HIGHLIGHT_CLASS + " {" +
      "outline: 3px solid rgba(" + HIGHLIGHT_RGB + ", 0.95) !important;" +
      "outline-offset: 2px;" +
      "animation: opendesk-highlight-pulse 1s ease-out 2;" +
      "}";
    doc.head.appendChild(style);
  }

  // Returns true when the action was handled locally.
  function executeUiAction(action) {
    if (!action || typeof action.type !== "string") return false;
    try {
      if (action.type === "navigate") {
        var path = action.path;
        // Same-origin guard: leading single slash, no scheme/host.
        if (
          typeof path !== "string" ||
          path.charAt(0) !== "/" ||
          path.charAt(1) === "/" ||
          path.indexOf("://") !== -1
        ) {
          return false;
        }
        window.location.assign(path);
        return true;
      }
      if (action.type === "highlight") {
        var selector = action.selector;
        if (typeof selector !== "string" || !SELECTOR_RE.test(selector)) {
          return false;
        }
        var el = document.querySelector(selector);
        if (!el) return false;
        injectHighlightStyle(document);
        el.scrollIntoView({ behavior: "smooth", block: "center" });
        el.classList.add(HIGHLIGHT_CLASS);
        window.setTimeout(function () {
          try {
            el.classList.remove(HIGHLIGHT_CLASS);
          } catch (e) {
            /* element gone — fine */
          }
        }, HIGHLIGHT_MS);
        return true;
      }
      if (action.type === "prefill_booking") {
        var offeringId = action.offering_id;
        if (typeof offeringId !== "string" || !UUID_RE.test(offeringId)) {
          return false;
        }
        document.dispatchEvent(
          new CustomEvent("opendesk:prefill", {
            detail: { offering_id: offeringId },
          })
        );
        return true;
      }
    } catch (e) {
      // Never break the host page because of an action.
      return false;
    }
    return false;
  }

  /**
   * Listen for actions forwarded from the widget iframe. Only messages from
   * the OpenDesk iframe itself (matching source + origin) are honored, and
   * each action is re-validated above before anything runs.
   */
  function installActionListener(widgetOrigin, iframe) {
    if (window[TAP_FLAG]) {
      window[TAP_FLAG].push({ origin: widgetOrigin, frame: iframe });
      return;
    }
    window[TAP_FLAG] = [{ origin: widgetOrigin, frame: iframe }];
    window.addEventListener("message", function (event) {
      var data = event && event.data;
      if (!data || data.type !== "opendesk:ui-action" || !data.action) return;
      var trusted = window[TAP_FLAG].some(function (entry) {
        return (
          event.origin === entry.origin &&
          (!entry.frame.contentWindow || event.source === entry.frame.contentWindow)
        );
      });
      if (!trusted) return;
      executeUiAction(data.action);
    });
  }

  // ---------------------------------------------------------------------
  // Widget GPS capture (SPEC-W11 Part D)
  // ---------------------------------------------------------------------
  /**
   * When the embed script carries data-location-consent="true", request the
   * host page's geolocation ONCE after the iframe loads and forward the fix
   * into the widget via the opendesk:* postMessage protocol. The widget's
   * fetch bridge merges it as client_location {lat,lng,accuracy} into each
   * /voice/chat payload (additive key; the server tolerates unknown keys).
   * Graceful by contract: denial, timeout or a browser without geolocation
   * never blocks chat and never sends anything.
   */
  var GEO_TIMEOUT_MS = 10000;
  var GEO_MAX_AGE_MS = 300000;

  function captureLocationOnce(widgetOrigin, iframe) {
    if (!("geolocation" in navigator) || !iframe.contentWindow) return;
    var send = function (pos) {
      try {
        var loc = {
          lat: pos.coords.latitude,
          lng: pos.coords.longitude,
          accuracy: pos.coords.accuracy,
        };
        if (
          typeof loc.lat !== "number" ||
          typeof loc.lng !== "number" ||
          !isFinite(loc.lat) ||
          !isFinite(loc.lng)
        ) {
          return;
        }
        iframe.contentWindow.postMessage(
          { type: "opendesk:location", location: loc },
          widgetOrigin
        );
      } catch (e) {
        /* best-effort — never break the host page */
      }
    };
    try {
      navigator.geolocation.getCurrentPosition(send, function () {
        /* denied/unavailable — chat proceeds without location */
      }, { timeout: GEO_TIMEOUT_MS, maximumAge: GEO_MAX_AGE_MS });
    } catch (e) {
      /* geolocation threw synchronously (e.g. permissions policy) — ignore */
    }
  }

  // ---------------------------------------------------------------------
  // Iframe loader (unchanged behaviour)
  // ---------------------------------------------------------------------
  function mount(script) {
    var site = script.getAttribute("data-site");
    if (!site) {
      console.error("[opendesk-embed] missing data-site attribute");
      return;
    }
    var origin = new URL(script.src).origin;
    var iframe = document.createElement("iframe");
    iframe.src = origin + "/embed/" + encodeURIComponent(site);
    iframe.title = "Booking widget";
    iframe.style.border = "0";
    iframe.style.width = script.getAttribute("data-width") || "100%";
    iframe.style.height = script.getAttribute("data-height") || "640px";
    iframe.style.maxWidth = "100%";
    iframe.setAttribute("loading", "lazy");
    // Mic access is needed if the tenant enables the voice button in embeds.
    iframe.setAttribute("allow", "microphone");

    installActionListener(origin, iframe);

    // SPEC-W11 Part D: opt-in GPS capture — one-shot, after iframe load.
    if (script.getAttribute("data-location-consent") === "true") {
      iframe.addEventListener("load", function onLoad() {
        iframe.removeEventListener("load", onLoad);
        captureLocationOnce(origin, iframe);
      });
    }

    var targetSel = script.getAttribute("data-target");
    var target = targetSel ? document.querySelector(targetSel) : null;
    if (target) {
      target.appendChild(iframe);
    } else if (script.parentNode) {
      script.parentNode.insertBefore(iframe, script.nextSibling);
    }
  }

  function init() {
    var scripts = document.querySelectorAll("script[data-site][src*='embed.js']");
    Array.prototype.forEach.call(scripts, mount);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
