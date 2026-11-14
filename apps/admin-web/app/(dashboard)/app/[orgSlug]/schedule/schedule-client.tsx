"use client";

import * as React from "react";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { BookingStatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDateTime, titleCase } from "@/lib/utils";
import type { Booking } from "@/lib/types";

/**
 * Staff self-service view (I19): the bookings assigned to the signed-in
 * team member. booking-service resolves "me" from the JWT email claim via
 * team_members.email (GET /v1/bookings?mine=true).
 */
export function ScheduleClient({
  orgSlug,
  email,
}: {
  orgSlug: string;
  token?: string;
  email?: string;
}) {
  const [bookings, setBookings] = React.useState<Booking[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [unsupported, setUnsupported] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    setUnsupported(false);
    try {
      const data = await api.get<Booking[] | { items: Booking[] }>(
        "/api/bookings/v1/bookings",
        { tenant: orgSlug, mine: true },
      );
      const items = Array.isArray(data) ? data : (data.items ?? []);
      setBookings(
        items
          .slice()
          .sort((a, b) => a.starts_at.localeCompare(b.starts_at)),
      );
    } catch (e) {
      if (e instanceof ApiError && (e.status === 404 || e.status === 501)) {
        setUnsupported(true);
      } else {
        setError(e instanceof ApiError ? e.message : "Failed to load your schedule.");
      }
    } finally {
      setLoading(false);
    }
  }, [orgSlug]);

  React.useEffect(() => {
    void load();
  }, [load]);

  const now = new Date().toISOString();
  const upcoming = bookings.filter((b) => b.ends_at >= now && b.status !== "cancelled");
  const past = bookings.filter((b) => b.ends_at < now || b.status === "cancelled");

  return (
    <div>
      <PageHeader
        title="My schedule"
        description={`Bookings assigned to you${email ? ` (${email})` : ""} across all offerings.`}
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => void load()}
            disabled={loading}
          >
            <RefreshCw className={loading ? "h-3.5 w-3.5 animate-spin" : "h-3.5 w-3.5"} />
            Refresh
          </Button>
        }
      />
      {error ? <ErrorNote message={error} /> : null}
      {unsupported ? (
        <ErrorNote message="This deployment's booking-service does not expose staff self-service yet (GET /v1/bookings?mine=true). Ask an admin to view the shared Bookings page instead." />
      ) : null}

      <div className="mb-3 flex items-center gap-2">
        <Badge variant="secondary">{upcoming.length} upcoming</Badge>
        <Badge variant="outline">{past.length} past / cancelled</Badge>
      </div>

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">When</TableHead>
              <TableHead>Guest</TableHead>
              <TableHead>Offering</TableHead>
              <TableHead>Source</TableHead>
              <TableHead className="pr-5">Status</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {bookings.length === 0 ? (
              <TableEmpty colSpan={5}>
                {loading
                  ? "Loading…"
                  : "Nothing assigned to you yet. Your email must match a team member record."}
              </TableEmpty>
            ) : (
              bookings.map((b) => {
                const isPast = b.ends_at < now || b.status === "cancelled";
                return (
                  <TableRow key={b.id} className={isPast ? "opacity-60" : undefined}>
                    <TableCell className="pl-5 font-medium">
                      {formatDateTime(b.starts_at)}
                    </TableCell>
                    <TableCell>
                      <div>{b.contact_name ?? b.contact_id}</div>
                      {b.contact_phone ? (
                        <div className="text-xs text-muted-foreground">
                          {b.contact_phone}
                        </div>
                      ) : null}
                    </TableCell>
                    <TableCell>{b.offering_name ?? b.offering_id}</TableCell>
                    <TableCell>
                      <Badge variant="outline">{titleCase(b.source)}</Badge>
                    </TableCell>
                    <TableCell className="pr-5">
                      <BookingStatusBadge status={b.status} />
                    </TableCell>
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </Card>
    </div>
  );
}
