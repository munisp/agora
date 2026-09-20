import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { OpsAlertsClient } from "./ops-alerts-client";

export const metadata = { title: "Ops alerts" };

/**
 * SPEC-W45 ORPH O15: ops alerts read-back. The gateway route
 * (/api/notifications/v1/ops-alerts) requires an admin realm role
 * (admin or platform-admin) — the page applies the same gate client-side.
 */
export default async function OpsAlertsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  const roles = session?.realmRoles ?? [];
  if (!roles.includes("admin") && !roles.includes("platform-admin")) {
    redirect(`/app/${orgSlug}`);
  }
  return <OpsAlertsClient orgSlug={orgSlug} />;
}
