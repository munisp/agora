"use client";

import * as React from "react";
import Link from "next/link";
import { ArrowLeft } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { BookingStatusBadge } from "@/components/status-badge";
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
  ConfirmDialog,
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input, Label, Textarea } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import { formatDateTime, titleCase } from "@/lib/utils";
import type { Booking } from "@/lib/types";

/**
 * SPEC-W47 UIWEB-10: booking ops actions. The bookings list can only
 * cancel/reschedule; this detail surface adds the two money-adjacent
 * actions the backend has always exposed:
 *   POST /v1/bookings/{id}/complete  — close out the visit, capture the
 *                                      deposit when one is referenced
 *   POST /v1/bookings/{id}/refund    — W45 K12 fail-closed refund rail
 * Both are manage_bookings + authenticated-subject gated server-side.
 */
export function BookingDetailClient({
  orgSlug,
  bookingId,
  initialBooking,
  initialError,
}: {
  orgSlug: string;
  bookingId: string;
  initialBooking: Booking | null;
  initialError: string | null;
}) {
  const { toast } = useToast();
  const [booking, setBooking] = React.useState<Booking | null>(initialBooking);
  const [error, setError] = React.useState<string | null>(initialError);
  const [busy, setBusy] = React.useState(false);
  const [confirmComplete, setConfirmComplete] = React.useState(false);
  const [refundOpen, setRefundOpen] = React.useState(false);

  const load = React.useCallback(async () => {
    setError(null);
    try {
      const b = await api.get<Booking>(
        `/api/bookings/v1/bookings/${encodeURIComponent(bookingId)}`,
        { tenant: orgSlug },
      );
      setBooking(b);
    } catch (e) {
      setError(
        e instanceof ApiError ? e.message : "Failed to reload the booking.",
      );
    }
  }, [orgSlug, bookingId]);

  const complete = async () => {
    if (!booking) return;
    setBusy(true);
    try {
      // Empty body = complete without referencing a specific deposit hold;
      // the service captures the booking's open hold when one exists.
      await api.post(
        `/api/bookings/v1/bookings/${booking.id}/complete`,
        {},
        { tenant: orgSlug },
      );
      toast({
        title: "Booking completed",
        description: "Any deposit hold was captured.",
        variant: "success",
      });
      setConfirmComplete(false);
      await load();
    } catch (e) {
      toast({
        title: "Complete failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const actionable =
    booking !== null &&
    (booking.status === "pending" || booking.status === "confirmed");

  return (
    <div className="max-w-3xl">
      <Link
        href={`/app/${orgSlug}/bookings`}
        className="mb-4 inline-flex items-center gap-2 text-sm text-muted-foreground hover:text-foreground"
      >
        <ArrowLeft className="h-4 w-4" /> Back to bookings
      </Link>
      <PageHeader
        title={booking ? `Booking · ${formatDateTime(booking.starts_at)}` : "Booking"}
        description={
          booking
            ? `${booking.offering_name ?? booking.offering_id} · ${booking.contact_name ?? "Guest"}`
            : undefined
        }
        actions={
          booking ? <BookingStatusBadge status={booking.status} /> : undefined
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      {booking ? (
        <Card>
          <CardHeader>
            <CardTitle>Details</CardTitle>
            <CardDescription>
              Booking reference {booking.id}
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-3 text-sm">
            <DetailRow label="Guest">
              {booking.contact_name ?? booking.contact_id}
              {booking.contact_phone ? (
                <span className="ml-2 text-xs text-muted-foreground">
                  {booking.contact_phone}
                </span>
              ) : null}
            </DetailRow>
            <DetailRow label="Offering">
              {booking.offering_name ?? booking.offering_id}
            </DetailRow>
            <DetailRow label="With">
              {booking.team_member_name ?? "Any available team member"}
            </DetailRow>
            <DetailRow label="When">
              {formatDateTime(booking.starts_at)} – {formatDateTime(booking.ends_at)}
            </DetailRow>
            <DetailRow label="Source">
              <Badge variant="outline">{titleCase(booking.source)}</Badge>
            </DetailRow>
            <DetailRow label="Created">
              {formatDateTime(booking.created_at)}
            </DetailRow>

            <div className="mt-2 flex flex-wrap gap-2 border-t border-border pt-4">
              {actionable ? (
                <Button
                  variant="secondary"
                  onClick={() => setConfirmComplete(true)}
                  disabled={busy}
                >
                  Mark completed
                </Button>
              ) : null}
              <Button
                variant="ghost"
                className="text-destructive"
                onClick={() => setRefundOpen(true)}
                disabled={busy}
              >
                Refund…
              </Button>
              <Button
                variant="outline"
                size="sm"
                onClick={() => void load()}
                disabled={busy}
              >
                Refresh
              </Button>
            </div>
            <p className="text-xs text-muted-foreground">
              Completing a booking captures its deposit hold (when one exists)
              and emits the completion event. Refunds move real money and are
              recorded with the authenticated operator identity.
            </p>
          </CardContent>
        </Card>
      ) : null}

      <ConfirmDialog
        open={confirmComplete}
        onOpenChange={(open) => !open && setConfirmComplete(false)}
        title="Mark this booking as completed?"
        description={
          booking
            ? `${booking.contact_name ?? "Guest"} · ${formatDateTime(booking.starts_at)}. The deposit hold (if any) is captured and the guest is marked as served.`
            : undefined
        }
        confirmLabel="Complete booking"
        busy={busy}
        onConfirm={() => void complete()}
      />
      <RefundDialog
        orgSlug={orgSlug}
        booking={booking}
        open={refundOpen}
        busy={busy}
        onClose={() => setRefundOpen(false)}
        onBusy={setBusy}
        onDone={() => {
          setRefundOpen(false);
          void load();
        }}
      />
    </div>
  );
}

function DetailRow({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex items-baseline justify-between gap-4">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="text-right font-medium">{children}</dd>
    </div>
  );
}

/**
 * W45 K12 refund rail: amount in major currency units → integer cents, with
 * an idempotency key per dialog opening so a double-submit cannot refund
 * twice.
 */
function RefundDialog({
  orgSlug,
  booking,
  open,
  busy,
  onClose,
  onBusy,
  onDone,
}: {
  orgSlug: string;
  booking: Booking | null;
  open: boolean;
  busy: boolean;
  onClose: () => void;
  onBusy: (b: boolean) => void;
  onDone: () => void;
}) {
  const { toast } = useToast();
  const [amount, setAmount] = React.useState("");
  const [reason, setReason] = React.useState("");
  const [idemKey, setIdemKey] = React.useState("");

  React.useEffect(() => {
    if (open) {
      setAmount("");
      setReason("");
      setIdemKey(crypto.randomUUID());
    }
  }, [open]);

  const amountCents = Math.round(parseFloat(amount || "0") * 100);
  const valid = Number.isFinite(amountCents) && amountCents > 0;

  const submit = async () => {
    if (!booking || !valid) return;
    onBusy(true);
    try {
      await api.post(
        `/api/bookings/v1/bookings/${booking.id}/refund`,
        {
          amount_cents: amountCents,
          reason: reason.trim() || undefined,
          idempotency_key: idemKey,
        },
        { tenant: orgSlug },
      );
      toast({ title: "Refund submitted", variant: "success" });
      onDone();
    } catch (e) {
      toast({
        title: "Refund failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      onBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose}>
        <DialogHeader>
          <DialogTitle>Refund booking</DialogTitle>
        </DialogHeader>
        <p className="mb-4 text-sm text-muted-foreground">
          Refunds move real money through the payments rail and are recorded
          against your operator identity. This cannot be undone from the UI.
        </p>
        <div className="grid gap-4">
          <div className="grid gap-1.5">
            <Label htmlFor="refund-amount">Amount</Label>
            <Input
              id="refund-amount"
              type="number"
              min="0.01"
              step="0.01"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
              placeholder="0.00"
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="refund-reason">Reason (optional)</Label>
            <Textarea
              id="refund-reason"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="e.g. service not rendered"
            />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Back
          </Button>
          <Button
            variant="destructive"
            onClick={() => void submit()}
            disabled={busy || !valid}
          >
            {busy ? "Refunding…" : "Refund"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
