import type { Metadata } from "next";
import Link from "next/link";

export const metadata: Metadata = {
  title: "Offline",
};

/**
 * Offline fallback page (SPEC-W16 §3): the service worker serves this when a
 * navigation fails with no cached copy. Warm tokens from globals.css.
 */
export default function OfflinePage() {
  return (
    <main
      className="flex min-h-screen flex-col items-center justify-center gap-4 p-8 text-center"
      style={{ backgroundColor: "#faf7f1", color: "#2e2a25" }}
    >
      <div
        className="rounded-2xl border p-8 shadow-sm"
        style={{ backgroundColor: "#ffffff", borderColor: "#e3dac8" }}
      >
        <h1 className="text-xl font-semibold">You&rsquo;re offline</h1>
        <p className="mt-2 max-w-sm text-sm" style={{ color: "#6e6558" }}>
          Agora can&rsquo;t reach the network right now and this page
          isn&rsquo;t cached yet. Reconnect and try again — pages you&rsquo;ve
          visited before remain available offline.
        </p>
        <Link
          href="/"
          className="mt-4 inline-block rounded-lg px-4 py-2 text-sm font-medium"
          style={{ backgroundColor: "#7c5b3e", color: "#faf7f1" }}
        >
          Retry
        </Link>
      </div>
    </main>
  );
}
