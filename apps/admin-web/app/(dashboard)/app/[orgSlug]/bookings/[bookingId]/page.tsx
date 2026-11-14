import { redirect } from "next/navigation";
import { auth } from "@/lib/auth";
import { serverApi } from "@/lib/server-api";
import { ApiError } from "@/lib/api";
import { BookingDetailClient } from "./booking-detail-client";
import type { Booking } from "@/lib/types";

export const metadata = { title: "Booking" };

/**
 * SPEC-W47 UIWEB-10/11: per-booking detail surface. The row is fetched
 * server-side (W46 SSR initialData pattern) and handed to the client, which
 * owns the money actions (complete = deposit capture, refund = W45 K12
 * fail-closed rail). Any authenticated tenant member can view; mutations are
 * manage_bookings-gated server-side (and re-checked by the service).
 */
export default async function BookingDetailPage({
  params,
}: {
  params: Promise<{ orgSlug: string; bookingId: string }>;
}) {
  const { orgSlug, bookingId } = await params;
  const session = await auth();
  if (!session) redirect(`/app/${orgSlug}`);

  let booking: Booking | null = null;
  let initialError: string | null = null;
  try {
    booking = await serverApi<Booking>(
      `/api/bookings/v1/bookings/${encodeURIComponent(bookingId)}`,
      { query: { tenant: orgSlug } },
    );
  } catch (e) {
    initialError =
      e instanceof ApiError && e.status === 404
        ? "Booking not found."
        : e instanceof ApiError
          ? e.message
          : "Failed to load the booking — the booking service may be offline.";
  }

  return (
    <BookingDetailClient
      orgSlug={orgSlug}
      bookingId={bookingId}
      initialBooking={booking}
      initialError={initialError}
    />
  );
}
