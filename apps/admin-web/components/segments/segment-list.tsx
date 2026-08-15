"use client";

/**
 * SPEC-W28 WS-C: saved segment list + "Create campaign" handoff.
 *
 * Create campaign opens a composer (channel + message, {name} placeholder)
 * and POSTs to the notification-worker audience intake
 * (/api/notifications/v1/audiences). The intake materializes the
 * consent-passing audience from graph-service and enqueues the sends through
 * the EXISTING pacer path — DND 2442 suppression, quiet-hours deferral, CPS
 * pacing and sender rotation all apply unchanged.
 *
 * Idempotency (SPEC-W24): the campaign id is generated up front and sent as
 * both the campaign_id and the Idempotency-Key header, so a retried submit
 * can never double-enqueue the campaign.
 */
import * as React from "react";
import { Megaphone, RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { formatDateTime } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label, Select, Textarea } from "@/components/ui/input";
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
import { AUDIENCES_API, type Segment } from "./types";

const MESSAGE_MAX = 1000;

const CHANNELS = [
  { value: "sms", label: "SMS" },
  { value: "whatsapp", label: "WhatsApp" },
  { value: "telegram", label: "Telegram" },
];

/** W24-style idempotency key; crypto.randomUUID with a fallback for old browsers. */
function newCampaignId(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return `camp-${crypto.randomUUID()}`;
  }
  return `camp-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
}

interface AudienceResult {
  campaign_id?: string;
  duplicate?: boolean;
  audience_size?: number;
  enqueued?: number;
  already_running?: number;
  skipped_no_phone?: number;
  skipped_quarantined?: number;
}

export function SegmentList({
  orgSlug,
  segments,
  loading,
  error,
  onRefresh,
}: {
  orgSlug: string;
  segments: Segment[];
  loading: boolean;
  error: string | null;
  onRefresh: () => void;
}) {
  const { toast } = useToast();
  const [target, setTarget] = React.useState<Segment | null>(null);
  const [campaignId, setCampaignId] = React.useState("");
  const [channel, setChannel] = React.useState("sms");
  const [message, setMessage] = React.useState("");
  const [launching, setLaunching] = React.useState(false);

  function openComposer(segment: Segment) {
    setTarget(segment);
    setCampaignId(newCampaignId()); // fixed per composer session → safe retries
    setChannel("sms");
    setMessage("");
  }

  async function launch() {
    if (!target) return;
    setLaunching(true);
    try {
      const res = await api.post<AudienceResult>(
        AUDIENCES_API,
        {
          segment_id: target.id,
          campaign_id: campaignId,
          message: message.trim(),
          channel,
        },
        { tenant: orgSlug },
        { "Idempotency-Key": campaignId },
      );
      const enqueued = res.enqueued ?? 0;
      toast({
        title: res.duplicate ? "Campaign already enqueued" : "Campaign created",
        description: res.duplicate
          ? "This campaign was already sent to the queue (idempotent retry)."
          : `${enqueued.toLocaleString()} of ${(res.audience_size ?? 0).toLocaleString()} consent-passing people enqueued. DND and quiet-hours rules apply on send.`,
        variant: "success",
      });
      setTarget(null);
    } catch (e) {
      toast({
        title: "Campaign could not be created",
        description:
          e instanceof ApiError
            ? e.status === 502
              ? "The graph service is unavailable — nothing was enqueued; retrying is safe."
              : e.message
            : "The notification service may be offline.",
        variant: "destructive",
      });
    } finally {
      setLaunching(false);
    }
  }

  return (
    <Card>
      <CardHeader>
        <div className="flex items-start justify-between gap-2">
          <div>
            <CardTitle>Saved segments</CardTitle>
            <CardDescription>
              Audiences saved to your customer graph. Creating a campaign hands
              the consent-passing audience to the notification worker.
            </CardDescription>
          </div>
          <Button
            variant="outline"
            size="sm"
            onClick={onRefresh}
            disabled={loading}
          >
            <RefreshCw className="h-3.5 w-3.5" />
            {loading ? "Loading…" : "Refresh"}
          </Button>
        </div>
      </CardHeader>
      <CardContent>
        {error ? (
          <p className="py-6 text-center text-sm text-muted-foreground">{error}</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Consent purpose</TableHead>
                <TableHead>Filters</TableHead>
                <TableHead>Created</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {segments.length === 0 && !loading ? (
                <TableEmpty colSpan={5}>
                  No segments yet — build one above and save it.
                </TableEmpty>
              ) : (
                segments.map((s) => (
                  <TableRow key={s.id}>
                    <TableCell className="font-medium">{s.name}</TableCell>
                    <TableCell>
                      <Badge variant="secondary">{s.has_consent}</Badge>
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {describeFilters(s)}
                    </TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {s.created_at ? formatDateTime(s.created_at) : "—"}
                    </TableCell>
                    <TableCell className="text-right">
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() => openComposer(s)}
                      >
                        <Megaphone className="h-3.5 w-3.5" />
                        Create campaign
                      </Button>
                    </TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        )}
      </CardContent>

      <Dialog open={target !== null} onOpenChange={(open) => !open && setTarget(null)}>
        <DialogContent onClose={() => setTarget(null)}>
          <DialogHeader>
            <DialogTitle>Create campaign — {target?.name}</DialogTitle>
            <DialogDescription>
              The consent-passing audience is materialized at send time. DND
              (2442) suppression, quiet hours and send pacing apply unchanged;
              quarantined people are always excluded.
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="camp-channel">Channel</Label>
              <Select
                id="camp-channel"
                value={channel}
                onChange={(e) => setChannel(e.target.value)}
              >
                {CHANNELS.map((c) => (
                  <option key={c.value} value={c.value}>
                    {c.label}
                  </option>
                ))}
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="camp-message">Message</Label>
              <Textarea
                id="camp-message"
                value={message}
                onChange={(e) => setMessage(e.target.value.slice(0, MESSAGE_MAX))}
                placeholder="Hi {name}, we'd love to see you again — book this week and get 10% off."
                rows={4}
              />
              <p className="text-xs text-muted-foreground">
                Use {"{name}"} to personalize. {message.length}/{MESSAGE_MAX}
              </p>
            </div>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setTarget(null)} disabled={launching}>
              Cancel
            </Button>
            <Button
              onClick={() => void launch()}
              disabled={launching || message.trim() === ""}
            >
              <Megaphone className="h-4 w-4" />
              {launching ? "Enqueuing…" : "Enqueue campaign"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  );
}

function describeFilters(s: Segment): string {
  const parts: string[] = [];
  if (s.last_booking_before) parts.push(`last booking before ${s.last_booking_before}`);
  if (s.lga) parts.push(`LGA ${s.lga}`);
  if (typeof s.not_messaged_since_days === "number")
    parts.push(`not messaged in ${s.not_messaged_since_days}d`);
  return parts.length ? parts.join(" · ") : "No extra filters";
}
