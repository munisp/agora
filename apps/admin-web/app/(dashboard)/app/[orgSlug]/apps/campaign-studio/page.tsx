import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { hasAnyRole } from "@/lib/roles";
import { StudioClient } from "./studio-client";

export const metadata = { title: "Campaign Studio" };

/**
 * Campaign Studio app page (SPEC-W19 Agent D, W18 portal nav_route
 * convention: /app/{org}/apps/campaign-studio — no org-nav edits; the
 * portal links here from the app catalog).
 *
 * Server-side role guard (same pattern as the W18 apps portal): journeys
 * spend messaging budget, so the studio is owner/admin only; everyone else
 * is bounced to the overview. The backend enforces view_analytics /
 * manage_bookings perms again on every endpoint.
 */
export default async function CampaignStudioPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  if (!hasAnyRole(session?.realmRoles, ["owner", "admin"])) {
    redirect(`/app/${orgSlug}`);
  }
  return <StudioClient orgSlug={orgSlug} canManage />;
}
