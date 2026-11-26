import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewGeoCampaigns } from "@/lib/roles";
import { SegmentsClient } from "./segments-client";

export const metadata = { title: "Segments" };

export default async function SegmentsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W28 WS-C): segments drive consent-gated
  // outreach campaigns — messaging spend, same audience as geo campaigns
  // (owner/admin only). The page's reads are tenant-scoped by graph-service
  // and the launch path re-checks consent/DND server-side regardless.
  const session = await auth();
  if (!canViewGeoCampaigns(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <SegmentsClient orgSlug={orgSlug} />;
}
