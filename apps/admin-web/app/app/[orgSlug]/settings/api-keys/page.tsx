import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { ApiKeysClient } from "./api-keys-client";
import type { ApiKey } from "./api-keys-client";

export const metadata = { title: "API keys" };

/**
 * SPEC-W46 AW-2: the key list is fetched server-side (same serverApi
 * pattern as the overview page) and passed to the client as initialData,
 * removing the HTML→hydrate→BFF waterfall. Mutations revalidate via
 * router.refresh() in the client.
 */
export default async function ApiKeysSettingsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;

  let initialKeys: ApiKey[] = [];
  let initialError: string | null = null;
  try {
    const data = await serverApi<{ api_keys?: ApiKey[] }>(
      `/api/identity/v1/tenants/${orgSlug}/api-keys`,
    );
    initialKeys = data.api_keys ?? [];
  } catch (e) {
    initialError =
      e instanceof ApiError ? e.message : "Failed to load API keys.";
  }

  return (
    <ApiKeysClient
      orgSlug={orgSlug}
      initialKeys={initialKeys}
      initialError={initialError}
    />
  );
}
