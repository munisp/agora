"use client";

import * as React from "react";
import { ArrowLeft, ArrowRight, CheckCircle2, Clock, Users } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { ChatWidget } from "@/components/chat-widget";
import { VoiceSessionButton } from "@/components/voice-session-button";
import { CalendarLite } from "@/components/ui/calendar";
import { Input, Label, Textarea } from "@/components/ui/input";
import { cn, formatMoney, formatTime, toISODate } from "@/lib/utils";
import type { AvailabilitySlot, Offering, PublicSite } from "@/lib/types";

type Step = "offering" | "slot" | "details" | "confirm" | "done";

interface Details {
  name: string;
  phone: string;
  email: string;
  notes: string;
}

export function PublicBookingClient({
  site,
  offerings,
  embed = false,
}: {
  site: PublicSite;
  offerings: Offering[];
  /** chromeless rendering for the /embed/[siteSlug] iframe widget */
  embed?: boolean;
}) {
  const accent = site.theme?.primaryColor ?? site.theme?.accent ?? "#7c5b3e";
  const logoUrl = site.theme?.logoUrl ?? site.theme?.logo_url;
  // White-label (SPEC-W7 Part C): brand name overrides the tenant business
  // name in the public header/footer when set in the theme editor.
  const brandName =
    site.theme?.brandName ?? site.theme?.brand_name ?? site.business_name;
  const heroTitle = site.theme?.heroTitle;
  const heroSubtitle = site.theme?.heroSubtitle ?? site.theme?.hero_blurb;
  const compact = embed || site.theme?.template === "compact";
  const [step, setStep] = React.useState<Step>("offering");
  const [offering, setOffering] = React.useState<Offering | null>(null);
  const [date, setDate] = React.useState<string>(toISODate(new Date()));
  const [slots, setSlots] = React.useState<AvailabilitySlot[]>([]);
  const [slotsLoading, setSlotsLoading] = React.useState(false);
  const [slot, setSlot] = React.useState<AvailabilitySlot | null>(null);
  const [details, setDetails] = React.useState<Details>({
    name: "",
    phone: "",
    email: "",
    notes: "",
  });
  const [phoneConfirm, setPhoneConfirm] = React.useState("");
  const [submitting, setSubmitting] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [bookingRef, setBookingRef] = React.useState<string | null>(null);

  // Load availability slots whenever offering/date changes.
  React.useEffect(() => {
    if (!offering) return;
    let cancelled = false;
    setSlotsLoading(true);
    setSlot(null);
    api
      .get<AvailabilitySlot[] | { items: AvailabilitySlot[] }>(
        `/api/bookings/public/sites/${site.site_slug}/availability`,
        { offering_id: offering.id, date },
      )
      .then((data) => {
        if (cancelled) return;
        setSlots(Array.isArray(data) ? data : (data.items ?? []));
      })
      .catch(() => {
        if (!cancelled) setSlots([]);
      })
      .finally(() => {
        if (!cancelled) setSlotsLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [offering, date, site.site_slug]);

  const detailsValid =
    details.name.trim().length > 1 &&
    details.phone.replace(/[^\d+]/g, "").length >= 7;
  const phoneMatches =
    phoneConfirm.replace(/\D/g, "") !== "" &&
    phoneConfirm.replace(/\D/g, "") === details.phone.replace(/\D/g, "");

  const submit = async () => {
    if (!offering || !slot || !phoneMatches) return;
    setSubmitting(true);
    setError(null);
    try {
      const res = await api.post<{ id: string }>(
        `/api/bookings/public/sites/${site.site_slug}/bookings`,
        {
          offering_id: offering.id,
          starts_at: slot.starts_at,
          team_member_id: slot.team_member_id ?? undefined,
          contact: {
            name: details.name.trim(),
            phone: details.phone.trim(),
            email: details.email.trim() || undefined,
          },
          notes: details.notes.trim() || undefined,
          // Platform policy: mutations require a confirmed phone number.
          phone_confirmed: true,
          idempotency_key: crypto.randomUUID(),
        },
      );
      setBookingRef(res.id);
      setStep("done");
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Booking failed. Please try again.");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="min-h-screen bg-background">
      <header className="border-b border-border bg-card">
        <div className={cn("mx-auto max-w-3xl px-6", compact ? "py-4" : "py-8")}>
          <div className="flex items-center gap-3">
            {logoUrl ? (
              // eslint-disable-next-line @next/next/no-img-element
              <img
                src={logoUrl}
                alt={`${brandName} logo`}
                className="h-10 w-10 rounded-md border border-border object-contain"
              />
            ) : null}
            <div>
              <h1 className={cn("font-bold tracking-tight", compact ? "text-xl" : "text-3xl")}>
                {brandName}
              </h1>
              {site.tagline ? (
                <p className="mt-1 text-muted-foreground">{site.tagline}</p>
              ) : null}
            </div>
          </div>
          {heroTitle ? (
            <p className="mt-3 text-lg font-medium">{heroTitle}</p>
          ) : null}
          {heroSubtitle ? (
            <p className="mt-2 max-w-xl text-sm text-muted-foreground">
              {heroSubtitle}
            </p>
          ) : null}
          {!embed ? (
            <div className="mt-5">
              <VoiceSessionButton
                tenant={site.tenant_slug}
                siteSlug={site.site_slug}
                accent={accent}
              />
            </div>
          ) : null}
        </div>
      </header>

      <main className="mx-auto max-w-3xl px-6 py-10">
        {/* Stepper */}
        <ol className="mb-8 flex flex-wrap items-center gap-2 text-xs font-medium text-muted-foreground">
          {["Choose a service", "Pick a time", "Your details", "Confirm phone"].map(
            (label, i) => {
              const order: Step[] = ["offering", "slot", "details", "confirm"];
              const active = order.indexOf(step) === i;
              const complete = order.indexOf(step) > i || step === "done";
              return (
                <li key={label} className="flex items-center gap-2">
                  <span
                    className={cn(
                      "flex h-6 w-6 items-center justify-center rounded-full border",
                      active && "border-transparent text-white",
                      complete && "border-transparent bg-success text-white",
                      !active && !complete && "border-border",
                    )}
                    style={active ? { backgroundColor: accent } : undefined}
                  >
                    {i + 1}
                  </span>
                  <span className={cn(active && "text-foreground")}>{label}</span>
                  {i < 3 ? <span className="text-border">—</span> : null}
                </li>
              );
            },
          )}
        </ol>

        {step === "offering" ? (
          <div className="grid gap-4 sm:grid-cols-2">
            {offerings.length === 0 ? (
              <p className="col-span-2 rounded-lg border border-border bg-card p-8 text-center text-sm text-muted-foreground">
                Online booking is not available right now — please call us or
                use the chat.
              </p>
            ) : (
              offerings.map((o) => (
                <button
                  key={o.id}
                  onClick={() => {
                    setOffering(o);
                    setStep("slot");
                  }}
                  className="rounded-lg border border-border bg-card p-5 text-left shadow-sm transition-shadow hover:shadow-md cursor-pointer"
                >
                  <p className="font-semibold">{o.name}</p>
                  {o.description ? (
                    <p className="mt-1 line-clamp-2 text-sm text-muted-foreground">
                      {o.description}
                    </p>
                  ) : null}
                  <div className="mt-3 flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
                    <span className="inline-flex items-center gap-1">
                      <Clock className="h-3.5 w-3.5" /> {o.duration_min} min
                    </span>
                    {o.capacity > 1 ? (
                      <span className="inline-flex items-center gap-1">
                        <Users className="h-3.5 w-3.5" /> up to {o.capacity}
                      </span>
                    ) : null}
                    <span
                      className="rounded-full px-2 py-0.5 font-medium text-white"
                      style={{ backgroundColor: accent }}
                    >
                      {formatMoney(o.price_cents, o.currency, site.locale)}
                    </span>
                  </div>
                </button>
              ))
            )}
          </div>
        ) : null}

        {step === "slot" && offering ? (
          <div>
            <BackButton onClick={() => setStep("offering")} label={offering.name} />
            <div className="flex flex-wrap items-start gap-8">
              <CalendarLite
                selected={date}
                onSelect={setDate}
                minDate={toISODate(new Date())}
              />
              <div className="min-w-56 flex-1">
                <p className="mb-3 text-sm font-medium">
                  Available times ·{" "}
                  {new Date(`${date}T00:00:00`).toLocaleDateString(site.locale, {
                    weekday: "long",
                    month: "long",
                    day: "numeric",
                  })}
                </p>
                {slotsLoading ? (
                  <p className="text-sm text-muted-foreground">Checking availability…</p>
                ) : slots.length === 0 ? (
                  <p className="text-sm text-muted-foreground">
                    No free slots this day — try another date.
                  </p>
                ) : (
                  <div className="grid grid-cols-3 gap-2">
                    {slots.map((s) => (
                      <button
                        key={s.starts_at}
                        onClick={() => {
                          setSlot(s);
                          setStep("details");
                        }}
                        className={cn(
                          "rounded-md border border-border bg-card px-2 py-2 text-sm font-medium transition-colors cursor-pointer hover:border-transparent hover:text-white",
                        )}
                        onMouseEnter={(e) =>
                          (e.currentTarget.style.backgroundColor = accent)
                        }
                        onMouseLeave={(e) =>
                          (e.currentTarget.style.backgroundColor = "")
                        }
                      >
                        {formatTime(s.starts_at, site.locale, site.timezone)}
                      </button>
                    ))}
                  </div>
                )}
              </div>
            </div>
          </div>
        ) : null}

        {step === "details" && offering && slot ? (
          <div className="max-w-md">
            <BackButton
              onClick={() => setStep("slot")}
              label={`${offering.name} · ${formatTime(slot.starts_at, site.locale, site.timezone)}`}
            />
            <div className="grid gap-4">
              <div className="grid gap-1.5">
                <Label htmlFor="pb-name">Full name</Label>
                <Input
                  id="pb-name"
                  value={details.name}
                  onChange={(e) => setDetails((d) => ({ ...d, name: e.target.value }))}
                  placeholder="Jordan Mensah"
                />
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="pb-phone">Phone number</Label>
                <Input
                  id="pb-phone"
                  type="tel"
                  value={details.phone}
                  onChange={(e) => setDetails((d) => ({ ...d, phone: e.target.value }))}
                  placeholder="+1 555 010 2345"
                />
                <p className="text-xs text-muted-foreground">
                  We confirm every booking by phone before it is final.
                </p>
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="pb-email">Email (optional)</Label>
                <Input
                  id="pb-email"
                  type="email"
                  value={details.email}
                  onChange={(e) => setDetails((d) => ({ ...d, email: e.target.value }))}
                  placeholder="jordan@example.com"
                />
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="pb-notes">Anything we should know? (optional)</Label>
                <Textarea
                  id="pb-notes"
                  value={details.notes}
                  onChange={(e) => setDetails((d) => ({ ...d, notes: e.target.value }))}
                />
              </div>
              <button
                onClick={() => setStep("confirm")}
                disabled={!detailsValid}
                className="inline-flex items-center justify-center gap-2 rounded-md px-4 py-2.5 text-sm font-medium text-white disabled:opacity-50 cursor-pointer"
                style={{ backgroundColor: accent }}
              >
                Continue <ArrowRight className="h-4 w-4" />
              </button>
            </div>
          </div>
        ) : null}

        {step === "confirm" && offering && slot ? (
          <div className="max-w-md">
            <BackButton onClick={() => setStep("details")} label="Edit details" />
            <div className="rounded-lg border border-border bg-card p-5">
              <h2 className="font-semibold">Confirm your phone number</h2>
              <p className="mt-1 text-sm text-muted-foreground">
                For your protection, bookings must be confirmed from the phone
                number on the appointment. Re-enter your number to finish.
              </p>
              <dl className="mt-4 space-y-1 rounded-md bg-muted/60 p-3 text-sm">
                <div className="flex justify-between">
                  <dt className="text-muted-foreground">Service</dt>
                  <dd className="font-medium">{offering.name}</dd>
                </div>
                <div className="flex justify-between">
                  <dt className="text-muted-foreground">When</dt>
                  <dd className="font-medium">
                    {new Date(slot.starts_at).toLocaleString(site.locale, {
                      dateStyle: "medium",
                      timeStyle: "short",
                      timeZone: site.timezone,
                    })}
                  </dd>
                </div>
                <div className="flex justify-between">
                  <dt className="text-muted-foreground">Name</dt>
                  <dd className="font-medium">{details.name}</dd>
                </div>
                <div className="flex justify-between">
                  <dt className="text-muted-foreground">Phone</dt>
                  <dd className="font-medium">{details.phone}</dd>
                </div>
              </dl>
              <div className="mt-4 grid gap-1.5">
                <Label htmlFor="pb-confirm">Re-enter phone number</Label>
                <Input
                  id="pb-confirm"
                  type="tel"
                  value={phoneConfirm}
                  onChange={(e) => setPhoneConfirm(e.target.value)}
                  placeholder={details.phone}
                />
                {phoneConfirm && !phoneMatches ? (
                  <p className="text-xs text-destructive">
                    Numbers don&apos;t match.
                  </p>
                ) : null}
              </div>
              {error ? (
                <p className="mt-3 rounded-md bg-danger-soft px-3 py-2 text-sm text-destructive">
                  {error}
                </p>
              ) : null}
              <button
                onClick={() => void submit()}
                disabled={!phoneMatches || submitting}
                className="mt-4 inline-flex w-full items-center justify-center gap-2 rounded-md px-4 py-2.5 text-sm font-medium text-white disabled:opacity-50 cursor-pointer"
                style={{ backgroundColor: accent }}
              >
                {submitting ? "Booking…" : "Confirm booking"}
              </button>
            </div>
          </div>
        ) : null}

        {step === "done" ? (
          <div className="mx-auto max-w-md rounded-lg border border-border bg-card p-8 text-center">
            <CheckCircle2 className="mx-auto h-10 w-10 text-success" />
            <h2 className="mt-3 text-xl font-semibold">You&apos;re booked!</h2>
            <p className="mt-2 text-sm text-muted-foreground">
              {offering?.name} ·{" "}
              {slot
                ? new Date(slot.starts_at).toLocaleString(site.locale, {
                    dateStyle: "full",
                    timeStyle: "short",
                    timeZone: site.timezone,
                  })
                : ""}
            </p>
            <p className="mt-4 text-sm text-muted-foreground">
              A confirmation is on its way to {details.phone}. Need to change
              or cancel? Just use the chat — our receptionist can handle it.
            </p>
            {bookingRef ? (
              <p className="mt-3 font-mono text-xs text-muted-foreground">
                ref {bookingRef}
              </p>
            ) : null}
            <button
              onClick={() => {
                setStep("offering");
                setOffering(null);
                setSlot(null);
                setPhoneConfirm("");
                setBookingRef(null);
              }}
              className="mt-6 inline-flex items-center gap-2 rounded-md border border-border px-4 py-2 text-sm font-medium hover:bg-accent cursor-pointer"
            >
              Book another <ArrowRight className="h-4 w-4" />
            </button>
          </div>
        ) : null}
      </main>

      {!embed ? (
        <footer className="border-t border-border py-6 text-center text-xs text-muted-foreground">
          {brandName} · Powered by Agora — open-source AI receptionist
        </footer>
      ) : null}

      <ChatWidget tenant={site.tenant_slug} siteSlug={site.site_slug} accent={accent} />
    </div>
  );
}

function BackButton({ onClick, label }: { onClick: () => void; label: string }) {
  return (
    <button
      onClick={onClick}
      className="mb-5 inline-flex items-center gap-2 text-sm text-muted-foreground hover:text-foreground cursor-pointer"
    >
      <ArrowLeft className="h-4 w-4" /> {label}
    </button>
  );
}
