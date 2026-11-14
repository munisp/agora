import Link from "next/link";
import { ArrowRight, CalendarCheck, Clock, Hourglass, Store } from "lucide-react";
import { serverApi, unwrapList } from "@/lib/server-api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { BookingStatusBadge } from "@/components/status-badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { addDays, formatTime, toISODate } from "@/lib/utils";
import type { Booking, Offering } from "@/lib/types";

export const metadata = { title: "Overview" };

function StatCard({
  label,
  value,
  icon: Icon,
  unavailable = false,
}: {
  label: string;
  value: string | number;
  icon: React.ComponentType<{ className?: string }>;
  /** W44/F15-11: explicit error state — never render a misleading 0. */
  unavailable?: boolean;
}) {
  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between space-y-0 pb-2">
        <p className="text-sm font-medium text-muted-foreground">{label}</p>
        <Icon className="h-4 w-4 text-muted-foreground" />
      </CardHeader>
      <CardContent>
        {unavailable ? (
          <>
            <p className="text-3xl font-bold text-muted-foreground">—</p>
            <p className="mt-1 text-xs text-destructive">Unavailable</p>
          </>
        ) : (
          <p className="text-3xl font-bold">{value}</p>
        )}
      </CardContent>
    </Card>
  );
}

export default async function OverviewPage({
  params,
}: {
  params: Promise<{ orgSlug: string }>;
}) {
  const { orgSlug } = await params;
  const today = toISODate(new Date());
  const tomorrow = toISODate(addDays(new Date(), 1));
  const nextWeek = toISODate(addDays(new Date(), 7));

  let todayBookings: Booking[] = [];
  let upcoming: Booking[] = [];
  let offerings: Offering[] = [];
  let error: string | null = null;

  try {
    const [todayRes, upcomingRes, offeringsRes] = await Promise.all([
      serverApi<unknown>("/api/bookings/v1/bookings", {
        query: { tenant: orgSlug, from: today, to: tomorrow },
      }),
      serverApi<unknown>("/api/bookings/v1/bookings", {
        query: { tenant: orgSlug, from: tomorrow, to: nextWeek },
      }),
      serverApi<unknown>("/api/bookings/v1/offerings", {
        query: { tenant: orgSlug },
      }),
    ]);
    todayBookings = unwrapList<Booking>(todayRes);
    upcoming = unwrapList<Booking>(upcomingRes);
    offerings = unwrapList<Offering>(offeringsRes);
  } catch (e) {
    error = e instanceof Error ? e.message : "Failed to load dashboard data.";
  }

  const pending = todayBookings.filter((b) => b.status === "pending").length;

  return (
    <div>
      <PageHeader
        title="Today at a glance"
        description={`Overview for ${orgSlug} · ${new Date().toLocaleDateString("en-US", { dateStyle: "full" })}`}
        actions={
          <Link href={`/app/${orgSlug}/bookings`}>
            <Button variant="outline" size="sm">
              All bookings <ArrowRight className="h-3.5 w-3.5" />
            </Button>
          </Link>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard
          label="Bookings today"
          value={todayBookings.length}
          icon={CalendarCheck}
          unavailable={error !== null}
        />
        <StatCard
          label="Awaiting confirmation"
          value={pending}
          icon={Hourglass}
          unavailable={error !== null}
        />
        <StatCard
          label="Next 7 days"
          value={upcoming.length}
          icon={Clock}
          unavailable={error !== null}
        />
        <StatCard
          label="Active offerings"
          value={offerings.filter((o) => o.bookable).length}
          icon={Store}
          unavailable={error !== null}
        />
      </div>

      <div className="mt-6 grid gap-6 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Today&apos;s schedule</CardTitle>
          </CardHeader>
          <CardContent className="px-0 pb-2">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-5">Time</TableHead>
                  <TableHead>Guest</TableHead>
                  <TableHead>Offering</TableHead>
                  <TableHead className="pr-5">Status</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {todayBookings.length === 0 ? (
                  <TableEmpty colSpan={4}>
                    {error ? "Unavailable while the gateway is down." : "Nothing scheduled today."}
                  </TableEmpty>
                ) : (
                  todayBookings
                    .slice()
                    .sort((a, b) => a.starts_at.localeCompare(b.starts_at))
                    .map((b) => (
                      <TableRow key={b.id}>
                        <TableCell className="pl-5 font-medium">
                          {formatTime(b.starts_at)}
                        </TableCell>
                        <TableCell>{b.contact_name ?? b.contact_id}</TableCell>
                        <TableCell>{b.offering_name ?? b.offering_id}</TableCell>
                        <TableCell className="pr-5">
                          <BookingStatusBadge status={b.status} />
                        </TableCell>
                      </TableRow>
                    ))
                )}
              </TableBody>
            </Table>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Coming up this week</CardTitle>
          </CardHeader>
          <CardContent>
            {upcoming.length === 0 ? (
              <p className="py-6 text-center text-sm text-muted-foreground">
                {error
                  ? "Unavailable while the gateway is down."
                  : "No upcoming bookings in the next 7 days."}
              </p>
            ) : (
              <ul className="divide-y divide-border">
                {upcoming
                  .slice()
                  .sort((a, b) => a.starts_at.localeCompare(b.starts_at))
                  .slice(0, 8)
                  .map((b) => (
                    <li
                      key={b.id}
                      className="flex items-center justify-between py-2.5 text-sm"
                    >
                      <div className="min-w-0">
                        <p className="truncate font-medium">
                          {b.contact_name ?? b.contact_id}
                        </p>
                        <p className="truncate text-xs text-muted-foreground">
                          {b.offering_name ?? b.offering_id}
                        </p>
                      </div>
                      <div className="flex items-center gap-3">
                        <span className="text-xs text-muted-foreground">
                          {new Date(b.starts_at).toLocaleDateString("en-US", {
                            weekday: "short",
                            month: "short",
                            day: "numeric",
                          })}{" "}
                          · {formatTime(b.starts_at)}
                        </span>
                        <BookingStatusBadge status={b.status} />
                      </div>
                    </li>
                  ))}
              </ul>
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
