import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAgents } from "@/lib/roles";
import { AgentsClient } from "./agents-client";

export const metadata = { title: "Agents" };

export default async function AgentsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W38): the agents section is visible to
  // owner/admin/staff; viewers and analysts are bounced to the overview.
  const session = await auth();
  if (!canViewAgents(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <AgentsClient orgSlug={orgSlug} />;
}
