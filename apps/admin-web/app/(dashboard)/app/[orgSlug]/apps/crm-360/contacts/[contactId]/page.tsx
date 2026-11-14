import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { hasAnyRole } from "@/lib/roles";
import { ContactProfileClient } from "./contact-profile-client";

export const metadata = { title: "Contact 360" };

/**
 * CRM-360 contact profile page (SPEC-W20 Agent A): the unified 360 view +
 * timeline + notes editor + tag chips for one contact. Same role guard
 * as the search page (owner/admin/staff; viewers/analysts bounced home).
 */
export default async function ContactProfilePage({
  params,
}: {
  params: Promise<{ orgSlug: string; contactId: string }>;
}) {
  const { orgSlug, contactId } = await params;
  const session = await auth();
  const roles = session?.realmRoles;
  if (!hasAnyRole(roles, ["owner", "admin", "staff"])) {
    redirect(`/app/${orgSlug}`);
  }
  return <ContactProfileClient orgSlug={orgSlug} contactId={contactId} canWork />;
}
