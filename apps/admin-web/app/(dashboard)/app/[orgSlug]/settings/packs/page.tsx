import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { hasAnyRole, type RealmRole } from "@/lib/roles";
import { PacksClient } from "./packs-client";

export const metadata = { title: "Industry packs" };

/**
 * Role gates are declared locally (not in lib/roles.ts) to keep this page
 * self-contained: browsing the pack catalog mirrors the voices studio
 * (owner/admin/staff), while activating a pack is owner/admin only — the
 * same manage split as brand-voice enrollment and geo campaigns.
 */
const PACKS_VIEW_ROLES: readonly RealmRole[] = ["owner", "admin", "staff"];
const PACKS_MANAGE_ROLES: readonly RealmRole[] = ["owner", "admin"];

export default async function PacksPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side guard, same pattern as the voices studio page.
  const session = await auth();
  if (!hasAnyRole(session?.realmRoles, PACKS_VIEW_ROLES)) {
    redirect(`/app/${orgSlug}`);
  }
  return (
    <PacksClient
      orgSlug={orgSlug}
      canManage={hasAnyRole(session?.realmRoles, PACKS_MANAGE_ROLES)}
    />
  );
}
