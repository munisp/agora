"use client";

import * as React from "react";
import { Download, Plus, RefreshCw, Search } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input, Label, Select } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, titleCase } from "@/lib/utils";
import { downloadCSV, toCSV, type CsvColumn } from "@/lib/export";
import type { Lead } from "@/lib/types";
import { EntitySearchPanel } from "@/components/entity-search-panel";
import { LEADS_DESCRIPTOR } from "@/lib/entity-search";

/** SPEC-W82 B1.5: CSV columns for the filtered leads export. */
const LEAD_CSV_COLUMNS: CsvColumn<Lead>[] = [
  { header: "Phone", value: (l) => l.phone_e164 },
  { header: "Channel", value: (l) => l.channel_of_first_touch },
  { header: "Promo", value: (l) => l.promo_code ?? "" },
  { header: "Status", value: (l) => l.status },
  { header: "Captured", value: (l) => l.created_at },
];

/** Statuses the backend transition endpoint accepts (booking-service leads.go). */
const NEXT_STATUSES = ["contacted", "qualified", "converted", "lost"] as const;
const STATUS_FILTERS = ["", "new", ...NEXT_STATUSES] as const;

function statusVariant(
  status: string,
): "success" | "warning" | "destructive" | "secondary" {
  switch (status) {
    case "converted":
      return "success";
    case "qualified":
    case "contacted":
      return "warning";
    case "lost":
      return "destructive";
    default:
      return "secondary";
  }
}

/** SPEC-W83: phone-prefix search shape — mirrors the backend 400 rule
 *  (booking-service httpapi/leads.go leadQueryRe). */
const LEAD_Q_RE = /^(\+[0-9]{2,31}|[0-9]{3,32})$/;

const CHANNELS = [
  "web",
  "voice",
  "whatsapp",
  "telegram",
  "sms",
  "ussd",
  "qr",
  "promo",
  "field",
  "webhook",
] as const;

export function LeadsClient({
  orgSlug,
  initialLeads,
  initialError,
}: {
  orgSlug: string;
  initialLeads: Lead[];
  initialError: string | null;
}) {
  const { toast } = useToast();
  const [leads, setLeads] = React.useState<Lead[]>(initialLeads);
  const [error, setError] = React.useState<string | null>(initialError);
  const [loading, setLoading] = React.useState(false);
  const [status, setStatus] = React.useState<string>("");
  const [channel, setChannel] = React.useState<string>("");
  /** SPEC-W83: phone-prefix search box (raw input; validated on fire). */
  const [phoneQ, setPhoneQ] = React.useState<string>("");
  const [showCreate, setShowCreate] = React.useState(false);
  const [creating, setCreating] = React.useState(false);
  const [phone, setPhone] = React.useState("");
  const [newChannel, setNewChannel] = React.useState<string>("web");
  const [busyId, setBusyId] = React.useState<string | null>(null);

  const load = React.useCallback(
    async (f: { status: string; channel: string; q: string }) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<{ leads?: Lead[] }>(
          "/api/bookings/v1/leads",
          {
            tenant: orgSlug,
            status: f.status || undefined,
            channel: f.channel || undefined,
            q: f.q || undefined,
          },
        );
        setLeads(data.leads ?? []);
      } catch (e) {
        setError(e instanceof ApiError ? e.message : "Failed to load leads.");
      } finally {
        setLoading(false);
      }
    },
    [orgSlug],
  );

  // SPEC-W83: all three filters share one 250 ms debounce (the platform
  // idiom); an invalid q holds the last good list instead of firing a
  // request the backend would 400.
  const qTrimmed = phoneQ.trim();
  const qValid = qTrimmed === "" || LEAD_Q_RE.test(qTrimmed);
  const skipInitialLoad = React.useRef(true);
  React.useEffect(() => {
    if (skipInitialLoad.current) {
      skipInitialLoad.current = false;
      return;
    }
    if (!qValid) return;
    const timer = setTimeout(() => {
      void load({ status, channel, q: qTrimmed });
    }, 250);
    return () => clearTimeout(timer);
  }, [status, channel, qTrimmed, qValid, load]);

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    const phoneE164 = phone.trim();
    if (!phoneE164) return;
    setCreating(true);
    try {
      await api.post(
        "/api/bookings/v1/leads",
        { phone_e164: phoneE164, channel: newChannel },
        { tenant: orgSlug },
      );
      toast({ title: "Lead captured", variant: "success" });
      setPhone("");
      setShowCreate(false);
      await load({ status, channel, q: "" });
    } catch (err) {
      toast({
        title: "Could not capture the lead",
        description: err instanceof ApiError ? err.message : undefined,
        variant: "destructive",
      });
    } finally {
      setCreating(false);
    }
  };

  // SPEC-W82 B1.5: export the currently filtered lead set (already-fetched
  // client state — no endpoint involved).
  const exportCsv = () => {
    downloadCSV(
      `leads-${new Date().toISOString().slice(0, 10)}.csv`,
      toCSV(leads, LEAD_CSV_COLUMNS),
    );
  };

  const transition = async (lead: Lead, next: string) => {
    setBusyId(lead.lead_id);
    try {
      await api.post(
        `/api/bookings/v1/leads/${lead.lead_id}/status`,
        { status: next },
        { tenant: orgSlug },
      );
      toast({ title: `Lead marked ${next}`, variant: "success" });
      await load({ status, channel, q: "" });
    } catch (e) {
      toast({
        title: "Status change failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusyId(null);
    }
  };

  return (
    <div>
      <PageHeader
        title="Leads"
        description="Every inbound lead across channels (voice, WhatsApp, field capture, QR…), deduplicated by phone. Move leads through the pipeline as you work them."
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void load({ status, channel, q: qTrimmed })}
              disabled={loading}
            >
              <RefreshCw className={loading ? "h-3.5 w-3.5 animate-spin" : "h-3.5 w-3.5"} />
              Refresh
            </Button>
            <Button size="sm" onClick={() => setShowCreate((v) => !v)}>
              <Plus className="h-4 w-4" />
              Capture lead
            </Button>
          </>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      {/* SPEC-W119: OpenSearch advanced search (indexed; newest captured
          first). The debounced prefix search below is unaffected. */}
      <div className="mb-4">
        <EntitySearchPanel
          orgSlug={orgSlug}
          descriptor={LEADS_DESCRIPTOR}
          title="Advanced search"
          description="Searches the OpenSearch leads index by phone number or promo code, with status, channel and capture-date filters."
          columns={["Phone", "Channel", "Status", "Promo", "Captured"]}
          hitKey={(h) => String(h.lead_id ?? JSON.stringify(h))}
          renderRow={(h) => (
            <>
              <TableCell className="pl-5 font-medium">
                {String(h.phone_e164 ?? "—")}
              </TableCell>
              <TableCell>
                <Badge variant="outline">
                  {titleCase(String(h.channel_of_first_touch ?? ""))}
                </Badge>
              </TableCell>
              <TableCell>
                <Badge variant={statusVariant(String(h.status ?? ""))}>
                  {String(h.status ?? "—")}
                </Badge>
              </TableCell>
              <TableCell className="text-xs text-muted-foreground">
                {String(h.promo_code ?? "—")}
              </TableCell>
              <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                {h.created_at ? formatDateTime(String(h.created_at)) : "—"}
              </TableCell>
            </>
          )}
        />
      </div>

      {showCreate ? (
        <Card className="mb-4">
          <CardHeader>
            <CardTitle>Capture lead</CardTitle>
          </CardHeader>
          <CardContent>
            <form onSubmit={create} className="flex flex-wrap items-end gap-3">
              <div className="grid min-w-56 gap-1.5">
                <Label htmlFor="lead-phone">Phone (E.164)</Label>
                <Input
                  id="lead-phone"
                  type="tel"
                  placeholder="+2348012345678"
                  value={phone}
                  onChange={(e) => setPhone(e.target.value)}
                />
              </div>
              <div className="grid w-44 gap-1.5">
                <Label htmlFor="lead-channel">Channel</Label>
                <Select
                  id="lead-channel"
                  value={newChannel}
                  onChange={(e) => setNewChannel(e.target.value)}
                >
                  {CHANNELS.map((c) => (
                    <option key={c} value={c}>
                      {titleCase(c)}
                    </option>
                  ))}
                </Select>
              </div>
              <Button type="submit" disabled={creating || !phone.trim()}>
                {creating ? "Saving…" : "Capture"}
              </Button>
            </form>
          </CardContent>
        </Card>
      ) : null}

      <div className="mb-3 flex flex-wrap items-center gap-3">
        {/* SPEC-W83: phone-prefix search, debounced, combinable with the
            status/channel filters. */}
        <div className="flex items-center gap-2">
          <Label htmlFor="lead-q" className="text-xs">
            Search
          </Label>
          <div className="relative">
            <Search className="pointer-events-none absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
            <Input
              id="lead-q"
              type="tel"
              value={phoneQ}
              onChange={(e) => setPhoneQ(e.target.value)}
              placeholder="Phone prefix e.g. +234803…"
              className="w-56 pl-8"
              maxLength={32}
              aria-invalid={!qValid}
            />
          </div>
          {!qValid ? (
            <p className="text-xs text-warning">
              Digits and + only, 3–32 chars
            </p>
          ) : null}
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor="lead-status" className="text-xs">
            Status
          </Label>
          <Select
            id="lead-status"
            value={status}
            onChange={(e) => setStatus(e.target.value)}
            className="w-40"
          >
            {STATUS_FILTERS.map((s) => (
              <option key={s} value={s}>
                {s === "" ? "All" : titleCase(s)}
              </option>
            ))}
          </Select>
        </div>
        <div className="flex items-center gap-2">
          <Label htmlFor="lead-channel-filter" className="text-xs">
            Channel
          </Label>
          <Select
            id="lead-channel-filter"
            value={channel}
            onChange={(e) => setChannel(e.target.value)}
            className="w-40"
          >
            <option value="">All</option>
            {CHANNELS.map((c) => (
              <option key={c} value={c}>
                {titleCase(c)}
              </option>
            ))}
          </Select>
        </div>
        {/* SPEC-W82 B1.5: export exactly the filtered set on screen. */}
        <Button
          variant="outline"
          size="sm"
          onClick={exportCsv}
          disabled={leads.length === 0}
        >
          <Download className="h-3.5 w-3.5" />
          Export CSV
        </Button>
      </div>

      {/* SPEC-W82 B1.2: table on ≥sm, stacked cards below. */}
      <Card className="hidden sm:block">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">Phone</TableHead>
              <TableHead>Channel</TableHead>
              <TableHead>Promo</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Captured</TableHead>
              <TableHead className="pr-5 text-right" sortable={false}>Move to</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {leads.length === 0 ? (
              <TableEmpty colSpan={6}>
                {loading
                  ? "Loading…"
                  : "No leads match — inbound leads from every channel land here."}
              </TableEmpty>
            ) : (
              leads.map((lead) => (
                <TableRow key={lead.lead_id}>
                  <TableCell className="pl-5 font-medium">
                    {lead.phone_e164}
                  </TableCell>
                  <TableCell>
                    <Badge variant="outline">
                      {titleCase(lead.channel_of_first_touch)}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {lead.promo_code ?? "—"}
                  </TableCell>
                  <TableCell>
                    <Badge variant={statusVariant(lead.status)}>
                      {lead.status}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {formatDateTime(lead.created_at)}
                  </TableCell>
                  <TableCell className="pr-5 text-right">
                    <div className="flex justify-end gap-1">
                      {NEXT_STATUSES.filter((s) => s !== lead.status).map(
                        (s) => (
                          <Button
                            key={s}
                            variant="ghost"
                            size="sm"
                            disabled={busyId === lead.lead_id}
                            onClick={() => void transition(lead, s)}
                          >
                            {titleCase(s)}
                          </Button>
                        ),
                      )}
                    </div>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </Card>

      {/* SPEC-W82 B1.2: stacked lead cards below sm:. */}
      <div className="space-y-2 sm:hidden">
        {leads.length === 0 ? (
          <p className="rounded-md border border-border bg-card px-4 py-6 text-sm text-muted-foreground">
            {loading
              ? "Loading…"
              : "No leads match — inbound leads from every channel land here."}
          </p>
        ) : (
          leads.map((lead) => (
            <div
              key={lead.lead_id}
              className="rounded-md border border-border bg-card px-4 py-3"
            >
              <div className="flex flex-wrap items-center gap-2">
                <p className="font-medium">{lead.phone_e164}</p>
                <Badge variant={statusVariant(lead.status)}>{lead.status}</Badge>
              </div>
              <p className="mt-1 text-xs text-muted-foreground">
                <Badge variant="outline">
                  {titleCase(lead.channel_of_first_touch)}
                </Badge>{" "}
                {lead.promo_code ? `· ${lead.promo_code} ` : ""}·{" "}
                {formatDateTime(lead.created_at)}
              </p>
              <div className="mt-2 flex flex-wrap gap-1">
                {NEXT_STATUSES.filter((s) => s !== lead.status).map((s) => (
                  <Button
                    key={s}
                    variant="ghost"
                    size="sm"
                    disabled={busyId === lead.lead_id}
                    onClick={() => void transition(lead, s)}
                  >
                    {titleCase(s)}
                  </Button>
                ))}
              </div>
            </div>
          ))
        )}
      </div>
    </div>
  );
}
