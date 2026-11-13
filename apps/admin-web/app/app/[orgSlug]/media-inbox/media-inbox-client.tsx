"use client";

/**
 * SPEC-W61 Scope D: media inbox review queue (owner/admin only — the page
 * redirects other roles server-side; the booking-service media inbox routes
 * enforce the same gate independently).
 *
 * Unlike the W58 KYC surface (a session-local lookup list — no list
 * endpoint existed there), this is a REAL server-side queue: rows live in
 * the booking-service `media_inbox` table and are listed, filtered and
 * mutated through the EXISTING /api/bookings/* APISIX OIDC wildcard (no new
 * gateway route):
 *
 *   GET  /api/bookings/v1/media/inbox?state=<state>&limit=<n>
 *     → MediaInboxItem[] (or {items: [...]}) — newest first, limit ≤ 500
 *   POST /api/bookings/v1/media/inbox/{id}/link    {booking_id: "<uuid>"}
 *   POST /api/bookings/v1/media/inbox/{id}/discard
 *
 * Notes (pinned contract):
 *  - The machine ingest route (POST /v1/media/inbox/ingest) is
 *    internauth-only and edge-DENIED in APISIX — it never appears here.
 *  - Media BYTES are never served by any API — reviewers work from the
 *    extracted metadata (channel, sender phone, mime, byte count) plus the
 *    booking they link to. After a subject erasure the sender phone is
 *    tombstoned to a salted `sha256:…` form and is rendered as-is.
 *  - List pagination mirrors bookings-client (SPEC-W46 AW-4): bounded list,
 *    "load more" refetches with a larger limit up to 500.
 */
import * as React from "react";
import { Inbox, Link2, RefreshCw, Trash2 } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
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
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
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

/** Row shape of booking-service GET /v1/media/inbox (SPEC-W61). */
export interface MediaInboxItem {
  id: string;
  channel: string;
  provider_message_id?: string | null;
  document_id?: string | null;
  /** Raw sender phone, or the salted `sha256:…` tombstone after erasure. */
  sender_phone: string;
  mime: string;
  bytes: number;
  state: string;
  anchor_method?: string | null;
  booking_id?: string | null;
  contact_id?: string | null;
  linked_by?: string | null;
  created_at?: string | null;
  updated_at?: string | null;
}

type StateFilter = "parked" | "anchored" | "linked" | "discarded";

const STATE_FILTERS: { value: StateFilter; label: string }[] = [
  { value: "parked", label: "Parked" },
  { value: "anchored", label: "Anchored" },
  { value: "linked", label: "Linked" },
  { value: "discarded", label: "Discarded" },
];

/** Bounded list pagination (bookings-client AW-4 precedent). */
const PAGE_SIZE = 50;
const MAX_LIMIT = 500;

const UUID_RE =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

function stateVariant(
  state: string | null | undefined,
): "success" | "destructive" | "warning" | "secondary" {
  const v = (state ?? "").toLowerCase();
  if (v === "linked" || v === "anchored") return "success";
  if (v === "parked") return "warning";
  if (v === "discarded") return "secondary";
  return "secondary";
}

function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "—";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

/** Tolerant unwrap — the endpoint returns a bare array or {items: [...]}. */
function unwrapItems(data: unknown): MediaInboxItem[] {
  if (Array.isArray(data)) return data as MediaInboxItem[];
  if (
    typeof data === "object" &&
    data !== null &&
    Array.isArray((data as { items?: unknown }).items)
  ) {
    return (data as { items: MediaInboxItem[] }).items;
  }
  return [];
}

export function MediaInboxClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();
  const [filter, setFilter] = React.useState<StateFilter>("parked");
  const [items, setItems] = React.useState<MediaInboxItem[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [limit, setLimit] = React.useState(PAGE_SIZE);
  const [linking, setLinking] = React.useState<MediaInboxItem | null>(null);
  const [discarding, setDiscarding] = React.useState<MediaInboxItem | null>(
    null,
  );
  const [busy, setBusy] = React.useState(false);

  const load = React.useCallback(
    async (state: StateFilter, lim: number, initial = false) => {
      if (initial) setLoading(true);
      setError(null);
      try {
        const data = await api.get<MediaInboxItem[] | { items: MediaInboxItem[] }>(
          "/api/bookings/v1/media/inbox",
          { tenant: orgSlug, state, limit: lim },
        );
        setItems(unwrapItems(data));
      } catch (err) {
        setError(
          err instanceof ApiError && err.status === 403
            ? "The media inbox requires the owner or admin role."
            : err instanceof ApiError
              ? err.message
              : "Failed to load the media inbox.",
        );
        if (initial) setItems([]);
      } finally {
        setLoading(false);
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    setLimit(PAGE_SIZE);
    void load(filter, PAGE_SIZE, true);
  }, [filter, load]);

  const refresh = () => void load(filter, limit);

  const loadMore = () => {
    const next = Math.min(limit + PAGE_SIZE, MAX_LIMIT);
    setLimit(next);
    void load(filter, next);
  };

  const canLoadMore = items.length >= limit && limit < MAX_LIMIT;

  const link = async (bookingId: string) => {
    if (!linking || busy) return;
    setBusy(true);
    try {
      await api.post(
        `/api/bookings/v1/media/inbox/${encodeURIComponent(linking.id)}/link`,
        { booking_id: bookingId.trim() },
        { tenant: orgSlug },
      );
      toast({
        title: "Media linked to booking",
        description:
          "The inbox row is now linked and the booking's contact is recorded on it.",
        variant: "success",
      });
      setLinking(null);
      refresh();
    } catch (err) {
      toast({
        title: "Link failed",
        description:
          err instanceof ApiError
            ? err.status === 404
              ? "No booking with that id exists in this organisation."
              : err.message
            : "Failed to link the media to a booking.",
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const discard = async () => {
    if (!discarding || busy) return;
    setBusy(true);
    try {
      await api.post(
        `/api/bookings/v1/media/inbox/${encodeURIComponent(discarding.id)}/discard`,
        {},
        { tenant: orgSlug },
      );
      toast({
        title: "Media discarded",
        description:
          "The row is marked discarded; the OCR bytes still expire on the media TTL.",
        variant: "success",
      });
      setDiscarding(null);
      refresh();
    } catch (err) {
      toast({
        title: "Discard failed",
        description:
          err instanceof ApiError
            ? err.message
            : "Failed to discard the media inbox row.",
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <PageHeader
        title="Media inbox"
        description="Inbound WhatsApp media awaiting review. Parked items could not be matched to a booking automatically; link them to a booking or discard them. Owner/admin only."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={refresh}
            disabled={loading}
          >
            <RefreshCw
              className={loading ? "h-4 w-4 animate-spin" : "h-4 w-4"}
            />
            {loading ? "Loading…" : "Refresh"}
          </Button>
        }
      />

      {/* State filter — parked is the default review queue. */}
      <div className="mb-4 flex flex-wrap items-center gap-2">
        {STATE_FILTERS.map((f) => (
          <Button
            key={f.value}
            variant={filter === f.value ? "default" : "outline"}
            size="sm"
            onClick={() => setFilter(f.value)}
            disabled={loading}
          >
            {f.label}
          </Button>
        ))}
      </div>

      {error ? <ErrorNote message={error} /> : null}

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Inbox className="h-5 w-5" />
            Inbound media queue
          </CardTitle>
          <CardDescription>
            Server-side queue from booking-service — newest first. Parked rows
            beyond the per-tenant cap of 500 are auto-discarded; OCR bytes
            expire on the media TTL regardless.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Received</TableHead>
                <TableHead>Channel</TableHead>
                <TableHead>Sender</TableHead>
                <TableHead>Type</TableHead>
                <TableHead>Size</TableHead>
                <TableHead>State</TableHead>
                <TableHead className="pr-5 text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {items.length === 0 && !loading ? (
                <TableEmpty colSpan={7}>
                  {error
                    ? "Could not load the media inbox."
                    : `No ${filter} media inbox items.`}
                </TableEmpty>
              ) : (
                items.map((m) => {
                  const actionable =
                    m.state !== "discarded" && m.state !== "linked";
                  return (
                    <TableRow key={m.id}>
                      <TableCell className="pl-5 text-sm text-muted-foreground">
                        {m.created_at ? formatDateTime(m.created_at) : "—"}
                      </TableCell>
                      <TableCell className="text-sm">{m.channel}</TableCell>
                      <TableCell className="font-mono text-xs">
                        {m.sender_phone}
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {m.mime}
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {formatBytes(m.bytes)}
                      </TableCell>
                      <TableCell>
                        <Badge variant={stateVariant(m.state)}>
                          {m.state}
                          {m.anchor_method
                            ? ` · ${m.anchor_method.replace(/_/g, " ")}`
                            : ""}
                        </Badge>
                      </TableCell>
                      <TableCell className="pr-5 text-right">
                        {actionable ? (
                          <div className="flex justify-end gap-1">
                            <Button
                              variant="ghost"
                              size="icon"
                              title="Link to a booking"
                              onClick={() => setLinking(m)}
                            >
                              <Link2 className="h-4 w-4" />
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon"
                              title="Discard"
                              onClick={() => setDiscarding(m)}
                            >
                              <Trash2 className="h-4 w-4" />
                            </Button>
                          </div>
                        ) : (
                          <span className="text-xs text-muted-foreground">
                            —
                          </span>
                        )}
                      </TableCell>
                    </TableRow>
                  );
                })
              )}
            </TableBody>
          </Table>
          {canLoadMore ? (
            <div className="mt-4 flex justify-center">
              <Button
                variant="outline"
                size="sm"
                onClick={loadMore}
                disabled={loading}
              >
                {loading ? "Loading…" : "Load more"}
              </Button>
            </div>
          ) : null}
        </CardContent>
      </Card>

      <LinkDialog
        item={linking}
        busy={busy}
        onClose={() => setLinking(null)}
        onSubmit={link}
      />
      <DiscardDialog
        item={discarding}
        busy={busy}
        onClose={() => setDiscarding(null)}
        onConfirm={() => void discard()}
      />
    </div>
  );
}

/** Link prompt — requires the target booking UUID, validated client-side. */
function LinkDialog({
  item,
  busy,
  onClose,
  onSubmit,
}: {
  item: MediaInboxItem | null;
  busy: boolean;
  onClose: () => void;
  onSubmit: (bookingId: string) => void;
}) {
  const [bookingId, setBookingId] = React.useState("");

  React.useEffect(() => {
    if (item) setBookingId("");
  }, [item]);

  const valid = UUID_RE.test(bookingId.trim());

  return (
    <Dialog open={item !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose} className="max-w-xl">
        <DialogHeader>
          <DialogTitle>Link media to a booking</DialogTitle>
        </DialogHeader>
        <p className="mb-4 text-sm text-muted-foreground">
          Enter the booking UUID this media belongs to. The booking must exist
          in this organisation; linking records the booking&apos;s contact on
          the inbox row and marks it <em>linked</em>.
        </p>
        <div className="grid gap-1.5">
          <Label htmlFor="media-inbox-booking-id">Booking UUID</Label>
          <Input
            id="media-inbox-booking-id"
            value={bookingId}
            onChange={(e) => setBookingId(e.target.value)}
            placeholder="e.g. 3f6b8f2e-…"
            autoComplete="off"
          />
          {bookingId.trim() !== "" && !valid ? (
            <p className="text-xs text-destructive">
              That does not look like a booking UUID.
            </p>
          ) : null}
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            onClick={() => onSubmit(bookingId)}
            disabled={busy || !valid}
          >
            {busy ? "Linking…" : "Link to booking"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** Discard confirmation — the action is a state change, not a byte delete. */
function DiscardDialog({
  item,
  busy,
  onClose,
  onConfirm,
}: {
  item: MediaInboxItem | null;
  busy: boolean;
  onClose: () => void;
  onConfirm: () => void;
}) {
  return (
    <Dialog open={item !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose} className="max-w-xl">
        <DialogHeader>
          <DialogTitle>Discard media</DialogTitle>
        </DialogHeader>
        <p className="mb-4 text-sm text-muted-foreground">
          Discard this media inbox item? It will no longer appear in the
          review queue and cannot be linked afterwards. The extracted OCR
          bytes still expire on the media TTL and remain covered by erasure
          requests.
        </p>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button variant="destructive" onClick={onConfirm} disabled={busy}>
            {busy ? "Discarding…" : "Discard media"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
