import { notFound } from "next/navigation";
import { serverApi, unwrapList } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { PublicBookingClient } from "./public-booking-client";
import type { Offering, PublicSite } from "@/lib/types";

export async function generateMetadata({
  params,
}: {
  params: Promise<{ siteSlug: string }>;
}) {
  const { siteSlug } = await params;
  try {
    const site = await serverApi<PublicSite>(
      `/api/bookings/public/sites/${siteSlug}`,
      { anonymous: true, revalidate: 60 },
    );
    const brand =
      site.theme?.brandName ?? site.theme?.brand_name ?? site.business_name;
    return { title: `Book · ${brand}` };
  } catch {
    return { title: "Booking" };
  }
}

export default async function PublicBookingPage({
  params,
}: {
  params: Promise<{ siteSlug: string }>;
}) {
  const { siteSlug } = await params;

  // SPEC-W46 AW-3: site + offerings fire in parallel (the offerings leg no
  // longer waits on the published check — a wasted fetch on unpublished
  // sites is cheaper than a serial RTT on every published hit) and both are
  // cached in the Data Cache for 60s (semi-static public data).
  const [siteRes, offeringsRes] = await Promise.allSettled([
    serverApi<PublicSite>(`/api/bookings/public/sites/${siteSlug}`, {
      anonymous: true,
      revalidate: 60,
    }),
    serverApi<unknown>(`/api/bookings/public/sites/${siteSlug}/offerings`, {
      anonymous: true,
      revalidate: 60,
    }),
  ]);

  if (siteRes.status === "rejected") {
    if (siteRes.reason instanceof ApiError && siteRes.reason.status === 404) {
      notFound();
    }
    throw siteRes.reason;
  }
  const site = siteRes.value;
  if (!site.published) notFound();

  const offerings: Offering[] =
    offeringsRes.status === "fulfilled"
      ? unwrapList<Offering>(offeringsRes.value).filter((o) => o.bookable)
      : [];

  return <PublicBookingClient site={site} offerings={offerings} />;
}
