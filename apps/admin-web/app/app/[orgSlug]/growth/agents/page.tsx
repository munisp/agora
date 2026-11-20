import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAnalytics, canViewBilling } from "@/lib/roles";
import { AgentsClient } from "./agents-client";

export const metadata = { title: "Referral agents" };

/**
 * Referral agents registry (SPEC-W92 B5): the agent lifecycle page —
 * pending registrations, approval/suspension, beneficiary linkage.
 * Listing is a view_analytics read (same gate as the referrals page);
 * approval/suspension PATCHes are admin-only SERVER-SIDE (SPEC-W45 STK
 * O10) — the UI surfaces the API's 403 rather than hiding actions, so the
 * permission boundary lives in exactly one place.
 */
export default async function AgentsPage({
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
    <AgentsClient
      orgSlug={orgSlug}
      canAnalytics
      canBilling={canViewBilling(session?.realmRoles)}
    />
  );
}
