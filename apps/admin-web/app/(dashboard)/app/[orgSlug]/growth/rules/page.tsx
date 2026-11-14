import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAnalytics, canViewBilling } from "@/lib/roles";
import { PageHeader } from "@/components/page-header";
import { GrowthTabs } from "@/components/growth/growth-tabs";
import { CommissionRulesEditor } from "@/components/growth/rules-editor";

export const metadata = { title: "Commission rules" };

export default async function CommissionRulesPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W14 Agent C): rules are manage_billing —
  // same gate as the billing page (owner/billing only).
  const session = await auth();
  if (!canViewBilling(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return (
    <div className="max-w-6xl">
      <PageHeader
        title="Commission rules"
        description="Tenant-editable bounty rules: trigger, beneficiary, flat amount or percentage (basis points), optional cap, priority and active toggle. Rules fire when a referral is verified."
      />
      <GrowthTabs
        orgSlug={orgSlug}
        canAnalytics={canViewAnalytics(session?.realmRoles)}
        canBilling
      />
      <CommissionRulesEditor orgSlug={orgSlug} />
    </div>
  );
}
