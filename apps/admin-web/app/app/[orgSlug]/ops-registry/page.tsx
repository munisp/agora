import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { OpsRegistryClient } from "./ops-registry-client";
import type { RegistryStats } from "./ops-registry-client";

export const metadata = { title: "Registry stats" };

/**
 * SPEC-W77: platform-admin read surface for the cross-tenant doc-fraud
 * registry (ocr-service `GET /v1/internal/ocr/registry-stats`).
 *
 * R2 (platform-admin ONLY): plain `admin` realm role must NOT see
 * cross-tenant aggregates — the gate here is strictly platform-admin,
 * tighter than the ops-alerts admin|platform-admin gate. Everyone else is
 * bounced to the overview.
 *
 * R5 (W46 AW-2): the stats object is fetched server-side and handed to the
 * client as initialStats, removing the HTML→hydrate→BFF waterfall. The
 * fetch goes DIRECTLY to the ocr-service with the internal token injected
 * here (server-side only — never shipped to the client bundle), because
 * serverApi targets the APISIX gateway, which deliberately has no
 * /v1/internal/* route (W53 posture). The client component's refresh button
 * goes through the BFF `/api/ocr-registry/registry-stats` exception, which
 * re-applies the same session + platform-admin gate.
 *
 * R1 (count-not-identity): the payload is aggregate integers + posture
 * fields ONLY — no registry keys, no tenant ids, no hashes, no per-key
 * rows.
 */

const OCR_BASE = process.env.OCR_BASE_URL ?? "http://ocr:7020";

async function fetchRegistryStats(): Promise<RegistryStats> {
  const token = process.env.OCR_INTERNAL_TOKEN;
  if (!token) {
    throw new Error("OCR_INTERNAL_TOKEN is not configured on admin-web.");
  }
  const res = await fetch(`${OCR_BASE}/v1/internal/ocr/registry-stats`, {
    headers: { accept: "application/json", "x-internal-token": token },
    cache: "no-store",
  });
  if (!res.ok) {
    throw new Error(
      res.status === 503
        ? "Registry stats unavailable: OCR_INTERNAL_TOKEN is not set on the ocr-service."
        : `Registry stats fetch failed (HTTP ${res.status}).`,
    );
  }
  return (await res.json()) as RegistryStats;
}

export default async function OpsRegistryPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  const roles = session?.realmRoles ?? [];
  if (!roles.includes("platform-admin")) {
    redirect(`/app/${orgSlug}`);
  }

  let initialStats: RegistryStats | null = null;
  let initialError: string | null = null;
  try {
    initialStats = await fetchRegistryStats();
  } catch (e) {
    initialError =
      e instanceof Error ? e.message : "Failed to load registry stats.";
  }

  return (
    <OpsRegistryClient
      orgSlug={orgSlug}
      initialStats={initialStats}
      initialError={initialError}
    />
  );
}
