import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { canViewKycReview } from "@/lib/roles";
import { KycReviewClient } from "./kyc-client";

export const metadata = { title: "KYC review" };

/**
 * SPEC-W58 Scope D: KYC document review surface. Server-side role guard
 * (geo-campaigns pattern): extracted KYC PII (names, ID numbers) is served
 * only through the owner/admin gate (RECON §5 R4) — everyone else is
 * bounced to the overview, and the booking-service KYC routes enforce the
 * same owner/admin rule independently.
 */
export default async function KycReviewPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  if (!canViewKycReview(session?.realmRoles)) {
    redirect(`/app/${orgSlug}`);
  }
  return <KycReviewClient orgSlug={orgSlug} />;
}
