import { ClaimClient } from "./claim-client";

export const metadata = { title: "Claim your slot" };

/**
 * SPEC-W45 K13: public waitlist-claim landing page. The claim link delivered
 * by the waitlist backfill notification points here
 * (/p/[slug]/claim?token=...). The token is the capability that authorizes
 * the claim — no sign-in required (booking-service claim endpoints are
 * token-authorized, not Permify-guarded).
 */
export default async function WaitlistClaimPage({
  params,
  searchParams,
}: {
  params: Promise<{ siteSlug: string }>;
  searchParams: Promise<{ token?: string }>;
}) {
  const { siteSlug } = await params;
  const { token } = await searchParams;
  return <ClaimClient tenantSlug={siteSlug} token={token ?? ""} />;
}
