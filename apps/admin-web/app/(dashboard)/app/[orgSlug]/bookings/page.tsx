import { auth } from "@/lib/auth";
import { serverApi, unwrapList } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import {
  DEFAULT_BOOKING_LABELS,
  resolveBookingLabels,
} from "@/lib/terminology";
import { addDays, toISODate } from "@/lib/utils";
import type { Booking, Tenant } from "@/lib/types";
import { BookingsClient } from "./bookings-client";

export const metadata = { title: "Bookings" };

/**
 * SPEC-W46 AW-2: the default range ("upcoming") and the tenant terminology
 * labels are fetched server-side in parallel and handed to the client as
 * initialData — the page paints with data instead of a Loading shell. Range
 * switches, live-refresh and mutation reloads stay client-side.
 * SPEC-W46 AW-4: the list is bounded (limit=100 pages, "load more" in the
 * client refetches with a larger limit — the endpoint has no cursor).
 */
export default async function BookingsPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const session = await auth();

  // Keep in sync with rangeQuery("upcoming") in bookings-client.tsx.
  const now = new Date();
  const from = toISODate(addDays(now, 1));
  const to = toISODate(addDays(now, 30));

  const [tenantRes, bookingsRes] = await Promise.allSettled([
    serverApi<Tenant>(`/api/identity/v1/tenants/${orgSlug}`),
    serverApi<unknown>("/api/bookings/v1/bookings", {
      query: { tenant: orgSlug, from, to, limit: 100 },
    }),
  ]);

  // Terminology is cosmetic — keep the defaults if identity is down (same
  // fallback the client previously applied).
  const initialLabels =
    tenantRes.status === "fulfilled"
      ? resolveBookingLabels(tenantRes.value)
      : DEFAULT_BOOKING_LABELS;
  const initialBookings =
    bookingsRes.status === "fulfilled"
      ? unwrapList<Booking>(bookingsRes.value)
          .slice()
          .sort((a, b) => a.starts_at.localeCompare(b.starts_at))
      : [];
  const initialError =
    bookingsRes.status === "rejected"
      ? bookingsRes.reason instanceof ApiError
        ? bookingsRes.reason.message
        : "Failed to load bookings."
      : null;

  return (
    <BookingsClient
      orgSlug={orgSlug}
      token={session?.accessToken}
      initialBookings={initialBookings}
      initialLabels={initialLabels}
      initialError={initialError}
    />
  );
}
