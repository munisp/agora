"use client";

/**
 * SPEC-W32 WS-C: PUBLIC civic dashboard — aggregate-only stats, no auth.
 *
 *   GET /api/civic/public/tenants/{slug}/stats
 *     → aggregate-only: open/resolved counts by category and ward (no
 *       person data — SPEC §3 WS-A; §4 gate 1 forbids any phone leakage,
 *       so this page renders counts and durations only)
 *
 * Renders: open vs resolved totals, avg resolution days by category, and a
 * ward heat table (bars + cells, sage → amber → terracotta ramp — no map
 * library). A 404 means the civic module isn't live on this tenant yet:
 * clean empty state.
 */
import * as React from "react";
import Link from "next/link";
import { BarChart3, Loader2 } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { cn } from "@/lib/utils";
import type { PublicSite } from "@/lib/types";

const SAGE = "#7A8B6F";
const AMBER = "#D99A4E";
const TERRACOTTA = "#C0562F";

interface CategoryStat {
  category: string;
  open: number;
  resolved: number;
  avgResolutionDays: number | null;
}

interface WardStat {
  ward: string;
  open: number;
  resolved: number;
}

interface Stats {
  openTotal: number;
  resolvedTotal: number;
  byCategory: CategoryStat[];
  byWard: WardStat[];
}

function num(v: unknown): number {
  const n = typeof v === "string" ? Number(v) : v;
  return typeof n === "number" && Number.isFinite(n) ? n : 0;
}

function numOrNull(v: unknown): number | null {
  const n = typeof v === "string" ? Number(v) : v;
  return typeof n === "number" && Number.isFinite(n) ? n : null;
}

function str(v: unknown): string | null {
  return typeof v === "string" && v !== "" ? v : null;
}

/** Aggregate rows may be arrays of objects or maps keyed by name. */
function rows(v: unknown): Record<string, unknown>[] {
  if (Array.isArray(v)) {
    return v.map((x) =>
      typeof x === "object" && x !== null ? (x as Record<string, unknown>) : {},
    );
  }
  if (typeof v === "object" && v !== null) {
    return Object.entries(v).map(([key, x]) => ({
      key,
      ...(typeof x === "object" && x !== null
        ? (x as Record<string, unknown>)
        : { open: x }),
    }));
  }
  return [];
}

function normalizeStats(data: unknown): Stats {
  const o = (typeof data === "object" && data !== null ? data : {}) as Record<
    string,
    unknown
  >;
  const totals = (typeof o.totals === "object" && o.totals !== null
    ? o.totals
    : {}) as Record<string, unknown>;

  const byCategory = rows(o.by_category ?? o.categories).map((r) => ({
    category:
      str(r.category) ?? str(r.category_slug) ?? str(r.name) ?? str(r.key) ?? "Other",
    open: num(r.open ?? r.open_count),
    resolved: num(r.resolved ?? r.resolved_count ?? r.closed),
    avgResolutionDays: numOrNull(
      r.avg_resolution_days ?? r.avg_resolve_days ?? r.avg_days,
    ),
  }));

  const byWard = rows(o.by_ward ?? o.wards).map((r) => ({
    ward: str(r.ward) ?? str(r.name) ?? str(r.key) ?? "Unknown ward",
    open: num(r.open ?? r.open_count),
    resolved: num(r.resolved ?? r.resolved_count ?? r.closed),
  }));

  const openTotal =
    numOrNull(o.open ?? o.open_total ?? totals.open) ??
    byCategory.reduce((a, c) => a + c.open, 0) +
      byWard.reduce((a, w) => a + w.open, 0) * 0;
  const resolvedTotal =
    numOrNull(o.resolved ?? o.resolved_total ?? totals.resolved) ??
    byCategory.reduce((a, c) => a + c.resolved, 0) +
      byWard.reduce((a, w) => a + w.resolved, 0) * 0;

  return { openTotal, resolvedTotal, byCategory, byWard };
}

/** Heat colour on the sage → amber → terracotta ramp by 0..1 intensity. */
function heatColor(t: number): { bg: string; fg: string } {
  const clamped = Math.max(0, Math.min(1, t));
  if (clamped < 0.34) return { bg: `${SAGE}2e`, fg: SAGE };
  if (clamped < 0.67) return { bg: `${AMBER}33`, fg: "#a8762f" };
  return { bg: `${TERRACOTTA}26`, fg: TERRACOTTA };
}

export function PublicDashboardClient({ site }: { site: PublicSite }) {
  const brandName =
    site.theme?.brandName ?? site.theme?.brand_name ?? site.business_name;

  const [stats, setStats] = React.useState<Stats | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [unavailable, setUnavailable] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    let cancelled = false;
    api
      .get<unknown>(`/api/civic/public/tenants/${site.site_slug}/stats`)
      .then((data) => {
        if (!cancelled) setStats(normalizeStats(data));
      })
      .catch((e) => {
        if (cancelled) return;
        if (e instanceof ApiError && e.status === 404) setUnavailable(true);
        else setError("Statistics are unavailable right now — please try again later.");
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [site.site_slug]);

  const maxWardOpen = stats
    ? Math.max(1, ...stats.byWard.map((w) => w.open))
    : 1;
  const maxCatTotal = stats
    ? Math.max(1, ...stats.byCategory.map((c) => c.open + c.resolved))
    : 1;
  const grandTotal = stats ? stats.openTotal + stats.resolvedTotal : 0;

  return (
    <div className="min-h-screen bg-background">
      <header className="border-b border-border bg-card">
        <div className="mx-auto max-w-3xl px-5 py-6">
          <h1 className="text-2xl font-bold tracking-tight">{brandName}</h1>
          <p className="mt-1 text-sm text-muted-foreground">
            Civic service dashboard — how reports are being handled, in
            aggregate. No personal data is shown.
          </p>
        </div>
      </header>

      <main className="mx-auto max-w-3xl space-y-6 px-5 py-6">
        {loading ? (
          <p className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" /> Loading statistics…
          </p>
        ) : unavailable ? (
          <div className="flex items-start gap-3 rounded-lg border border-border bg-card px-5 py-8 shadow-sm">
            <BarChart3 className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground" />
            <div className="text-sm">
              <p className="font-medium">No statistics published yet.</p>
              <p className="mt-1 text-muted-foreground">
                Civic reporting has not been switched on for {brandName} yet.
                Check back soon — or{" "}
                <Link
                  href={`/p/${site.site_slug}/report`}
                  className="underline underline-offset-2"
                >
                  file a report
                </Link>
                .
              </p>
            </div>
          </div>
        ) : error ? (
          <p className="rounded-md border border-[#C0562F]/40 bg-[#C0562F]/10 px-3 py-2 text-sm text-[#C0562F]">
            {error}
          </p>
        ) : stats ? (
          <>
            {/* Open vs resolved */}
            <section className="rounded-lg border border-border bg-card px-5 py-5 shadow-sm">
              <h2 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">
                Open vs resolved
              </h2>
              <div className="mt-3 grid grid-cols-2 gap-4">
                <div>
                  <p className="text-3xl font-bold" style={{ color: AMBER }}>
                    {stats.openTotal}
                  </p>
                  <p className="text-xs text-muted-foreground">Open reports</p>
                </div>
                <div>
                  <p className="text-3xl font-bold" style={{ color: SAGE }}>
                    {stats.resolvedTotal}
                  </p>
                  <p className="text-xs text-muted-foreground">Resolved</p>
                </div>
              </div>
              {grandTotal > 0 ? (
                <div
                  className="mt-3 flex h-3 w-full overflow-hidden rounded-full bg-muted"
                  role="img"
                  aria-label={`${stats.resolvedTotal} of ${grandTotal} reports resolved`}
                >
                  <div
                    className="h-full"
                    style={{
                      width: `${(stats.resolvedTotal / grandTotal) * 100}%`,
                      backgroundColor: SAGE,
                    }}
                  />
                  <div className="h-full flex-1" style={{ backgroundColor: AMBER }} />
                </div>
              ) : (
                <p className="mt-3 text-xs text-muted-foreground">
                  No reports recorded yet.
                </p>
              )}
            </section>

            {/* By category */}
            <section className="rounded-lg border border-border bg-card px-5 py-5 shadow-sm">
              <h2 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">
                By category
              </h2>
              {stats.byCategory.length === 0 ? (
                <p className="mt-3 text-sm text-muted-foreground">
                  No category data yet.
                </p>
              ) : (
                <div className="mt-3 space-y-3">
                  {stats.byCategory.map((c) => {
                    const total = c.open + c.resolved;
                    return (
                      <div key={c.category}>
                        <div className="flex items-baseline justify-between text-sm">
                          <span className="font-medium">{c.category}</span>
                          <span className="text-xs text-muted-foreground">
                            {c.open} open · {c.resolved} resolved
                            {c.avgResolutionDays !== null
                              ? ` · avg ${c.avgResolutionDays.toFixed(1)} days`
                              : ""}
                          </span>
                        </div>
                        <div className="mt-1 flex h-2.5 w-full overflow-hidden rounded-full bg-muted">
                          {total > 0 ? (
                            <>
                              <div
                                className="h-full"
                                style={{
                                  width: `${(c.resolved / maxCatTotal) * 100}%`,
                                  backgroundColor: SAGE,
                                }}
                              />
                              <div
                                className="h-full"
                                style={{
                                  width: `${(c.open / maxCatTotal) * 100}%`,
                                  backgroundColor: AMBER,
                                }}
                              />
                            </>
                          ) : null}
                        </div>
                      </div>
                    );
                  })}
                </div>
              )}
            </section>

            {/* Ward heat table */}
            <section className="rounded-lg border border-border bg-card px-5 py-5 shadow-sm">
              <h2 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">
                Ward activity
              </h2>
              <p className="mt-1 text-xs text-muted-foreground">
                Darker cells mean more open reports — sage is quiet, amber is
                busy, terracotta needs attention.
              </p>
              {stats.byWard.length === 0 ? (
                <p className="mt-3 text-sm text-muted-foreground">
                  No ward data yet.
                </p>
              ) : (
                <div className="mt-3 overflow-x-auto">
                  <table className="w-full text-sm">
                    <thead>
                      <tr className="border-b border-border text-left text-xs uppercase tracking-wide text-muted-foreground">
                        <th className="py-2 pr-3 font-semibold">Ward</th>
                        <th className="py-2 pr-3 font-semibold">Open</th>
                        <th className="py-2 pr-3 font-semibold">Resolved</th>
                        <th className="py-2 font-semibold">Load</th>
                      </tr>
                    </thead>
                    <tbody>
                      {[...stats.byWard]
                        .sort((a, b) => b.open - a.open)
                        .map((w) => {
                          const t = w.open / maxWardOpen;
                          const heat = heatColor(t);
                          return (
                            <tr key={w.ward} className="border-b border-border/60">
                              <td className="py-2 pr-3 font-medium">{w.ward}</td>
                              <td className="py-2 pr-3">
                                <span
                                  className={cn(
                                    "inline-block min-w-8 rounded px-2 py-0.5 text-center text-xs font-semibold",
                                  )}
                                  style={{ backgroundColor: heat.bg, color: heat.fg }}
                                >
                                  {w.open}
                                </span>
                              </td>
                              <td className="py-2 pr-3 text-muted-foreground">
                                {w.resolved}
                              </td>
                              <td className="w-2/5 py-2">
                                <div className="h-2.5 w-full overflow-hidden rounded-full bg-muted">
                                  <div
                                    className="h-full rounded-full"
                                    style={{
                                      width: `${Math.max(t * 100, w.open > 0 ? 4 : 0)}%`,
                                      backgroundColor: heat.fg,
                                    }}
                                  />
                                </div>
                              </td>
                            </tr>
                          );
                        })}
                    </tbody>
                  </table>
                </div>
              )}
            </section>
          </>
        ) : null}

        <p className="text-center text-xs text-muted-foreground">
          <Link href={`/p/${site.site_slug}/report`} className="underline underline-offset-2">
            Report an issue
          </Link>{" "}
          ·{" "}
          <Link href={`/p/${site.site_slug}/track`} className="underline underline-offset-2">
            Track a report
          </Link>
        </p>
      </main>

      <footer className="border-t border-border py-6 text-center text-xs text-muted-foreground">
        {brandName} · Powered by Agora
      </footer>
    </div>
  );
}
