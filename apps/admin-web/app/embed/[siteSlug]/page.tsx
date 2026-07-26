import { notFound } from "next/navigation";
import { serverApi, unwrapList } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { PublicBookingClient } from "../../p/[siteSlug]/public-booking-client";
import { UiActionsBridge } from "../ui-actions-bridge";
import type { Offering, PublicSite } from "@/lib/types";

export const metadata = { title: "Booking widget" };

/**
 * Chromeless booking + chat page, intended to be loaded inside an iframe via
 * the /embed.js loader (see docs/embedding.md). Same data as /p/[siteSlug]
 * but without the marketing header, voice button or footer.
 */
export default async function EmbedPage({
  params,
}: {
  params: Promise<{ siteSlug: string }>;
}) {
  const { siteSlug } = await params;

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

  let offerings: Offering[] = [];
  try {
    const data = await serverApi<unknown>(
      `/api/bookings/public/sites/${siteSlug}/offerings`,
      { anonymous: true },
    );
    offerings = unwrapList<Offering>(data).filter((o) => o.bookable);
  } catch {
    offerings = [];
  }

  return (
    <>
      <PublicBookingClient site={site} offerings={offerings} embed />
      {/* SPEC-W9 Part B: executes agent-driven UI actions (navigate /
          highlight / prefill_booking) tapped from /voice/chat responses. */}
      <UiActionsBridge
        offerings={offerings.map((o) => ({ id: o.id, name: o.name }))}
      />
    </>
  );
}

