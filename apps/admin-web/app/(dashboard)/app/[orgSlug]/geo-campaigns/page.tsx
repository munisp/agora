import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewGeoCampaigns } from "@/lib/roles";
import { GeoCampaignsClient } from "./geo-campaigns-client";

export const metadata = { title: "Geo campaigns" };

export default async function GeoCampaignsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W8 Part C): geo campaigns spend messaging
  // budget — owner/admin only; everyone else is bounced to the overview.
  const session = await auth();
  if (!canViewGeoCampaigns(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <GeoCampaignsClient orgSlug={orgSlug} />;
}
