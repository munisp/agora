import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewGeoCampaigns } from "@/lib/roles";
import { PersonClient } from "./person-client";

export const metadata = { title: "Person 360" };

export default async function PersonPage({
  params,
}: {
  params: Promise<{ orgSlug: string; id: string }>;
}) {
  const { orgSlug, id } = await params;
  // Same guard as the segments page (SPEC-W28 WS-C): the graph explorer
  // reads the same tenant-scoped graph.
  const session = await auth();
  if (!canViewGeoCampaigns(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <PersonClient orgSlug={orgSlug} personId={id} />;
}
