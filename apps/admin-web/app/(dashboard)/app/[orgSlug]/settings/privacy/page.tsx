import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewGeoCampaigns } from "@/lib/roles";
import { PrivacyClient } from "./privacy-client";

export const metadata = { title: "Privacy" };

/**
 * SPEC-W47 UIWEB-17: NDPA data-subject rights console (staff-triggered
 * export/erase). Mutations are manage_bookings-gated server-side; the page
 * is additionally restricted to owner/admin because erasure is destructive
 * and irreversible. There is no workflow list/status read-back endpoint —
 * the console shows the workflow id returned by the service and where the
 * export bundle lands.
 */
export default async function PrivacyPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  if (!canViewGeoCampaigns(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <PrivacyClient orgSlug={orgSlug} />;
}
