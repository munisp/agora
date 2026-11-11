import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { WaitlistClient } from "./waitlist-client";
import type { Offering, PublicSite, WaitlistEntry } from "@/lib/types";

export const metadata = { title: "Waitlist" };

/**
 * SPEC-W47 UIWEB-12: tenant-admin waitlist console. The public token claim
 * page (/p/[siteSlug]/claim) existed since W45 K13 but staff had no way to
 * see or add entries. SSR initialData pattern (W46 AW-2): entries +
 * offerings + the site slug (for claim links) load server-side.
 *
 * Notification is AUTOMATED: the notification-worker WaitlistBackfillWorkflow
 * notifies the top-3 waiting entries with their single-use claim link
 * whenever a booking is cancelled. Since W48 the console also offers manual
 * per-entry actions: POST /v1/waitlist/{id}/notify (resend the claim link)
 * and POST /v1/waitlist/{id}/expire. A bookingops sweep also marks entries
 * expired (~every 5 minutes) once their window passes; claims on expired
 * windows answer 410.
 */
export default async function WaitlistPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();
  if (!session) redirect(`/app/${orgSlug}`);

  let initialEntries: WaitlistEntry[] = [];
  let initialOfferings: Offering[] = [];
  let siteSlug = orgSlug;
  let initialError: string | null = null;
  try {
    const data = await serverApi<{ entries?: WaitlistEntry[] }>(
      "/api/bookings/v1/waitlist",
      { query: { tenant: orgSlug } },
    );
    initialEntries = data.entries ?? [];
  } catch (e) {
    initialError =
      e instanceof ApiError ? e.message : "Failed to load the waitlist.";
  }
  // Soft-fail enrichment reads: the page still works without them.
  try {
    const offerings = await serverApi<Offering[] | { items: Offering[] }>(
      "/api/bookings/v1/offerings",
      { query: { tenant: orgSlug } },
    );
    initialOfferings = Array.isArray(offerings)
      ? offerings
      : (offerings.items ?? []);
  } catch {
    initialOfferings = [];
  }
  try {
    const site = await serverApi<PublicSite>("/api/bookings/v1/site", {
      query: { tenant: orgSlug },
    });
    if (site.site_slug) siteSlug = site.site_slug;
  } catch {
    /* fall back to the tenant slug */
  }

  return (
    <WaitlistClient
      orgSlug={orgSlug}
      siteSlug={siteSlug}
      initialEntries={initialEntries}
      initialOfferings={initialOfferings}
      initialError={initialError}
    />
  );
}
