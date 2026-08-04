import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { hasAnyRole } from "@/lib/roles";
import { Crm360Client } from "./crm-360-client";

export const metadata = { title: "CRM 360" };

/**
 * CRM-360 app (SPEC-W20 Agent A): unified customer profile — contact
 * search, tags, notes, 360 aggregation and timeline. Server-side role
 * guard mirrors the growth/helpdesk page pattern — this is an operator
 * surface (owner/admin/staff); viewers/analysts are bounced home. Note
 * and tag mutations are re-checked by booking-service perms
 * (manage_bookings); reads ride view_analytics.
 */
export default async function Crm360Page({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  const roles = session?.realmRoles;
  if (!hasAnyRole(roles, ["owner", "admin", "staff"])) {
    redirect(`/app/${orgSlug}`);
  }
  return <Crm360Client orgSlug={orgSlug} canWork />;
}
