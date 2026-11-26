import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { hasAnyRole } from "@/lib/roles";
import { HelpdeskClient } from "./helpdesk-client";

export const metadata = { title: "Helpdesk" };

/**
 * Helpdesk app (SPEC-W19 Agent A): SLA ticketing queue. Server-side role
 * guard mirrors the growth page pattern — the queue is an operator surface
 * (owner/admin/staff, same set as canViewLocations); viewers/analysts are
 * bounced home. Ticket mutations are re-checked by booking-service perms
 * (manage_bookings) and SLA-policy edits are owner/admin only in the UI.
 */
export default async function HelpdeskPage({
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
  return (
    <HelpdeskClient
      orgSlug={orgSlug}
      canWork
      canManage={hasAnyRole(roles, ["owner", "admin"])}
    />
  );
}
