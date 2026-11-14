import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import type { TeamMember } from "@/lib/types";
import { TeamClient } from "./team-client";

export const metadata = { title: "Team" };

/**
 * SPEC-W46 AW-2: the team list is fetched server-side (same serverApi
 * pattern as the overview page) and passed to the client as initialData.
 * Mutations revalidate via router.refresh() in the client.
 */
export default async function TeamPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;

  let initialMembers: TeamMember[] = [];
  let initialError: string | null = null;
  try {
    const data = await serverApi<TeamMember[] | { items: TeamMember[] }>(
      "/api/bookings/v1/team-members",
      { query: { tenant: orgSlug } },
    );
    // Same unwrap semantics the client used.
    initialMembers = Array.isArray(data) ? data : (data.items ?? []);
  } catch (e) {
    initialError = e instanceof ApiError ? e.message : "Failed to load team.";
  }

  return (
    <TeamClient
      orgSlug={orgSlug}
      initialMembers={initialMembers}
      initialError={initialError}
    />
  );
}
