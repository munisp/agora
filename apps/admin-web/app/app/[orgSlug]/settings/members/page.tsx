import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { MembersClient } from "./members-client";
import type { IdentityMember } from "./members-client";

export const metadata = { title: "Members" };

/**
 * SPEC-W46 AW-2: the member list is fetched server-side (same serverApi
 * pattern as the overview page) and passed to the client as initialData,
 * removing the HTML→hydrate→BFF waterfall. Mutations revalidate via
 * router.refresh() in the client.
 */
export default async function MembersSettingsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;

  let initialMembers: IdentityMember[] = [];
  let initialError: string | null = null;
  try {
    const data = await serverApi<{ members?: IdentityMember[] }>(
      `/api/identity/v1/tenants/${orgSlug}/members`,
    );
    initialMembers = data.members ?? [];
  } catch (e) {
    initialError =
      e instanceof ApiError ? e.message : "Failed to load members.";
  }

  return (
    <MembersClient
      orgSlug={orgSlug}
      initialMembers={initialMembers}
      initialError={initialError}
    />
  );
}
