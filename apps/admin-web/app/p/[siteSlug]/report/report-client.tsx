"use client";

/**
 * SPEC-W32 WS-C: PUBLIC civic report intake — no auth, mobile-first.
 *
 * Unauthenticated civic endpoints (APISIX public route, rate-limited at the
 * gateway; the BFF forwards without a session):
 *
 *   GET  /api/civic/public/tenants/{slug}/categories   active categories
 *   GET  /api/civic/public/tenants/{slug}/stats        ward name suggestions
 *   POST /api/civic/public/tenants/{slug}/reports      {category_slug,
 *        description (10..2000), ward?, lat?, lon?, location_text?,
 *        reporter_phone_e164?, reporter_name?, anonymous?, wants_updates?,
 *        website (honeypot — must stay empty)}
 *        → {ref, ack_due_at}
 *
 * Abuse posture (SPEC §0.2/§4 gate 6): honeypot field named `website`,
 * gateway + service-side throttling. A 404 on categories renders a clean
 * "reporting not available" state — never an error wall for citizens.
 */
import * as React from "react";
import Link from "next/link";
import {
  CheckCircle2,
  ClipboardCopy,
  Loader2,
  LocateFixed,
  Search,
} from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input, Label, Select, Textarea } from "@/components/ui/input";
import { cn, formatDateTime } from "@/lib/utils";
import type { PublicSite } from "@/lib/types";

interface PublicCategory {
  slug: string;
  name: string;
}

function normalizeCategories(data: unknown): PublicCategory[] {
  const rows = Array.isArray(data)
    ? data
    : typeof data === "object" && data !== null
      ? (Object.values(data).find(Array.isArray) as unknown[] | undefined) ?? []
      : [];
  return rows
    .map((r) => {
      const o = (typeof r === "object" && r !== null ? r : {}) as Record<
        string,
        unknown
      >;
      return {
        slug: typeof o.slug === "string" ? o.slug : "",
        name:
          typeof o.name === "string"
            ? o.name
            : typeof o.slug === "string"
              ? o.slug
              : "Category",
      };
    })
    .filter((c) => c.slug !== "");
}

/** Ward names surface anywhere in the stats payload — collect them tolerantly. */
function wardsFromStats(data: unknown): string[] {
  const out = new Set<string>();
  const walk = (v: unknown, key = "") => {
    if (typeof v === "string" && /ward/i.test(key) && v) out.add(v);
    else if (Array.isArray(v)) v.forEach((x) => walk(x, key));
    else if (typeof v === "object" && v !== null) {
      for (const [k, x] of Object.entries(v)) {
        if (k === "ward" && typeof x === "string" && x) out.add(x);
        else walk(x, k);
      }
    }
  };
  walk(data);
  return [...out].sort();
}

const OTHER_WARD = "__other__";

export function PublicReportClient({ site }: { site: PublicSite }) {
  const brandName =
    site.theme?.brandName ?? site.theme?.brand_name ?? site.business_name;
  const base = `/api/civic/public/tenants/${site.site_slug}`;

  const [categories, setCategories] = React.useState<PublicCategory[]>([]);
  const [catLoading, setCatLoading] = React.useState(true);
  const [catUnavailable, setCatUnavailable] = React.useState(false);
  const [wards, setWards] = React.useState<string[]>([]);

  const [category, setCategory] = React.useState("");
  const [description, setDescription] = React.useState("");
  const [ward, setWard] = React.useState("");
  const [wardOther, setWardOther] = React.useState("");
  const [locationText, setLocationText] = React.useState("");
  const [coords, setCoords] = React.useState<{ lat: number; lon: number } | null>(
    null,
  );
  const [locating, setLocating] = React.useState(false);
  const [locNote, setLocNote] = React.useState<string | null>(null);
  const [name, setName] = React.useState("");
  const [phone, setPhone] = React.useState("");
  const [wantsUpdates, setWantsUpdates] = React.useState(true);
  const [anonymous, setAnonymous] = React.useState(false);
  // Honeypot — must stay empty (SPEC §3 WS-A).
  const [website, setWebsite] = React.useState("");

  const [submitting, setSubmitting] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [result, setResult] = React.useState<{
    ref: string;
    ack_due_at: string | null;
  } | null>(null);
  const [copied, setCopied] = React.useState(false);

  React.useEffect(() => {
    let cancelled = false;
    api
      .get<unknown>(`${base}/categories`)
      .then((data) => {
        if (cancelled) return;
        setCategories(normalizeCategories(data));
      })
      .catch((e) => {
        if (cancelled) return;
        setCategories([]);
        if (e instanceof ApiError && e.status === 404) setCatUnavailable(true);
      })
      .finally(() => {
        if (!cancelled) setCatLoading(false);
      });
    // Ward suggestions are best-effort — the form works without them.
    api
      .get<unknown>(`${base}/stats`)
      .then((data) => {
        if (!cancelled) setWards(wardsFromStats(data));
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [base]);

  const useMyLocation = () => {
    if (!("geolocation" in navigator)) {
      setLocNote("Location is not supported on this device.");
      return;
    }
    setLocating(true);
    setLocNote(null);
    navigator.geolocation.getCurrentPosition(
      (pos) => {
        setCoords({ lat: pos.coords.latitude, lon: pos.coords.longitude });
        setLocating(false);
        setLocNote("Location attached — thank you.");
      },
      () => {
        setLocating(false);
        setLocNote("Could not get your location. You can describe it instead.");
      },
      { enableHighAccuracy: true, timeout: 10_000, maximumAge: 60_000 },
    );
  };

  const descLen = description.trim().length;
  const descValid = descLen >= 10 && descLen <= 2000;
  const effectiveWard = ward === OTHER_WARD ? wardOther.trim() : ward;
  const phoneTrim = phone.trim();
  const updatesNeedPhone = wantsUpdates && phoneTrim === "";

  const submit = async () => {
    if (!category || !descValid || updatesNeedPhone) return;
    setSubmitting(true);
    setError(null);
    try {
      const res = await api.post<{ ref?: string; ack_due_at?: string }>(
        `${base}/reports`,
        {
          category_slug: category,
          description: description.trim(),
          ward: effectiveWard || undefined,
          lat: coords?.lat,
          lon: coords?.lon,
          location_text: locationText.trim() || undefined,
          reporter_phone_e164: phoneTrim || undefined,
          reporter_name: name.trim() || undefined,
          anonymous: anonymous || undefined,
          wants_updates: wantsUpdates && phoneTrim !== "" ? true : undefined,
          website, // honeypot — always "" for humans
        },
      );
      setResult({
        ref: typeof res.ref === "string" && res.ref ? res.ref : "RECEIVED",
        ack_due_at: typeof res.ack_due_at === "string" ? res.ack_due_at : null,
      });
    } catch (e) {
      setError(
        e instanceof ApiError
          ? e.status === 429
            ? "Too many reports from this device — please try again later."
            : e.message
          : "Your report could not be sent. Please try again.",
      );
    } finally {
      setSubmitting(false);
    }
  };

  const copyRef = async () => {
    if (!result) return;
    try {
      await navigator.clipboard.writeText(result.ref);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      setCopied(false);
    }
  };

  return (
    <div className="min-h-screen bg-background">
      <header className="border-b border-border bg-card">
        <div className="mx-auto max-w-xl px-5 py-6">
          <h1 className="text-2xl font-bold tracking-tight">{brandName}</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Report a public issue — no account needed.
          </p>
        </div>
      </header>

      <main className="mx-auto max-w-xl px-5 py-6">
        {result ? (
          /* ------------------------------------------------ success screen */
          <div className="rounded-lg border border-border bg-card px-6 py-10 text-center shadow-sm">
            <CheckCircle2
              className="mx-auto h-12 w-12"
              style={{ color: "#7A8B6F" }}
            />
            <h2 className="mt-3 text-xl font-semibold">Report received</h2>
            <p className="mt-1 text-sm text-muted-foreground">
              Save this reference number — you need it (with your phone number)
              to track your report.
            </p>
            <p className="mt-5 break-all rounded-md border border-border bg-muted px-4 py-3 font-mono text-2xl font-bold tracking-wide">
              {result.ref}
            </p>
            {result.ack_due_at ? (
              <p className="mt-3 text-sm text-muted-foreground">
                We aim to acknowledge your report by{" "}
                <span className="font-medium text-foreground">
                  {formatDateTime(result.ack_due_at)}
                </span>
                .
              </p>
            ) : null}
            <div className="mt-5 flex flex-col items-center gap-2">
              <Button variant="outline" onClick={() => void copyRef()}>
                <ClipboardCopy className="h-4 w-4" />
                {copied ? "Copied!" : "Copy reference number"}
              </Button>
              <Link
                href={`/p/${site.site_slug}/track`}
                className="inline-flex items-center gap-2 rounded-md bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:bg-primary-hover"
              >
                <Search className="h-4 w-4" /> Track this report
              </Link>
              <button
                type="button"
                onClick={() => {
                  setResult(null);
                  setDescription("");
                  setCategory("");
                  setLocationText("");
                  setCoords(null);
                }}
                className="mt-2 text-sm text-muted-foreground underline underline-offset-2 hover:text-foreground cursor-pointer"
              >
                Report another issue
              </button>
            </div>
          </div>
        ) : (
          /* ------------------------------------------------------ intake */
          <div className="space-y-5 rounded-lg border border-border bg-card px-5 py-6 shadow-sm">
            {catLoading ? (
              <p className="flex items-center gap-2 text-sm text-muted-foreground">
                <Loader2 className="h-4 w-4 animate-spin" /> Loading…
              </p>
            ) : catUnavailable ? (
              <p className="text-sm text-muted-foreground">
                Public reporting is not available for {brandName} yet. Please
                check back soon.
              </p>
            ) : (
              <>
                <div className="space-y-1.5">
                  <Label htmlFor="rpt-category">What kind of issue? *</Label>
                  <Select
                    id="rpt-category"
                    className="h-11 text-base"
                    value={category}
                    onChange={(e) => setCategory(e.target.value)}
                  >
                    <option value="">Choose a category…</option>
                    {categories.map((c) => (
                      <option key={c.slug} value={c.slug}>
                        {c.name}
                      </option>
                    ))}
                  </Select>
                </div>

                <div className="space-y-1.5">
                  <Label htmlFor="rpt-description">Describe the issue *</Label>
                  <Textarea
                    id="rpt-description"
                    className="min-h-28 text-base"
                    placeholder="What is wrong, and where exactly? (10–2000 characters)"
                    maxLength={2000}
                    value={description}
                    onChange={(e) => setDescription(e.target.value)}
                  />
                  <p
                    className={cn(
                      "text-xs",
                      descValid
                        ? "text-muted-foreground"
                        : descLen > 0
                          ? "text-[#C0562F]"
                          : "text-muted-foreground",
                    )}
                  >
                    {descLen}/2000 characters{descLen > 0 && descLen < 10 ? " — at least 10 needed" : ""}
                  </p>
                </div>

                <div className="space-y-1.5">
                  <Label htmlFor="rpt-ward">Ward</Label>
                  {wards.length > 0 ? (
                    <Select
                      id="rpt-ward"
                      className="h-11 text-base"
                      value={ward}
                      onChange={(e) => setWard(e.target.value)}
                    >
                      <option value="">Choose your ward…</option>
                      {wards.map((w) => (
                        <option key={w} value={w}>
                          {w}
                        </option>
                      ))}
                      <option value={OTHER_WARD}>Other / not listed</option>
                    </Select>
                  ) : null}
                  {wards.length === 0 || ward === OTHER_WARD ? (
                    <Input
                      id={wards.length === 0 ? "rpt-ward" : "rpt-ward-other"}
                      className="h-11 text-base"
                      placeholder="Your ward (optional)"
                      value={wards.length === 0 ? ward : wardOther}
                      onChange={(e) =>
                        wards.length === 0
                          ? setWard(e.target.value)
                          : setWardOther(e.target.value)
                      }
                    />
                  ) : null}
                </div>

                <div className="space-y-1.5">
                  <Label htmlFor="rpt-location">Location</Label>
                  <Input
                    id="rpt-location"
                    className="h-11 text-base"
                    placeholder="e.g. Junction of Broad St & Marina"
                    value={locationText}
                    onChange={(e) => setLocationText(e.target.value)}
                  />
                  <div className="flex items-center gap-2 pt-1">
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={useMyLocation}
                      disabled={locating}
                    >
                      {locating ? (
                        <Loader2 className="h-4 w-4 animate-spin" />
                      ) : (
                        <LocateFixed className="h-4 w-4" />
                      )}
                      {coords ? "Update my location" : "Use my location"}
                    </Button>
                    {coords ? (
                      <span className="font-mono text-xs text-muted-foreground">
                        {coords.lat.toFixed(4)}, {coords.lon.toFixed(4)}
                      </span>
                    ) : null}
                  </div>
                  {locNote ? (
                    <p className="text-xs text-muted-foreground">{locNote}</p>
                  ) : null}
                </div>

                <div className="space-y-3 border-t border-border pt-4">
                  <div className="space-y-1.5">
                    <Label htmlFor="rpt-name">Your name (optional)</Label>
                    <Input
                      id="rpt-name"
                      className="h-11 text-base"
                      autoComplete="name"
                      value={name}
                      onChange={(e) => setName(e.target.value)}
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="rpt-phone">Phone number (optional)</Label>
                    <Input
                      id="rpt-phone"
                      className="h-11 text-base"
                      type="tel"
                      autoComplete="tel"
                      placeholder="e.g. +2348012345678"
                      value={phone}
                      onChange={(e) => setPhone(e.target.value)}
                    />
                    <p className="text-xs text-muted-foreground">
                      Needed to get status updates and to track your report
                      later.
                    </p>
                  </div>
                  <label className="flex items-start gap-2 text-sm">
                    <input
                      type="checkbox"
                      className="mt-0.5 cursor-pointer accent-[#7c5b3e]"
                      checked={wantsUpdates}
                      onChange={(e) => setWantsUpdates(e.target.checked)}
                    />
                    Send me status updates
                    {wantsUpdates && phoneTrim === "" ? (
                      <span className="text-[#C0562F]">(needs a phone number)</span>
                    ) : null}
                  </label>
                  <label className="flex items-start gap-2 text-sm">
                    <input
                      type="checkbox"
                      className="mt-0.5 cursor-pointer accent-[#7c5b3e]"
                      checked={anonymous}
                      onChange={(e) => setAnonymous(e.target.checked)}
                    />
                    Report anonymously — hide my identity from the public list
                    views
                  </label>
                </div>

                {/* Honeypot: invisible to humans, named `website` (SPEC §3 WS-A). */}
                <div
                  className="absolute left-[-9999px] top-[-9999px] h-0 w-0 overflow-hidden"
                  aria-hidden="true"
                >
                  <label>
                    Website
                    <input
                      type="text"
                      name="website"
                      tabIndex={-1}
                      autoComplete="off"
                      value={website}
                      onChange={(e) => setWebsite(e.target.value)}
                    />
                  </label>
                </div>

                {error ? (
                  <p className="rounded-md border border-[#C0562F]/40 bg-[#C0562F]/10 px-3 py-2 text-sm text-[#C0562F]">
                    {error}
                  </p>
                ) : null}

                <Button
                  size="lg"
                  className="w-full"
                  onClick={() => void submit()}
                  disabled={
                    submitting || !category || !descValid || updatesNeedPhone
                  }
                >
                  {submitting ? (
                    <Loader2 className="h-4 w-4 animate-spin" />
                  ) : null}
                  {submitting ? "Sending…" : "Submit report"}
                </Button>
                <p className="text-center text-xs text-muted-foreground">
                  Already reported?{" "}
                  <Link
                    href={`/p/${site.site_slug}/track`}
                    className="underline underline-offset-2"
                  >
                    Track your report
                  </Link>
                </p>
              </>
            )}
          </div>
        )}
      </main>

      <footer className="border-t border-border py-6 text-center text-xs text-muted-foreground">
        {brandName} · Powered by Agora
      </footer>
    </div>
  );
}
