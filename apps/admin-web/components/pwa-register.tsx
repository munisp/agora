"use client";

import { useEffect } from "react";

/**
 * Registers /sw.js once on load (SPEC-W16 §3). Skipped in development so the
 * dev server's HMR and error overlays are never shadowed by a stale cache.
 */
export function PwaRegister() {
  useEffect(() => {
    if (process.env.NODE_ENV !== "production") return;
    if (!("serviceWorker" in navigator)) return;

    const register = () => {
      navigator.serviceWorker.register("/sw.js").catch((err) => {
        // Registration failure is non-fatal: the app works without a SW.
        console.warn("[pwa] service worker registration failed", err);
      });
    };

    if (document.readyState === "complete") {
      register();
    } else {
      window.addEventListener("load", register, { once: true });
      return () => window.removeEventListener("load", register);
    }
  }, []);

  return null;
}
