import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewMediaInbox } from "@/lib/roles";
import { MediaInboxClient } from "./media-inbox-client";

export const metadata = { title: "Media inbox" };

/**
 * SPEC-W61 Scope D: inbound WhatsApp media review queue. Server-side role
 * guard (W58 KYC pattern): media inbox rows carry sender phone PII and a
 * link to the OCR-extracted document, so they are served only through the
 * owner/admin gate — everyone else is bounced to the overview, and the
 * booking-service media inbox routes enforce the same owner/admin rule
 * independently.
 */
export default async function MediaInboxPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  if (!canViewMediaInbox(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <MediaInboxClient orgSlug={orgSlug} />;
}
