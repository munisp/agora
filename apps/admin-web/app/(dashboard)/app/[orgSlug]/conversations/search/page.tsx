import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewConversations } from "@/lib/roles";
import { ConversationsClient } from "./search-client";

export const metadata = { title: "Search transcripts" };

/**
 * SPEC-W117: OpenSearch-backed advanced search across conversation
 * transcripts (turn-level `conversations` index fed by the W10
 * TranscriptIndexer). Staff-grade surface (owner/admin/staff — the
 * agents-registry precedent): transcripts are operational data.
 */
export default async function ConversationsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  if (!canViewConversations(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <ConversationsClient orgSlug={orgSlug} />;
}
