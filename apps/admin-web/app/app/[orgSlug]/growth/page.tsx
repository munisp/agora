import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAnalytics, canViewBilling } from "@/lib/roles";

export const metadata = { title: "Growth" };

/**
 * Growth section index (SPEC-W14 Agent C): no content of its own — sends the
 * user to the first growth page their roles allow. Referrals/ledger are
 * view_analytics reads; rules/payouts are manage_billing (owner/billing).
 */
export default async function GrowthPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  const roles = session?.realmRoles;
  if (canViewAnalytics(roles)) {
    redirect(`/app/${orgSlug}/growth/referrals`);
  }
  if (canViewBilling(roles)) {
    redirect(`/app/${orgSlug}/growth/rules`);
  }
  redirect(`/app/${orgSlug}`);
}
