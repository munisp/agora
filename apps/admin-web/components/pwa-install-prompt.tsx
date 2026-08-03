"use client";

import * as React from "react";
import { Download, X } from "lucide-react";

/**
 * Install-prompt UX (SPEC-W16, Agent C): captures the browser's
 * `beforeinstallprompt` event and shows a small, dismissible warm-styled
 * banner. Dismissing snoozes the banner for 14 days (localStorage); a
 * successful install hides it permanently for this profile.
 */

const SNOOZE_KEY = "opendesk.pwa-install-snoozed-until";
const SNOOZE_DAYS = 14;

/** Non-standard event fired by Chromium when the app is installable. */
interface BeforeInstallPromptEvent extends Event {
  prompt: () => Promise<void>;
  userChoice: Promise<{ outcome: "accepted" | "dismissed" }>;
}

function snoozed(): boolean {
  try {
    const until = Number(localStorage.getItem(SNOOZE_KEY) ?? "0");
    return Number.isFinite(until) && until > Date.now();
  } catch {
    return false;
  }
}

function snooze() {
  try {
    localStorage.setItem(
      SNOOZE_KEY,
      String(Date.now() + SNOOZE_DAYS * 24 * 60 * 60 * 1000),
    );
  } catch {
    /* private mode: nothing to persist */
  }
}

export function PwaInstallPrompt() {
  const [deferred, setDeferred] = React.useState<BeforeInstallPromptEvent | null>(null);
  const [visible, setVisible] = React.useState(false);

  React.useEffect(() => {
    const onBeforeInstall = (event: Event) => {
      event.preventDefault();
      if (snoozed()) return;
      setDeferred(event as BeforeInstallPromptEvent);
      setVisible(true);
    };
    const onInstalled = () => {
      setVisible(false);
      setDeferred(null);
    };
    window.addEventListener("beforeinstallprompt", onBeforeInstall);
    window.addEventListener("appinstalled", onInstalled);
    return () => {
      window.removeEventListener("beforeinstallprompt", onBeforeInstall);
      window.removeEventListener("appinstalled", onInstalled);
    };
  }, []);

  if (!visible || !deferred) return null;

  const install = async () => {
    try {
      await deferred.prompt();
      await deferred.userChoice;
    } catch {
      /* prompt rejected — treat as dismissed */
    }
    setVisible(false);
    setDeferred(null);
  };

  const dismiss = () => {
    snooze();
    setVisible(false);
    setDeferred(null);
  };

  return (
    <div
      role="dialog"
      aria-label="Install OpenDesk"
      className="fixed bottom-4 left-4 z-50 flex max-w-sm items-start gap-3 rounded-xl border p-4 shadow-lg"
      style={{
        backgroundColor: "#ffffff",
        borderColor: "#e3dac8",
        color: "#2e2a25",
      }}
    >
      <div className="min-w-0 flex-1">
        <p className="text-sm font-semibold">Install OpenDesk Admin</p>
        <p className="mt-1 text-xs" style={{ color: "#6e6558" }}>
          Add the dashboard to your home screen for one-tap access and offline
          fallback.
        </p>
        <button
          type="button"
          onClick={install}
          className="mt-3 inline-flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-xs font-medium"
          style={{ backgroundColor: "#7c5b3e", color: "#faf7f1" }}
        >
          <Download className="h-3.5 w-3.5" aria-hidden />
          Install
        </button>
      </div>
      <button
        type="button"
        onClick={dismiss}
        aria-label="Dismiss install prompt"
        className="rounded-md p-1"
        style={{ color: "#6e6558" }}
      >
        <X className="h-4 w-4" aria-hidden />
      </button>
    </div>
  );
}
