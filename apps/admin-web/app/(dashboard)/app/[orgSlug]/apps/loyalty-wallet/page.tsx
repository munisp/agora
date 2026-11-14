import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAnalytics, hasAnyRole } from "@/lib/roles";
import { LoyaltyWalletClient } from "./loyalty-wallet-client";

export const metadata = { title: "Loyalty & Wallet" };

/**
 * Loyalty & Wallet app page (SPEC-W19 Agent C, nav_route
 * /apps/loyalty-wallet — registered in the W18 app catalog by the
 * integrator). Wallet/leaderboard/program reads are view_analytics
 * (owner/admin/analyst); program editing and accrue/redeem writes are
 * manage_bookings (owner/admin/staff) — computed here from the session
 * (same server-side role-guard pattern as the W14 growth pages) and
 * enforced again by booking-service.
 */
export default async function LoyaltyWalletPage({
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
    <LoyaltyWalletClient
      orgSlug={orgSlug}
      canManage={hasAnyRole(session?.realmRoles, ["owner", "admin", "staff"])}
    />
  );
}
