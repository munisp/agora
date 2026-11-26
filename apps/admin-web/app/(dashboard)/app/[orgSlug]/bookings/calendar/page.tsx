import { auth } from "@/lib/auth";
import { CalendarClient } from "./calendar-client";

export const metadata = { title: "Advisor calendar" };

/** One column per team member, the day's bookings by time; updates live as bookings are made. */
export default async function CalendarPage({
  params,
  searchParams,
}: {
  params: Promise<{ orgSlug: string }>;
  searchParams: Promise<{ date?: string }>;
}) {
  const { orgSlug } = await params;
  const { date } = await searchParams;
  const session = await auth();
  return <CalendarClient orgSlug={orgSlug} token={session?.accessToken} initialDate={date} />;
}
