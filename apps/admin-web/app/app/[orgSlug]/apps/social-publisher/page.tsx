import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAnalytics, hasAnyRole } from "@/lib/roles";
import { SocialPublisherClient } from "./social-publisher-client";

export const metadata = { title: "Social Publisher" };

/**
 * Social Publisher app page (SPEC-W21 Agent B, nav_route
 * /apps/social-publisher — registered in the W18 app catalog by the
 * integrator). Account/creative/post/ad reads are view_analytics
 * (owner/admin/analyst); connect/edit/publish/launch writes are
 * manage_bookings (owner/admin/staff) — computed here from the session
 * (same server-side role-guard pattern as the W19/W20 app pages) and
 * enforced again by booking-service.
 */
export default async function SocialPublisherPage({
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
    <SocialPublisherClient
      orgSlug={orgSlug}
      canManage={hasAnyRole(session?.realmRoles, ["owner", "admin", "staff"])}
    />
  );
}
