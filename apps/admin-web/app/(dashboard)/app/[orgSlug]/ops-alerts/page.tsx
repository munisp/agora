import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { OpsAlertsClient } from "./ops-alerts-client";
import type { OpsAlert } from "./ops-alerts-client";

export const metadata = { title: "Ops alerts" };

/**
 * SPEC-W45 ORPH O15: ops alerts read-back. The gateway route
 * (/api/notifications/v1/ops-alerts) requires an admin realm role
 * (admin or platform-admin) — the page applies the same gate client-side.
 *
 * SPEC-W46 AW-2: the first page of alerts is fetched server-side (same
 * serverApi pattern as the overview page) and handed to the client as
 * initialData, removing the HTML→hydrate→BFF waterfall. SPEC-W46 AW-4:
 * the list is bounded (limit=100 pages; the client offers "load more"
 * which refetches with a larger limit — the endpoint has no cursor).
 */
export default async function OpsAlertsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  const roles = session?.realmRoles ?? [];
  if (!roles.includes("admin") && !roles.includes("platform-admin")) {
    redirect(`/app/${orgSlug}`);
  }

  let initialAlerts: OpsAlert[] = [];
  let initialError: string | null = null;
  try {
    const data = await serverApi<{ alerts?: OpsAlert[] }>(
      "/api/notifications/v1/ops-alerts",
      // Keep in sync with PAGE_SIZE in ops-alerts-client.tsx.
      { query: { tenant: orgSlug, limit: 100 } },
    );
    initialAlerts = data.alerts ?? [];
  } catch (e) {
    initialError =
      e instanceof ApiError && e.status === 403
        ? "Ops alerts require an admin role."
        : e instanceof ApiError
          ? e.message
          : "Failed to load ops alerts.";
  }

  return (
    <OpsAlertsClient
      orgSlug={orgSlug}
      initialAlerts={initialAlerts}
      initialError={initialError}
    />
  );
}
