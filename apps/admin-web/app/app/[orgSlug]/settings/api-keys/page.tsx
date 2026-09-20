import { ApiKeysClient } from "./api-keys-client";

export const metadata = { title: "API keys" };

export default async function ApiKeysSettingsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  return <ApiKeysClient orgSlug={orgSlug} />;
}
