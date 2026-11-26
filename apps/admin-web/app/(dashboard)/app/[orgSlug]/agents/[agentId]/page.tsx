import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAgents } from "@/lib/roles";
import { AgentDetailClient } from "./agent-detail-client";

export const metadata = { title: "Agent" };

export default async function AgentDetailPage({
  params,
}: {
  params: Promise<{ orgSlug: string; agentId: string }>;
}) {
  const { orgSlug, agentId } = await params;
  // Same server-side gate as the agents list (SPEC-W38).
  const session = await auth();
  if (!canViewAgents(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <AgentDetailClient orgSlug={orgSlug} agentId={agentId} />;
}
