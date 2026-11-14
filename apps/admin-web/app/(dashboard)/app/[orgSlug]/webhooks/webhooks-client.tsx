"use client";

import * as React from "react";
import { Copy, Plus, Trash2, Webhook } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input, Label } from "@/components/ui/input";
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
import { formatDateTime } from "@/lib/utils";
import type {
  WebhookDelivery,
  WebhookSubscription,
  WebhookSubscriptionCreated,
} from "@/lib/types";

/**
 * Outbound webhook platform (Wave 5 #10): per-tenant subscriptions on
 * booking/conversation events with HMAC-signed delivery + retry history.
 * Backed by notification-worker at /api/notifications/v1/webhooks.
 */

const EVENT_CHOICES = [
  { value: "*", label: "All events (*)" },
  { value: "com.opendesk.booking.BookingCreated", label: "Booking created" },
  { value: "com.opendesk.booking.BookingConfirmed", label: "Booking confirmed" },
  { value: "com.opendesk.booking.BookingRescheduled", label: "Booking rescheduled" },
  { value: "com.opendesk.booking.BookingCancelled", label: "Booking cancelled" },
  { value: "com.opendesk.booking.BookingNoShow", label: "Booking no-show" },
  { value: "com.opendesk.conversation.SessionStarted", label: "Conversation started" },
  { value: "com.opendesk.conversation.SessionEnded", label: "Conversation ended" },
];

const statusVariant: Record<string, "default" | "success" | "warning" | "destructive" | "secondary"> = {
  pending: "secondary",
  retrying: "warning",
  delivered: "success",
  dlq: "destructive",
};

export function WebhooksClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [subs, setSubs] = React.useState<WebhookSubscription[]>([]);
  const [error, setError] = React.useState<string | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [url, setUrl] = React.useState("");
  const [selected, setSelected] = React.useState<string[]>([]);
  const [secret, setSecret] = React.useState("");
  const [creating, setCreating] = React.useState(false);
  /** Newly created secret — shown exactly once. */
  const [shownSecret, setShownSecret] = React.useState<{ id: string; secret: string } | null>(null);
  const [deliveriesFor, setDeliveriesFor] = React.useState<string | null>(null);
  const [deliveries, setDeliveries] = React.useState<WebhookDelivery[]>([]);

  const load = React.useCallback(async () => {
    try {
      const data = await api.get<{ webhooks: WebhookSubscription[] }>(
        "/api/notifications/v1/webhooks",
        { tenant: orgSlug },
      );
      setSubs(data.webhooks ?? []);
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to load webhooks.");
    } finally {
      setLoading(false);
    }
  }, [orgSlug]);

  React.useEffect(() => {
    load();
  }, [load]);

  const toggleEvent = (value: string) => {
    setSelected((prev) =>
      value === "*"
        ? prev.includes("*")
          ? []
          : ["*"] // "*" is exclusive
        : prev.includes(value)
          ? prev.filter((v) => v !== value)
          : [...prev.filter((v) => v !== "*"), value],
    );
  };

  const create = async () => {
    setCreating(true);
    setError(null);
    try {
      const res = await api.post<WebhookSubscriptionCreated>(
        "/api/notifications/v1/webhooks",
        {
          url: url.trim(),
          events: selected,
          ...(secret.trim() ? { secret: secret.trim() } : {}),
        },
        { tenant: orgSlug },
      );
      setShownSecret({ id: res.id, secret: res.secret });
      setUrl("");
      setSelected([]);
      setSecret("");
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to create webhook.");
    } finally {
      setCreating(false);
    }
  };

  const remove = async (id: string) => {
    try {
      await api.delete(`/api/notifications/v1/webhooks/${id}`, { tenant: orgSlug });
      if (deliveriesFor === id) {
        setDeliveriesFor(null);
        setDeliveries([]);
      }
      await load();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to delete webhook.");
    }
  };

  const toggleDeliveries = async (id: string) => {
    if (deliveriesFor === id) {
      setDeliveriesFor(null);
      setDeliveries([]);
      return;
    }
    setDeliveriesFor(id);
    try {
      const data = await api.get<{ deliveries: WebhookDelivery[] }>(
        `/api/notifications/v1/webhooks/${id}/deliveries`,
        { tenant: orgSlug },
      );
      setDeliveries(data.deliveries ?? []);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to load deliveries.");
    }
  };

  const copySecret = (value: string) => {
    navigator.clipboard
      .writeText(value)
      .then(() => toast({ title: "Secret copied" }))
      .catch(() => {});
  };

  return (
    <div className="space-y-6 p-6">
      <PageHeader
        title="Webhooks"
        description="Push booking & conversation events to your own endpoints — HMAC-signed, retried with backoff."
      />
      {error ? <ErrorNote message={error} /> : null}

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Plus className="h-4 w-4" /> New subscription
          </CardTitle>
          <CardDescription>
            Events are POSTed as CloudEvents JSON with X-OpenDesk-Signature
            (HMAC-SHA256). Failed deliveries retry after 1m, 5m, 15m, 1h and 4h,
            then land in the DLQ.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <div>
            <Label htmlFor="hook-url">Endpoint URL</Label>
            <Input
              id="hook-url"
              type="url"
              placeholder="https://your-app.example/opendesk-webhook"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
            />
          </div>
          <div>
            <Label>Events</Label>
            <div className="mt-2 grid grid-cols-1 gap-1 sm:grid-cols-2">
              {EVENT_CHOICES.map((c) => (
                <label key={c.value} className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    checked={selected.includes(c.value)}
                    onChange={() => toggleEvent(c.value)}
                  />
                  {c.label}
                </label>
              ))}
            </div>
          </div>
          <div>
            <Label htmlFor="hook-secret">Signing secret (optional)</Label>
            <Input
              id="hook-secret"
              value={secret}
              onChange={(e) => setSecret(e.target.value)}
              placeholder="Leave empty to auto-generate"
            />
          </div>
          <Button onClick={create} disabled={creating || !url.trim() || selected.length === 0}>
            {creating ? "Creating…" : "Create subscription"}
          </Button>

          {shownSecret ? (
            <div className="rounded-md border border-warning/40 bg-warning-soft p-3 text-sm">
              <p className="font-medium text-warning">
                Save this signing secret — it is shown only once:
              </p>
              <div className="mt-2 flex items-center gap-2">
                <code className="rounded bg-background px-2 py-1 text-xs">{shownSecret.secret}</code>
                <Button size="sm" variant="outline" onClick={() => copySecret(shownSecret.secret)}>
                  <Copy className="h-3 w-3" />
                </Button>
                <Button size="sm" variant="ghost" onClick={() => setShownSecret(null)}>
                  Dismiss
                </Button>
              </div>
            </div>
          ) : null}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Webhook className="h-4 w-4" /> Subscriptions
          </CardTitle>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>URL</TableHead>
                <TableHead>Events</TableHead>
                <TableHead>Signed</TableHead>
                <TableHead>Created</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {subs.length === 0 ? (
                <TableEmpty colSpan={5}>
                  {loading ? "Loading…" : "No webhook subscriptions yet."}
                </TableEmpty>
              ) : (
                subs.map((s) => (
                  <React.Fragment key={s.id}>
                    <TableRow>
                      <TableCell className="max-w-[260px] truncate font-mono text-xs">
                        {s.url}
                      </TableCell>
                      <TableCell className="max-w-[220px]">
                        <div className="flex flex-wrap gap-1">
                          {s.events.map((e) => (
                            <Badge key={e} variant="outline">
                              {e === "*" ? "all" : (e.split(".").pop() ?? e)}
                            </Badge>
                          ))}
                        </div>
                      </TableCell>
                      <TableCell>
                        <Badge variant={s.secret_set ? "success" : "secondary"}>
                          {s.secret_set ? "HMAC" : "none"}
                        </Badge>
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">
                        {formatDateTime(s.created_at)}
                      </TableCell>
                      <TableCell className="text-right">
                        <div className="flex justify-end gap-2">
                          <Button size="sm" variant="outline" onClick={() => toggleDeliveries(s.id)}>
                            {deliveriesFor === s.id ? "Hide" : "Deliveries"}
                          </Button>
                          <Button size="sm" variant="destructive" onClick={() => remove(s.id)}>
                            <Trash2 className="h-3 w-3" />
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                    {deliveriesFor === s.id ? (
                      <TableRow>
                        <TableCell colSpan={5} className="bg-muted/40">
                          <DeliveryTable deliveries={deliveries} />
                        </TableCell>
                      </TableRow>
                    ) : null}
                  </React.Fragment>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  );
}

function DeliveryTable({ deliveries }: { deliveries: WebhookDelivery[] }) {
  if (deliveries.length === 0) {
    return <p className="py-2 text-sm text-muted-foreground">No deliveries yet.</p>;
  }
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Event</TableHead>
          <TableHead>Status</TableHead>
          <TableHead>Attempts</TableHead>
          <TableHead>Last HTTP</TableHead>
          <TableHead>Next retry</TableHead>
          <TableHead>Created</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {deliveries.map((d) => (
          <TableRow key={d.id}>
            <TableCell className="text-xs">{d.event_type.split(".").pop()}</TableCell>
            <TableCell>
              <Badge variant={statusVariant[d.status] ?? "secondary"}>{d.status}</Badge>
            </TableCell>
            <TableCell>{d.attempts}</TableCell>
            <TableCell>{d.last_status_code ?? "—"}</TableCell>
            <TableCell className="text-xs text-muted-foreground">
              {d.next_retry_at ? formatDateTime(d.next_retry_at) : "—"}
            </TableCell>
            <TableCell className="text-xs text-muted-foreground">
              {formatDateTime(d.created_at)}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
