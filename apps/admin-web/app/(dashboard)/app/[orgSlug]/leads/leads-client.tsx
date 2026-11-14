"use client";

import * as React from "react";
import { Plus, RefreshCw } from "lucide-react";
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
import type { Lead } from "@/lib/types";

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
  const [showCreate, setShowCreate] = React.useState(false);
  const [creating, setCreating] = React.useState(false);
  const [phone, setPhone] = React.useState("");
  const [newChannel, setNewChannel] = React.useState<string>("web");
  const [busyId, setBusyId] = React.useState<string | null>(null);

  const load = React.useCallback(
    async (f: { status: string; channel: string }) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<{ leads?: Lead[] }>(
          "/api/bookings/v1/leads",
          {
            tenant: orgSlug,
            status: f.status || undefined,
            channel: f.channel || undefined,
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

  const skipInitialLoad = React.useRef(true);
  React.useEffect(() => {
    if (skipInitialLoad.current) {
      skipInitialLoad.current = false;
      return;
    }
    void load({ status, channel });
  }, [status, channel, load]);

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
      await load({ status, channel });
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

  const transition = async (lead: Lead, next: string) => {
    setBusyId(lead.lead_id);
    try {
      await api.post(
        `/api/bookings/v1/leads/${lead.lead_id}/status`,
        { status: next },
        { tenant: orgSlug },
      );
      toast({ title: `Lead marked ${next}`, variant: "success" });
      await load({ status, channel });
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
              onClick={() => void load({ status, channel })}
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
      </div>

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">Phone</TableHead>
              <TableHead>Channel</TableHead>
              <TableHead>Promo</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Captured</TableHead>
              <TableHead className="pr-5 text-right">Move to</TableHead>
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
    </div>
  );
}
