import { auth } from "@/lib/auth";
import { hasAnyRole } from "@/lib/roles";
import { AppsClient } from "./apps-client";

export const metadata = { title: "Apps" };

/**
 * Apps management portal (SPEC-W18 Agent B, contract §2). The catalog grid
 * is viewable by any signed-in role; the lifecycle mutations (provision /
 * enable / disable / config) are owner/admin only — computed here from the
 * session (same server-side role-guard pattern as the W14 growth pages,
 * e.g. growth/rules/page.tsx) and enforced again by identity-service.
 */
export default async function AppsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  const canManage = hasAnyRole(session?.realmRoles, ["owner", "admin"]);
  return <AppsClient orgSlug={orgSlug} canManage={canManage} />;
}
