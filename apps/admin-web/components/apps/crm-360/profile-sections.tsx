"use client";

/**
 * 360 profile sections (SPEC-W20 Agent A): stat tiles + tickets /
 * bookings / active work orders tables, each with an honest empty state
 * (the backend degrades missing source tables to empty arrays — an empty
 * section can mean "nothing yet" OR "that app is not deployed"; the copy
 * says so). Pure presentational.
 */
import * as React from "react";
import { Coins, LifeBuoy, ShieldCheck } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDateTime } from "@/lib/utils";
import { statusVariant, type Profile360 } from "./types";

function SectionEmpty({ children }: { children: React.ReactNode }) {
  return (
    <p className="px-4 py-6 text-sm text-muted-foreground">{children}</p>
  );
}

export function ProfileSections({ profile }: { profile: Profile360 }) {
  const { contact, wallet, consent, open_ticket_count: openTickets } = profile;
  return (
    <div className="space-y-4">
      {/* Stat tiles */}
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
        <Card>
          <CardHeader className="pb-2">
            <CardDescription className="flex items-center gap-1.5">
              <LifeBuoy className="h-4 w-4" /> Open tickets
            </CardDescription>
            <CardTitle className="text-2xl">{openTickets}</CardTitle>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardDescription className="flex items-center gap-1.5">
              <Coins className="h-4 w-4" /> Loyalty wallet
            </CardDescription>
            <CardTitle className="text-2xl">
              {wallet ? (
                <>
                  {wallet.balance.toLocaleString()}
                  <span className="ml-2 text-sm font-normal text-muted-foreground">
                    pts{wallet.tier ? ` · ${wallet.tier}` : ""}
                  </span>
                </>
              ) : (
                <span className="text-sm font-normal text-muted-foreground">
                  No wallet — the loyalty-wallet app may not be deployed for
                  this tenant.
                </span>
              )}
            </CardTitle>
          </CardHeader>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardDescription className="flex items-center gap-1.5">
              <ShieldCheck className="h-4 w-4" /> Consent
            </CardDescription>
            <CardTitle className="text-2xl">
              {consent ? (
                <Badge variant="success">{consent}</Badge>
              ) : (
                <span className="text-sm font-normal text-muted-foreground">
                  Not resolvable — consent records live in identity-service;
                  see docs/apps/crm-360.md.
                </span>
              )}
            </CardTitle>
          </CardHeader>
        </Card>
      </div>

      {contact.notes ? (
        <Card>
          <CardHeader className="pb-2">
            <CardDescription>Contact record notes (legacy field)</CardDescription>
          </CardHeader>
          <CardContent>
            <p className="whitespace-pre-wrap text-sm text-foreground">{contact.notes}</p>
          </CardContent>
        </Card>
      ) : null}

      {/* Tickets */}
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-base">Latest tickets</CardTitle>
          <CardDescription>Newest 5 helpdesk tickets, any status.</CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          {profile.tickets.length === 0 ? (
            <SectionEmpty>
              No tickets — or the helpdesk app is not deployed for this tenant.
            </SectionEmpty>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Subject</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Priority</TableHead>
                  <TableHead>Opened</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {profile.tickets.map((t) => (
                  <TableRow key={t.id}>
                    <TableCell className="font-medium">{t.subject}</TableCell>
                    <TableCell>
                      <Badge variant={statusVariant(t.status)}>{t.status}</Badge>
                    </TableCell>
                    <TableCell>{t.priority}</TableCell>
                    <TableCell>{formatDateTime(t.created_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Bookings */}
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-base">Recent bookings</CardTitle>
          <CardDescription>Newest 5 bookings by scheduled start.</CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          {profile.bookings.length === 0 ? (
            <SectionEmpty>No bookings recorded for this contact yet.</SectionEmpty>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Starts</TableHead>
                  <TableHead>Ends</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Source</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {profile.bookings.map((b) => (
                  <TableRow key={b.id}>
                    <TableCell className="font-medium">{formatDateTime(b.starts_at)}</TableCell>
                    <TableCell>{formatDateTime(b.ends_at)}</TableCell>
                    <TableCell>
                      <Badge variant={statusVariant(b.status)}>{b.status.replace("_", " ")}</Badge>
                    </TableCell>
                    <TableCell>{b.source}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Active work orders */}
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="text-base">Active work orders</CardTitle>
          <CardDescription>Field work in flight (completed work drops off this list).</CardDescription>
        </CardHeader>
        <CardContent className="p-0">
          {profile.work_orders.length === 0 ? (
            <SectionEmpty>
              No active work — or the field-service app is not deployed for
              this tenant.
            </SectionEmpty>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Title</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Scheduled</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {profile.work_orders.map((w) => (
                  <TableRow key={w.id}>
                    <TableCell className="font-medium">{w.title}</TableCell>
                    <TableCell>
                      <Badge variant={statusVariant(w.status)}>{w.status.replace("_", " ")}</Badge>
                    </TableCell>
                    <TableCell>
                      {w.scheduled_start ? formatDateTime(w.scheduled_start) : "—"}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
