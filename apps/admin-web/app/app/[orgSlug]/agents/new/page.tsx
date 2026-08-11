import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewAgents } from "@/lib/roles";
import { NewAgentClient } from "./new-agent-client";

export const metadata = { title: "New agent" };

export default async function NewAgentPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Same server-side gate as the agents list (SPEC-W38).
  const session = await auth();
  if (!canViewAgents(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <NewAgentClient orgSlug={orgSlug} />;
}
