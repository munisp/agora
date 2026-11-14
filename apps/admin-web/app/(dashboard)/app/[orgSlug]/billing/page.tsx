import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewBilling } from "@/lib/roles";
import { BillingClient } from "./billing-client";

export const metadata = { title: "Billing" };

export default async function BillingPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W7 Part C): billing is visible to
  // owner/billing only; everyone else is bounced to the overview.
  const session = await auth();
  if (!canViewBilling(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <BillingClient orgSlug={orgSlug} />;
}
