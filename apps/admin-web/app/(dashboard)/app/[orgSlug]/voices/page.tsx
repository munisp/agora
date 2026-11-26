import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canEnrollVoices, canViewVoices } from "@/lib/roles";
import { VoicesClient } from "./voices-client";

export const metadata = { title: "Voices" };

export default async function VoicesPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  // Server-side role guard (SPEC-W10 Part C): the voices studio is visible to
  // owner/admin/staff; viewers and analysts are bounced to the overview.
  const session = await auth();
  if (!canViewVoices(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  // Brand-voice enrollment (voice cloning) is owner/admin only — staff can
  // browse and preview but never enroll.
  return (
    <VoicesClient
      orgSlug={orgSlug}
      canEnroll={canEnrollVoices(session?.realmRoles)}
    />
  );
}
