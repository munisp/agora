"use client";

/**
 * SPEC-W32 WS-C: PUBLIC case tracking — no auth, possession-based lookup.
 *
 *   GET /api/civic/public/tenants/{slug}/reports/{ref}?phone=
 *     → {ref, category, status, ward, created_at, acked_at, resolved_at,
 *        mda_queue} (+ merged_into pointer on merged cases — SPEC §4 gate 3)
 *
 * ref + phone must both match, so a guessed reference alone reveals nothing.
 * A mismatch 404s — rendered as a friendly not-found, never an error wall.
 * Due dates (ack_due_at / resolve_due_at) are shown when the backend
 * includes them; the timeline itself always renders from the lifecycle
 * timestamps.
 */
import * as React from "react";
import Link from "next/link";
import { CheckCircle2, FileWarning, Loader2, Search } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input, Label } from "@/components/ui/input";
import { cn, formatDateTime } from "@/lib/utils";
import type { PublicSite } from "@/lib/types";

interface TrackResult {
  ref: string;
  category: string | null;
  status: string;
  ward: string | null;
  created_at: string | null;
  acked_at: string | null;
  resolved_at: string | null;
  closed_at: string | null;
  ack_due_at: string | null;
  resolve_due_at: string | null;
  mda_queue: string | null;
  merged_into: string | null;
}

function str(v: unknown): string | null {
  return typeof v === "string" && v !== "" ? v : null;
}

function normalizeTrack(data: unknown): TrackResult {
  const o = (typeof data === "object" && data !== null ? data : {}) as Record<
    string,
    unknown
  >;
  return {
    ref: str(o.ref) ?? "",
    category: str(o.category) ?? str(o.category_name),
    status: str(o.status) ?? "new",
    ward: str(o.ward),
    created_at: str(o.created_at),
    acked_at: str(o.acked_at),
    resolved_at: str(o.resolved_at),
    closed_at: str(o.closed_at),
    ack_due_at: str(o.ack_due_at),
    resolve_due_at: str(o.resolve_due_at),
    mda_queue: str(o.mda_queue),
    merged_into: str(o.merged_into),
  };
}

const STEPS = [
  { key: "new", label: "Received" },
  { key: "triaged", label: "Triaged" },
  { key: "assigned", label: "Assigned" },
  { key: "in_progress", label: "In progress" },
  { key: "resolved", label: "Resolved" },
] as const;

function stepIndex(status: string): number {
  if (status === "closed") return STEPS.length; // all steps done + closed
  const i = STEPS.findIndex((s) => s.key === status);
  return i === -1 ? 0 : i;
}

/** Step timestamp, when the backend surfaces one. */
function stepTimestamp(r: TrackResult, key: string): string | null {
  switch (key) {
    case "new":
      return r.created_at;
    case "triaged":
      return r.acked_at;
    case "resolved":
      return r.resolved_at;
    default:
      return null;
  }
}

export function PublicTrackClient({ site }: { site: PublicSite }) {
  const brandName =
    site.theme?.brandName ?? site.theme?.brand_name ?? site.business_name;

  const [ref, setRef] = React.useState("");
  const [phone, setPhone] = React.useState("");
  const [loading, setLoading] = React.useState(false);
  const [notFound, setNotFound] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [result, setResult] = React.useState<TrackResult | null>(null);

  const lookup = async () => {
    if (!ref.trim() || !phone.trim()) return;
    setLoading(true);
    setNotFound(false);
    setError(null);
    setResult(null);
    try {
      const data = await api.get<unknown>(
        `/api/civic/public/tenants/${site.site_slug}/reports/${encodeURIComponent(ref.trim())}`,
        { phone: phone.trim() },
      );
      setResult(normalizeTrack(data));
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) {
        setNotFound(true);
      } else {
        setError(
          e instanceof ApiError
            ? e.message
            : "Tracking is unavailable right now — please try again.",
        );
      }
    } finally {
      setLoading(false);
    }
  };

  const current = result ? stepIndex(result.status) : 0;

  return (
    <div className="min-h-screen bg-background">
      <header className="border-b border-border bg-card">
        <div className="mx-auto max-w-xl px-5 py-6">
          <h1 className="text-2xl font-bold tracking-tight">{brandName}</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Track a report with your reference number and phone.
          </p>
        </div>
      </header>

      <main className="mx-auto max-w-xl space-y-5 px-5 py-6">
        <div className="space-y-4 rounded-lg border border-border bg-card px-5 py-6 shadow-sm">
          <div className="space-y-1.5">
            <Label htmlFor="trk-ref">Reference number</Label>
            <Input
              id="trk-ref"
              className="h-11 font-mono text-base"
              placeholder="e.g. GOV-IKEJA-03-2026-000123"
              value={ref}
              onChange={(e) => setRef(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void lookup();
              }}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="trk-phone">Phone number used in the report</Label>
            <Input
              id="trk-phone"
              className="h-11 text-base"
              type="tel"
              placeholder="e.g. +2348012345678"
              value={phone}
              onChange={(e) => setPhone(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void lookup();
              }}
            />
          </div>
          <Button
            size="lg"
            className="w-full"
            onClick={() => void lookup()}
            disabled={loading || !ref.trim() || !phone.trim()}
          >
            {loading ? (
              <Loader2 className="h-4 w-4 animate-spin" />
            ) : (
              <Search className="h-4 w-4" />
            )}
            {loading ? "Looking up…" : "Track report"}
          </Button>
        </div>

        {notFound ? (
          <div className="flex items-start gap-3 rounded-lg border border-border bg-card px-5 py-6 shadow-sm">
            <FileWarning className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground" />
            <div className="text-sm">
              <p className="font-medium">We couldn&rsquo;t find that report.</p>
              <p className="mt-1 text-muted-foreground">
                Check the reference number and the phone number you used when
                reporting — both must match exactly. If you reported without a
                phone number, tracking isn&rsquo;t available for that report.
              </p>
              <Link
                href={`/p/${site.site_slug}/report`}
                className="mt-2 inline-block underline underline-offset-2"
              >
                File a new report
              </Link>
            </div>
          </div>
        ) : null}

        {error ? (
          <p className="rounded-md border border-[#C0562F]/40 bg-[#C0562F]/10 px-3 py-2 text-sm text-[#C0562F]">
            {error}
          </p>
        ) : null}

        {result ? (
          <div className="space-y-5 rounded-lg border border-border bg-card px-5 py-6 shadow-sm">
            <div>
              <p className="break-all font-mono text-lg font-semibold">
                {result.ref}
              </p>
              <p className="mt-1 text-sm text-muted-foreground">
                {result.category ? `${result.category} · ` : ""}
                {result.ward ? `Ward: ${result.ward} · ` : ""}
                {result.created_at
                  ? `Reported ${formatDateTime(result.created_at)}`
                  : ""}
              </p>
              {result.merged_into ? (
                <p className="mt-2 rounded-md border border-[#D99A4E]/50 bg-[#D99A4E]/15 px-3 py-2 text-xs text-[#a8762f]">
                  This report was merged with a similar one and is being handled
                  together under{" "}
                  <span className="font-mono font-medium">
                    {result.merged_into}
                  </span>
                  .
                </p>
              ) : null}
            </div>

            <ol className="space-y-0">
              {STEPS.map((step, i) => {
                const done = i < current || result.status === "closed";
                const active = i === current && result.status !== "closed";
                const at = stepTimestamp(result, step.key);
                const due =
                  step.key === "triaged"
                    ? result.ack_due_at
                    : step.key === "resolved"
                      ? result.resolve_due_at
                      : null;
                return (
                  <li key={step.key} className="flex gap-3">
                    <div className="flex flex-col items-center">
                      <span
                        className={cn(
                          "flex h-7 w-7 items-center justify-center rounded-full border-2",
                          done
                            ? "border-[#7A8B6F] bg-[#7A8B6F] text-[#FAF6F0]"
                            : active
                              ? "border-[#D99A4E] bg-[#D99A4E]/15 text-[#a8762f]"
                              : "border-border bg-muted text-muted-foreground",
                        )}
                        aria-hidden
                      >
                        {done ? (
                          <CheckCircle2 className="h-4 w-4" />
                        ) : (
                          <span className="text-xs font-semibold">{i + 1}</span>
                        )}
                      </span>
                      {i < STEPS.length - 1 ? (
                        <span
                          className={cn(
                            "w-0.5 flex-1",
                            done ? "bg-[#7A8B6F]" : "bg-border",
                          )}
                          aria-hidden
                        />
                      ) : null}
                    </div>
                    <div className="pb-6">
                      <p
                        className={cn(
                          "text-sm font-medium",
                          !done && !active && "text-muted-foreground",
                        )}
                      >
                        {step.label}
                        {active ? (
                          <span className="ml-2 text-xs font-normal text-[#a8762f]">
                            current
                          </span>
                        ) : null}
                      </p>
                      {at ? (
                        <p className="text-xs text-muted-foreground">
                          {formatDateTime(at)}
                        </p>
                      ) : null}
                      {!done && due ? (
                        <p className="text-xs text-muted-foreground">
                          Due by {formatDateTime(due)}
                        </p>
                      ) : null}
                    </div>
                  </li>
                );
              })}
            </ol>

            {result.status === "closed" && result.closed_at ? (
              <p className="text-sm text-muted-foreground">
                Case closed {formatDateTime(result.closed_at)}. Thank you for
                helping improve your community.
              </p>
            ) : null}

            <p className="text-center text-xs text-muted-foreground">
              Something else to report?{" "}
              <Link
                href={`/p/${site.site_slug}/report`}
                className="underline underline-offset-2"
              >
                File a new report
              </Link>
            </p>
          </div>
        ) : null}
      </main>

      <footer className="border-t border-border py-6 text-center text-xs text-muted-foreground">
        {brandName} · Powered by Agora
      </footer>
    </div>
  );
}
