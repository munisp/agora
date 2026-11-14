import { VoiceAgentClient } from "./voice-agent-client";

export const metadata = { title: "Voice Agent" };

export default async function VoiceAgentPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  return <VoiceAgentClient orgSlug={orgSlug} />;
}
