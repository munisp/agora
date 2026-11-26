import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewLocations } from "@/lib/roles";
import { LocationsClient } from "./locations-client";

export const metadata = { title: "Locations" };

export default async function LocationsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W8 Part C): locations is visible to
  // owner/admin/staff; viewers and analysts are bounced to the overview.
  const session = await auth();
  if (!canViewLocations(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <LocationsClient orgSlug={orgSlug} />;
}
