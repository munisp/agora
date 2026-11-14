import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { hasAnyRole } from "@/lib/roles";
import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { unwrap } from "@/components/apps/types";
import type { ContactSearchResult } from "@/components/apps/crm-360/types";
import { Crm360Client } from "./crm-360-client";

export const metadata = { title: "CRM 360" };

/**
 * CRM-360 app (SPEC-W20 Agent A): unified customer profile — contact
 * search, tags, notes, 360 aggregation and timeline. Server-side role
 * guard mirrors the growth/helpdesk page pattern — this is an operator
 * surface (owner/admin/staff); viewers/analysts are bounced home. Note
 * and tag mutations are re-checked by booking-service perms
 * (manage_bookings); reads ride view_analytics.
 */
export default async function Crm360Page({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  const roles = session?.realmRoles;
  if (!hasAnyRole(roles, ["owner", "admin", "staff"])) {
    redirect(`/app/${orgSlug}`);
  }

  // SPEC-W46 AW-2: the unfiltered contact search (default view) is fetched
  // server-side and handed to the client as initialData; the debounced
  // search interaction stays client-side. Same degradation semantics the
  // client applied (404 → "still rolling out" note).
  let initialResults: ContactSearchResult[] = [];
  let initialError: string | null = null;
  try {
    const data = await serverApi<unknown>(
      "/api/bookings/v1/crm/contacts/search",
      { query: { tenant: orgSlug, limit: 50 } },
    );
    initialResults = unwrap<ContactSearchResult>(data);
  } catch (e) {
    initialError =
      e instanceof ApiError && e.status !== 404
        ? e.message
        : "CRM-360 is not available yet — the booking-service CRM API may still be rolling out.";
  }

  return (
    <Crm360Client
      orgSlug={orgSlug}
      canWork
      initialResults={initialResults}
      initialError={initialError}
    />
  );
}
