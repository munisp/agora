import { DocumentChecksClient } from "./document-checks-client";

export const metadata = { title: "Document checks" };

export default async function DocumentChecksPage({ params }: { params: Promise<{ orgSlug: string }> }) {
  const { orgSlug } = await params;
  return <DocumentChecksClient orgSlug={orgSlug} />;
}
