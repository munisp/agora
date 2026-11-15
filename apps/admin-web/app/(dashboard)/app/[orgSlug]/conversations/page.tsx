import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { ConversationsClient } from "./conversations-client";
import type { ConversationSummary } from "./conversations-client";

export const metadata = { title: "Conversations" };

/**
 * Every web chat and voice call the receptionist handled, with full transcripts (conversation-service
 * /v1/conversations, written by the voice runtime's transcript recorder). ?contact=<phone> narrows to one customer —
 * the Contacts page links here.
 */
export default async function ConversationsPage({
  params,
  searchParams,
}: {
  params: Promise<{ orgSlug: string }>;
  searchParams: Promise<{ contact?: string }>;
}) {
  const { orgSlug } = await params;
  const { contact } = await searchParams;
  let initial: ConversationSummary[] = [];
  let initialError: string | null = null;
  try {
    const data = await serverApi<{ conversations?: ConversationSummary[] }>("/api/conversations/v1/conversations", {
      query: { tenant: orgSlug, limit: 100, ...(contact ? { contact } : {}) },
    });
    initial = data.conversations ?? [];
  } catch (e) {
    initialError = e instanceof ApiError ? e.message : "Failed to load conversations.";
  }
  return <ConversationsClient orgSlug={orgSlug} contact={contact ?? null} initial={initial} initialError={initialError} />;
}
