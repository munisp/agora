import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewLocations, hasAnyRole } from "@/lib/roles";
import { CasesClient } from "./cases-client";

export const metadata = { title: "Cases" };

export default async function CasesPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W32 WS-C): civic cases are operational —
  // same gate as locations (owner/admin/staff); viewers and analysts are
  // bounced to the overview.
  const session = await auth();
  if (!canViewLocations(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  // Anonymous reporter unmasking is owner/admin only (SPEC §2/§4 gate 4);
  // staff operate the queue but never see a masked reporter's identity.
  return (
    <CasesClient
      orgSlug={orgSlug}
      canRevealReporter={hasAnyRole(session?.realmRoles, ["owner", "admin"])}
    />
  );
}
