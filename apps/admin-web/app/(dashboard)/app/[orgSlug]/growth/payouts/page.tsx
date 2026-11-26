import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAnalytics, canViewBilling } from "@/lib/roles";
import { PayoutsClient } from "./payouts-client";

export const metadata = { title: "Commission payouts" };

export default async function CommissionPayoutsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W14 Agent C): payouts are manage_billing —
  // same gate as the billing page (owner/billing only).
  const session = await auth();
  if (!canViewBilling(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return (
    <PayoutsClient
      orgSlug={orgSlug}
      canAnalytics={canViewAnalytics(session?.realmRoles)}
      canBilling
    />
  );
}
