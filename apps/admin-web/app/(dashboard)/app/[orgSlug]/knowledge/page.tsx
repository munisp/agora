import { KnowledgeClient } from "./knowledge-client";

export const metadata = { title: "Knowledge" };

export default async function KnowledgePage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  return <KnowledgeClient orgSlug={orgSlug} />;
}
