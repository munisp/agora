import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAnalytics, hasAnyRole } from "@/lib/roles";
import { LendingClient } from "./lending-client";

export const metadata = { title: "Lending" };

/**
 * Lending app page (SPEC-W20 Agent C, nav_route /apps/lending — registered
 * in the W18 app catalog by the integrator). Portfolio/queue/product reads
 * are view_analytics (owner/admin/analyst); product editing and
 * decision/disburse/repay writes are manage_bookings (owner/admin/staff) —
 * computed here from the session (same server-side role-guard pattern as
 * the W14 growth pages) and enforced again by booking-service.
 */
export default async function LendingPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  if (!canViewAnalytics(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return (
    <LendingClient
      orgSlug={orgSlug}
      canManage={hasAnyRole(session?.realmRoles, ["owner", "admin", "staff"])}
    />
  );
}
