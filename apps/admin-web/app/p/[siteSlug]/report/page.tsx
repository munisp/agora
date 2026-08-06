import { notFound } from "next/navigation";
import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { PublicReportClient } from "./report-client";
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
      { anonymous: true },
    );
    const brand =
      site.theme?.brandName ?? site.theme?.brand_name ?? site.business_name;
    return { title: `Report an issue · ${brand}` };
  } catch {
    return { title: "Report an issue" };
  }
}

export default async function PublicReportPage({
  params,
}: {
  params: Promise<{ siteSlug: string }>;
}) {
  const { siteSlug } = await params;

  // Branding comes from the existing public-site contract (same as the
  // booking page); the civic intake itself is unauthenticated and hits the
  // public civic endpoints from the browser (SPEC-W32 §3 WS-A).
  let site: PublicSite;
  try {
    site = await serverApi<PublicSite>(
      `/api/bookings/public/sites/${siteSlug}`,
      { anonymous: true },
    );
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) notFound();
    throw e;
  }
  if (!site.published) notFound();

  return <PublicReportClient site={site} />;
}
