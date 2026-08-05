/**
 * SPEC-W28 WS-C: Person 360 — the Graph Explorer v1 single-person view.
 *
 * Server-rendered tables (no graph-viz lib, per §4 WS-C): contacts,
 * bookings, consents, referrals and messages of one Person node, fetched
 * from GET /v1/graph/persons/{id}. The graph holds no raw phone numbers
 * (hashed only, SPEC-W28 §3) — the view shows the hash, never PII.
 */
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
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDateTime } from "@/lib/utils";
import type { Person360 } from "./types";

function dash(v: unknown): string {
  if (v === null || v === undefined || v === "") return "—";
  if (typeof v === "boolean") return v ? "yes" : "no";
  return String(v);
}

function ts(v?: string): string {
  return v ? formatDateTime(v) : "—";
}

export function Person360View({ person }: { person: Person360 }) {
  return (
    <div className="space-y-4">
      <Card>
        <CardHeader>
          <div className="flex flex-wrap items-center gap-2">
            <CardTitle>{person.name || "Unnamed person"}</CardTitle>
            {person.quarantine ? (
              <Badge variant="warning">Quarantined — audience-ineligible</Badge>
            ) : (
              <Badge variant="success">Graph verified</Badge>
            )}
          </div>
          <CardDescription>
            Person node <span className="font-mono">{person.person_id}</span>
            {person.consent_summary ? (
              <> · Consent: {person.consent_summary}</>
            ) : null}
            {person.channels?.length ? (
              <> · Channels: {person.channels.join(", ")}</>
            ) : null}
            {person.phone_hash ? (
              <>
                {" "}
                · Phone: <span className="font-mono">hash {person.phone_hash.slice(0, 12)}…</span>
              </>
            ) : null}
          </CardDescription>
        </CardHeader>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Contacts</CardTitle>
          <CardDescription>Lead captures linked to this person.</CardDescription>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Lead</TableHead>
                <TableHead>First touch</TableHead>
                <TableHead>Source</TableHead>
                <TableHead>Captured</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {person.contacts.length === 0 ? (
                <TableEmpty colSpan={4}>No contacts recorded.</TableEmpty>
              ) : (
                person.contacts.map((c, i) => (
                  <TableRow key={c.lead_id ?? i}>
                    <TableCell className="font-mono text-xs">{dash(c.lead_id)}</TableCell>
                    <TableCell>{dash(c.channel_of_first_touch)}</TableCell>
                    <TableCell>{dash(c.source)}</TableCell>
                    <TableCell>{ts(c.captured_at)}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Bookings</CardTitle>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Booking</TableHead>
                <TableHead>Offering</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Showed</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {person.bookings.length === 0 ? (
                <TableEmpty colSpan={5}>No bookings recorded.</TableEmpty>
              ) : (
                person.bookings.map((b, i) => (
                  <TableRow key={b.booking_id ?? i}>
                    <TableCell className="font-mono text-xs">{dash(b.booking_id)}</TableCell>
                    <TableCell>{dash(b.offering ?? b.offering_id)}</TableCell>
                    <TableCell>
                      <Badge variant="secondary">{dash(b.status)}</Badge>
                    </TableCell>
                    <TableCell>{b.showed === undefined ? "—" : b.showed ? "yes" : "no"}</TableCell>
                    <TableCell>{ts(b.created_at)}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Consents</CardTitle>
          <CardDescription>
            Purpose-scoped consent edges — the basis of every audience.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Purpose</TableHead>
                <TableHead>Granted</TableHead>
                <TableHead>Revoked</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {person.consents.length === 0 ? (
                <TableEmpty colSpan={3}>No consents recorded.</TableEmpty>
              ) : (
                person.consents.map((c, i) => (
                  <TableRow key={c.consent_id ?? i}>
                    <TableCell>
                      <Badge variant={c.revoked_at ? "secondary" : "success"}>
                        {dash(c.purpose)}
                      </Badge>
                    </TableCell>
                    <TableCell>{ts(c.granted_at)}</TableCell>
                    <TableCell>{c.revoked_at ? ts(c.revoked_at) : "active"}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Referrals</CardTitle>
          <CardDescription>People this person referred, or was referred by.</CardDescription>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Person</TableHead>
                <TableHead>Direction</TableHead>
                <TableHead>Program</TableHead>
                <TableHead>At</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {person.referrals.length === 0 ? (
                <TableEmpty colSpan={4}>No referrals recorded.</TableEmpty>
              ) : (
                person.referrals.map((r, i) => (
                  <TableRow key={`${r.person_id ?? i}-${i}`}>
                    <TableCell>
                      {r.name ? `${r.name} ` : ""}
                      <span className="font-mono text-xs text-muted-foreground">
                        {dash(r.person_id)}
                      </span>
                    </TableCell>
                    <TableCell>
                      {r.direction === "in" ? (
                        <Badge variant="info">referred by</Badge>
                      ) : (
                        <Badge variant="secondary">referred</Badge>
                      )}
                    </TableCell>
                    <TableCell>{dash(r.program)}</TableCell>
                    <TableCell>{ts(r.at)}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Messages</CardTitle>
          <CardDescription>Outreach this person has received.</CardDescription>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Campaign</TableHead>
                <TableHead>Channel</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>At</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {person.messages.length === 0 ? (
                <TableEmpty colSpan={4}>No messages recorded.</TableEmpty>
              ) : (
                person.messages.map((m, i) => (
                  <TableRow key={`${m.campaign_id ?? i}-${i}`}>
                    <TableCell className="font-mono text-xs">{dash(m.campaign_id)}</TableCell>
                    <TableCell>{dash(m.channel)}</TableCell>
                    <TableCell>
                      <Badge variant="secondary">{dash(m.status)}</Badge>
                    </TableCell>
                    <TableCell>{ts(m.at)}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  );
}
