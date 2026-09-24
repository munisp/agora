import { notFound } from "next/navigation";
import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { PublicDashboardClient } from "./dashboard-client";
import type { PublicSite } from "@/lib/types";

export async function generateMetadata({
  params,
}: {
  params: Promise<{ siteSlug: string }>;
}) {
  const { siteSlug } = await params;
  try {
    const site = await serverApi<PublicSite>(
      `/api/bookings/public/sites/${siteSlug}`,
      // SPEC-W46 AW-3: semi-static public site data — 60s Data Cache TTL.
      { anonymous: true, revalidate: 60 },
    );
    const brand =
      site.theme?.brandName ?? site.theme?.brand_name ?? site.business_name;
    return { title: `Service dashboard · ${brand}` };
  } catch {
    return { title: "Service dashboard" };
  }
}

export default async function PublicDashboardPage({
  params,
}: {
  params: Promise<{ siteSlug: string }>;
}) {
  const { siteSlug } = await params;

  let site: PublicSite;
  try {
    site = await serverApi<PublicSite>(
      `/api/bookings/public/sites/${siteSlug}`,
      // SPEC-W46 AW-3: semi-static public site data — 60s Data Cache TTL.
      { anonymous: true, revalidate: 60 },
    );
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) notFound();
    throw e;
  }
  if (!site.published) notFound();

  return <PublicDashboardClient site={site} />;
}
