import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAnalytics, canViewBilling } from "@/lib/roles";
import { LedgerClient } from "./ledger-client";

export const metadata = { title: "Commission ledger" };

export default async function CommissionLedgerPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W14 Agent C): the ledger is a read view —
  // same view_analytics gate as the analytics/CAC pages (owner/admin/
  // analyst). Rules/payouts writes stay manage_billing-gated.
  const session = await auth();
  if (!canViewAnalytics(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return (
    <LedgerClient
      orgSlug={orgSlug}
      canAnalytics
      canBilling={canViewBilling(session?.realmRoles)}
    />
  );
}
