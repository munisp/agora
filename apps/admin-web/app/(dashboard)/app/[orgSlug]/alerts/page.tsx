import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewGeoCampaigns } from "@/lib/roles";
import { AlertsClient } from "./alerts-client";

export const metadata = { title: "Alerts" };

export default async function AlertsPage({
  params,
  searchParams,
}: {
  params: Promise<{ orgSlug: string }>;
  searchParams: Promise<{ person_id?: string }>;
}) {
  const { orgSlug } = await params;
  const { person_id } = await searchParams;
  // Same gate as the segments page (SPEC-W30 §4 WS-D): fraud adjudication
  // can quarantine/release audience members — owner/admin only. The graph
  // service enforces the same tenant scoping and auth on every request.
  const session = await auth();
  if (!canViewGeoCampaigns(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <AlertsClient orgSlug={orgSlug} initialPersonId={person_id} />;
}
