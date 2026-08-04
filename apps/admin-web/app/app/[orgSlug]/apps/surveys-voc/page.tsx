import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { hasAnyRole } from "@/lib/roles";
import { SurveysVocClient } from "./surveys-voc-client";

export const metadata = { title: "Surveys & VoC" };

/**
 * Surveys & VoC (NPS / CSAT / CES) — SPEC-W20 Agent B. Server-side role
 * guard per the growth-page pattern: reads are view_analytics
 * (owner/admin/analyst), writes are manage_bookings (owner/admin/staff),
 * so the page admits the union and passes canWrite down. lib/roles.ts is a
 * shared file (not owned this wave), so the role sets are composed here
 * from the exported hasAnyRole helper instead of adding new helpers.
 */
export default async function SurveysVocPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  const roles = session?.realmRoles;
  const canView = hasAnyRole(roles, ["owner", "admin", "staff", "analyst"]);
  if (!canView) {
    redirect(`/app/${orgSlug}`);
  }
  const canWrite = hasAnyRole(roles, ["owner", "admin", "staff"]);
  return <SurveysVocClient orgSlug={orgSlug} canWrite={canWrite} />;
}
