import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAnalytics } from "@/lib/roles";
import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { LeadsClient } from "./leads-client";
import type { Lead } from "@/lib/types";

export const metadata = { title: "Leads" };

/**
 * SPEC-W47 UIWEB-14: lead pipeline console. The backend read path is
 * view_analytics-gated (same gate as the CAC page), so the page applies the
 * same server-side role guard. SSR initialData (W46 AW-2).
 */
export default async function LeadsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  if (!canViewAnalytics(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }

  let initialLeads: Lead[] = [];
  let initialError: string | null = null;
  try {
    const data = await serverApi<{ leads?: Lead[] }>(
      "/api/bookings/v1/leads",
      { query: { tenant: orgSlug } },
    );
    initialLeads = data.leads ?? [];
  } catch (e) {
    initialError =
      e instanceof ApiError && e.status === 403
        ? "Leads require an analytics role."
        : e instanceof ApiError
          ? e.message
          : "Failed to load leads.";
  }

  return (
    <LeadsClient
      orgSlug={orgSlug}
      initialLeads={initialLeads}
      initialError={initialError}
    />
  );
}
