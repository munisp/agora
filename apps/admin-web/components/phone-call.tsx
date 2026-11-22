"use client";

/**
 * Outbound call buttons (2026-11-22): staff ask the receptionist to phone a contributor.
 *
 *   POST /api/voice-admin/calls { site_slug, phone_number, purpose, contact_name, context }
 *   (BFF -> APISIX voice-admin route: signed in; a business's owner/admin for their own business, agency staff for any)
 *
 * The receptionist opens with who is calling and checks it is speaking to the right person before it says anything
 * about the appointment; the call then appears in Conversations as a "Phone call" (voice-agent-runtime
 * app/outbound_script.py).
 */
import * as React from "react";
import { PhoneOutgoing } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input, Label } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import type { AvailabilitySlot, TeamMember } from "@/lib/types";
import { listFrom } from "@/lib/utils";

export type CallPurpose = "confirm_appointment" | "waitlist_offer" | "no_show_rebook" | "callback";

export interface CallTarget {
  purpose: CallPurpose;
  phone: string;
  name?: string | null;
  /** Facts the receptionist reads out: offering, team_member, when, reason. */
  context?: Record<string, string>;
}

const TITLES: Record<CallPurpose, string> = {
  confirm_appointment: "Call to confirm",
  waitlist_offer: "Offer slot by phone",
  no_show_rebook: "Call to rebook",
  callback: "Call back",
};

/** "Tuesday 24 November at 9:00 am": how the receptionist says a time (the business's own clock on this screen). */
export function spokenWhen(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const day = d.toLocaleDateString("en-GB", { weekday: "long", day: "numeric", month: "long" });
  const time = d.toLocaleTimeString("en-GB", { hour: "numeric", minute: "2-digit", hour12: true }).toLowerCase();
  return `${day} at ${time}`;
}

/** What the receptionist will say, shown to staff before they place the call. */
function preview(t: CallTarget): string {
  const c = t.context ?? {};
  const about = [c.offering ? `the ${c.offering}` : "the appointment", c.team_member ? `with ${c.team_member}` : "", c.when ? `on ${c.when}` : ""]
    .filter(Boolean)
    .join(" ");
  switch (t.purpose) {
    case "confirm_appointment":
      return `The receptionist will ask ${t.name || "them"} to confirm ${about}, and offer a new time if they can't make it.`;
    case "no_show_rebook":
      return `The receptionist will say we missed ${t.name || "them"} at ${about} and offer to book a new time.`;
    case "waitlist_offer":
      return `The receptionist will offer ${t.name || "them"} ${about} and book it if they say yes.`;
    default:
      return `The receptionist will call ${t.name || "them"} back${c.reason ? ` about ${c.reason}` : ""} and help with what they need.`;
  }
}

async function placeCall(orgSlug: string, t: CallTarget) {
  return api.post<{ status: string; room: string }>(
    "/api/voice-admin/calls",
    { site_slug: orgSlug, phone_number: t.phone, purpose: t.purpose, contact_name: t.name || undefined, context: t.context ?? {} },
    { tenant: orgSlug },
  );
}

function useCall(orgSlug: string, onDone: () => void) {
  const { toast } = useToast();
  const [busy, setBusy] = React.useState(false);
  const call = async (t: CallTarget) => {
    setBusy(true);
    try {
      await placeCall(orgSlug, t);
      toast({
        title: `Calling ${t.name || t.phone}…`,
        description: "The receptionist is dialing now. The call and its transcript appear in Conversations when it ends.",
        variant: "success",
      });
      onDone();
    } catch (e) {
      toast({
        title: "The call could not be placed",
        description: e instanceof ApiError ? e.message : "Please try again in a moment.",
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };
  return { busy, call };
}

/** Small row button that opens the call dialog. */
export function CallButton({ purpose, onClick, disabled }: { purpose: CallPurpose; onClick: () => void; disabled?: boolean }) {
  return (
    <Button variant="outline" size="sm" onClick={onClick} disabled={disabled}>
      <PhoneOutgoing className="h-3.5 w-3.5" />
      {TITLES[purpose]}
    </Button>
  );
}

/** Confirm-and-call dialog for confirm / rebook / call-back (call-back lets staff say what it's about). */
export function PhoneCallDialog({ orgSlug, target, onClose }: { orgSlug: string; target: CallTarget | null; onClose: () => void }) {
  const { busy, call } = useCall(orgSlug, onClose);
  const [reason, setReason] = React.useState("");
  React.useEffect(() => setReason(target?.context?.reason ?? ""), [target]);
  if (!target) return null;
  const t: CallTarget = target.purpose === "callback"
    ? { ...target, context: { ...(target.context ?? {}), ...(reason.trim() ? { reason: reason.trim() } : {}) } }
    : target;
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent onClose={onClose}>
        <DialogHeader>
          <DialogTitle>{TITLES[target.purpose]}</DialogTitle>
          <DialogDescription>
            {target.name ? `${target.name} · ` : ""}
            {target.phone}
          </DialogDescription>
        </DialogHeader>
        {target.purpose === "callback" ? (
          <div className="space-y-1.5">
            <Label htmlFor="call-reason">What is it about? (optional)</Label>
            <Input id="call-reason" value={reason} maxLength={150} placeholder="e.g. their RSA statement"
                   onChange={(e) => setReason(e.target.value)} />
          </div>
        ) : null}
        <p className="text-sm text-muted-foreground">{preview(t)}</p>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>Not now</Button>
          <Button onClick={() => void call(t)} disabled={busy}>
            <PhoneOutgoing className="h-3.5 w-3.5" />
            {busy ? "Dialing…" : "Place the call"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export interface WaitlistCallTarget {
  name?: string | null;
  phone: string;
  offeringId: string;
  offeringName: string;
  windowStart: string;
  windowEnd: string;
}

interface OfferableSlot extends AvailabilitySlot {
  teamMemberName: string;
}

/**
 * Waitlist offer: the receptionist must offer a REAL free slot with a named advisor (without one it checks advisors
 * one by one and runs out of steps before it can book), so staff pick one of the free slots in the person's window.
 */
export function OfferSlotCallDialog({ orgSlug, target, onClose }: { orgSlug: string; target: WaitlistCallTarget | null; onClose: () => void }) {
  const { busy, call } = useCall(orgSlug, onClose);
  const [slots, setSlots] = React.useState<OfferableSlot[] | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [picked, setPicked] = React.useState<number | null>(null);

  React.useEffect(() => {
    if (!target) return;
    let cancelled = false;
    setSlots(null);
    setError(null);
    setPicked(null);
    (async () => {
      try {
        const now = Date.now();
        const from = new Date(Math.max(now, new Date(target.windowStart).getTime()));
        const end = new Date(target.windowEnd).getTime();
        const to = new Date(Math.min(end, from.getTime() + 7 * 86400000));
        if (to.getTime() <= from.getTime()) {
          if (!cancelled) setSlots([]);
          return;
        }
        const raw = await api.get<unknown>("/api/bookings/v1/team-members", { tenant: orgSlug });
        const team = listFrom<TeamMember>(raw).filter((m) => m.active !== false);
        const found: OfferableSlot[] = [];
        await Promise.all(
          team.map(async (m) => {
            const data = await api.get<unknown>(
              `/api/bookings/public/sites/${orgSlug}/availability`,
              { offering_id: target.offeringId, team_member_id: m.id, from: from.toISOString(), to: to.toISOString() },
            );
            for (const s of listFrom<AvailabilitySlot>(data)) found.push({ ...s, teamMemberName: m.name });
          }),
        );
        found.sort((a, b) => a.starts_at.localeCompare(b.starts_at) || a.teamMemberName.localeCompare(b.teamMemberName));
        if (!cancelled) setSlots(found.slice(0, 12));
      } catch (e) {
        if (!cancelled) setError(e instanceof ApiError ? e.message : "Free slots could not be loaded right now.");
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [orgSlug, target]);

  if (!target) return null;
  const slot = picked !== null && slots ? slots[picked] : null;
  const callTarget: CallTarget | null = slot
    ? {
        purpose: "waitlist_offer",
        phone: target.phone,
        name: target.name,
        context: { offering: target.offeringName, team_member: slot.teamMemberName, when: spokenWhen(slot.starts_at) },
      }
    : null;
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent onClose={onClose}>
        <DialogHeader>
          <DialogTitle>{TITLES.waitlist_offer}</DialogTitle>
          <DialogDescription>
            {target.name ? `${target.name} · ` : ""}
            {target.phone} · {target.offeringName}
          </DialogDescription>
        </DialogHeader>
        {error ? <p className="text-sm text-destructive">{error}</p> : null}
        {slots === null && !error ? <p className="text-sm text-muted-foreground">Finding free slots in their window…</p> : null}
        {slots && slots.length === 0 ? (
          <p className="text-sm text-muted-foreground">No free slots in this person&apos;s window (or the window has passed).</p>
        ) : null}
        {slots && slots.length > 0 ? (
          <fieldset className="space-y-1.5" aria-label="Slot to offer">
            <legend className="mb-1 text-sm font-medium">Which slot should the receptionist offer?</legend>
            {slots.map((s, i) => (
              <label key={`${s.starts_at}-${s.teamMemberName}`} className="flex cursor-pointer items-center gap-2 rounded-md border px-3 py-2 text-sm hover:bg-accent">
                <input type="radio" name="offer-slot" checked={picked === i} onChange={() => setPicked(i)} />
                <span>{spokenWhen(s.starts_at)}</span>
                <span className="text-muted-foreground">· {s.teamMemberName}</span>
              </label>
            ))}
          </fieldset>
        ) : null}
        {callTarget ? <p className="text-sm text-muted-foreground">{preview(callTarget)}</p> : null}
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>Not now</Button>
          <Button onClick={() => callTarget && void call(callTarget)} disabled={busy || !callTarget}>
            <PhoneOutgoing className="h-3.5 w-3.5" />
            {busy ? "Dialing…" : "Place the call"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
