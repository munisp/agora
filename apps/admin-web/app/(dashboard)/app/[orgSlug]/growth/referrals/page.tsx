import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAnalytics, canViewBilling } from "@/lib/roles";
import { ReferralsClient } from "./referrals-client";

export const metadata = { title: "Referrals" };

export default async function ReferralsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W14 Agent C): referrals and the referrer
  // leaderboard are view_analytics reads — owner/admin/analyst only.
  const session = await auth();
  if (!canViewAnalytics(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return (
    <ReferralsClient
      orgSlug={orgSlug}
      canAnalytics
      canBilling={canViewBilling(session?.realmRoles)}
    />
  );
}
