"use client";

import * as React from "react";
import {
  CalendarCheck,
  MessageSquare,
  PhoneCall,
  SmilePlus,
  Wallet,
} from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { ErrorNote } from "@/components/error-note";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { cn, formatMoney } from "@/lib/utils";
import type {
  Booking,
  Conversation,
  ConversationWithTurns,
  Offering,
  Tenant,
} from "@/lib/types";

/**
 * Role-based KPI dashboard (SPEC-W7 Part C). Data is assembled from the
 * existing platform endpoints — no new backend surface:
 *   - bookings:      GET /api/bookings/v1/bookings?from=&to=   (tenant-scoped)
 *   - revenue:       bookings × GET /api/bookings/v1/offerings prices
 *   - calls/channel: GET /api/conversations/v1/conversations (tenant_id uuid)
 *   - sentiment:     GET /api/conversations/v1/conversations/{id} turns
 * Charts are hand-rolled SVG — the repo has no charting dependency and none
 * is added.
 */

const PERIODS = [
  { days: 7, label: "7d" },
  { days: 30, label: "30d" },
  { days: 90, label: "90d" },
];

/** Omnichannel buckets shown in the channel breakdown card. */
const CHANNELS = ["whatsapp", "telegram", "web", "voice"] as const;
type ChannelBucket = (typeof CHANNELS)[number];

/** Map a conversation channel or booking source onto a display bucket. */
function channelBucket(raw: string | undefined | null): ChannelBucket {
  switch ((raw ?? "").toLowerCase()) {
    case "whatsapp":
      return "whatsapp";
    case "telegram":
      return "telegram";
    case "voice":
    case "phone":
      return "voice";
    // web chat widget, admin-created and API bookings all arrive via web.
    default:
      return "web";
  }
}

interface KpiData {
  bookings: number;
  revenueCents: number;
  currency: string;
  callMinutes: number;
  /** average turn sentiment in [-1, 1]; null when nothing was scored */
  avgSentiment: number | null;
  /** number of conversations the sentiment sample covers (capped, see load) */
  sentimentSamples: number;
  channelCounts: Record<ChannelBucket, number>;
  /** bookings per day, oldest → newest, aligned with `dayLabels` */
  dailyCounts: number[];
  dayLabels: string[];
}

export function KpiDashboard({ orgSlug }: { orgSlug: string }) {
  const [days, setDays] = React.useState(30);
  const [data, setData] = React.useState<KpiData | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  /** soft-failure notes for optional sources (conversations/sentiment) */
  const [notes, setNotes] = React.useState<string[]>([]);

  const load = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    setNotes([]);
    const to = new Date();
    const from = new Date(to.getTime() - days * 24 * 60 * 60 * 1000);
    const softNotes: string[] = [];
    try {
      // 1) tenant (uuid + currency) — required for the conversation API.
      const tenant = await api.get<Tenant>(
        `/api/identity/v1/tenants/${orgSlug}`,
      );

      // 2) bookings in period + offerings (for per-booking revenue).
      const [bookingsRaw, offeringsRaw] = await Promise.all([
        api.get<Booking[] | { items: Booking[] }>(
          "/api/bookings/v1/bookings",
          {
            tenant: orgSlug,
            from: from.toISOString(),
            to: to.toISOString(),
            // W44/F15-10: booking-service honors an explicit limit up to 500
            // (default 100 would silently truncate the 90d view).
            limit: 500,
          },
        ),
        api.get<Offering[] | { items: Offering[] }>(
          "/api/bookings/v1/offerings",
          { tenant: orgSlug },
        ),
      ]);
      const bookings = Array.isArray(bookingsRaw)
        ? bookingsRaw
        : (bookingsRaw.items ?? []);
      const offerings = Array.isArray(offeringsRaw)
        ? offeringsRaw
        : (offeringsRaw.items ?? []);
      const priceByOffering = new Map(
        offerings.map((o) => [o.id, o.price_cents]),
      );

      const active = bookings.filter(
        (b) => b.status !== "cancelled" && b.status !== "no_show",
      );
      const revenueCents = active.reduce(
        (sum, b) => sum + (priceByOffering.get(b.offering_id) ?? 0),
        0,
      );

      // Daily booking counts (oldest → newest).
      const dailyCounts: number[] = [];
      const dayLabels: string[] = [];
      for (let i = days - 1; i >= 0; i--) {
        const dayStart = new Date(to.getTime() - i * 24 * 60 * 60 * 1000);
        const key = dayStart.toISOString().slice(0, 10);
        dayLabels.push(key);
        dailyCounts.push(
          bookings.filter((b) => b.starts_at.slice(0, 10) === key).length,
        );
      }

      // 3) conversations → call minutes + channel breakdown + sentiment.
      const channelCounts: Record<ChannelBucket, number> = {
        whatsapp: 0,
        telegram: 0,
        web: 0,
        voice: 0,
      };
      let callMinutes = 0;
      let avgSentiment: number | null = null;
      let sentimentSamples = 0;
      try {
        const convRes = await api.get<{
          conversations?: Conversation[];
          items?: Conversation[];
        }>("/api/conversations/v1/conversations", {
          tenant_id: tenant.id,
          limit: 200,
        });
        const conversations = convRes.conversations ?? convRes.items ?? [];
        const inPeriod = conversations.filter(
          (c) => new Date(c.started_at) >= from,
        );
        for (const c of inPeriod) {
          channelCounts[channelBucket(c.channel)] += 1;
          if (
            c.ended_at &&
            (channelBucket(c.channel) === "voice" ||
              c.channel === "phone")
          ) {
            callMinutes +=
              (new Date(c.ended_at).getTime() -
                new Date(c.started_at).getTime()) /
              60000;
          }
        }
        // Sentiment: average scored turns across a capped sample of the
        // most recent in-period conversations (detail fetch per conv).
        const sample = inPeriod.slice(0, 8);
        sentimentSamples = sample.length;
        const scores: number[] = [];
        for (const c of sample) {
          try {
            const detail = await api.get<ConversationWithTurns>(
              `/api/conversations/v1/conversations/${c.id}`,
              { tenant_id: tenant.id },
            );
            for (const t of detail.turns ?? []) {
              if (typeof t.sentiment === "number") scores.push(t.sentiment);
            }
          } catch {
            // single conversation unreadable — skip it
          }
        }
        if (scores.length > 0) {
          avgSentiment =
            scores.reduce((a, b) => a + b, 0) / scores.length;
        }
      } catch (e) {
        softNotes.push(
          e instanceof ApiError
            ? `Conversation metrics unavailable: ${e.message}`
            : "Conversation metrics unavailable.",
        );
      }

      setData({
        bookings: bookings.length,
        revenueCents,
        currency: tenant.currency ?? "USD",
        callMinutes,
        avgSentiment,
        sentimentSamples,
        channelCounts,
        dailyCounts,
        dayLabels,
      });
      setNotes(softNotes);
    } catch (e) {
      setError(
        e instanceof ApiError ? e.message : "Failed to load KPI data.",
      );
      setData(null);
    } finally {
      setLoading(false);
    }
  }, [orgSlug, days]);

  React.useEffect(() => {
    void load();
  }, [load]);

  const totalChannel = data
    ? CHANNELS.reduce((sum, c) => sum + data.channelCounts[c], 0)
    : 0;

  return (
    <div className="mb-8">
      <div className="mb-4 flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">Key performance indicators</h2>
          <p className="text-sm text-muted-foreground">
            Operational KPIs for this organisation — read-only.
          </p>
        </div>
        <div className="flex gap-1 rounded-md border border-border p-0.5">
          {PERIODS.map((p) => (
            <button
              key={p.days}
              type="button"
              onClick={() => setDays(p.days)}
              className={cn(
                "rounded px-2.5 py-1 text-xs font-medium cursor-pointer",
                days === p.days
                  ? "bg-secondary text-secondary-foreground"
                  : "text-muted-foreground hover:text-foreground",
              )}
            >
              {p.label}
            </button>
          ))}
        </div>
      </div>

      {error ? <ErrorNote message={error} /> : null}
      {notes.map((n) => (
        <p key={n} className="mb-2 text-xs text-muted-foreground">
          {n}
        </p>
      ))}

      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <KpiCard
          icon={<CalendarCheck className="h-4 w-4 text-muted-foreground" />}
          label={`Bookings · last ${days}d`}
          value={loading || !data ? "—" : String(data.bookings)}
          hint="Excludes nothing — all statuses"
        />
        <KpiCard
          icon={<Wallet className="h-4 w-4 text-muted-foreground" />}
          label="Booked value (list price)"
          value={
            loading || !data
              ? "—"
              : formatMoney(data.revenueCents, data.currency)
          }
          hint="Active bookings × offering price"
        />
        <KpiCard
          icon={<PhoneCall className="h-4 w-4 text-muted-foreground" />}
          label="Call minutes"
          value={loading || !data ? "—" : String(Math.round(data.callMinutes))}
          hint="Voice conversations in period"
        />
        <KpiCard
          icon={<SmilePlus className="h-4 w-4 text-muted-foreground" />}
          label={`Avg sentiment (n=${data?.sentimentSamples ?? 0})`}
          value={
            loading || !data
              ? "—"
              : data.avgSentiment === null
                ? "—"
                : sentimentLabel(data.avgSentiment)
          }
          hint="Lexicon score across recent turns"
        />
      </div>

      <div className="mt-4 grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <MessageSquare className="h-4 w-4" /> Channel breakdown
            </CardTitle>
            <CardDescription>
              Conversations per channel in the selected period.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {loading || !data ? (
              <p className="text-sm text-muted-foreground">Loading…</p>
            ) : totalChannel === 0 ? (
              <p className="text-sm text-muted-foreground">
                No conversations in this period.
              </p>
            ) : (
              <div className="space-y-3">
                {CHANNELS.map((c) => {
                  const count = data.channelCounts[c];
                  const pct =
                    totalChannel > 0
                      ? Math.round((count / totalChannel) * 100)
                      : 0;
                  return (
                    <div key={c}>
                      <div className="mb-1 flex items-center justify-between text-xs">
                        <span className="font-medium capitalize">{c}</span>
                        <span className="text-muted-foreground">
                          {count} · {pct}%
                        </span>
                      </div>
                      <div className="h-2 overflow-hidden rounded-full bg-muted">
                        <div
                          className="h-full rounded-full bg-primary transition-all"
                          style={{ width: `${pct}%` }}
                        />
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Bookings trend</CardTitle>
            <CardDescription>
              Bookings per day over the last {days} days.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {loading || !data ? (
              <p className="text-sm text-muted-foreground">Loading…</p>
            ) : (
              <DailyBars counts={data.dailyCounts} labels={data.dayLabels} />
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}

function KpiCard({
  icon,
  label,
  value,
  hint,
}: {
  icon: React.ReactNode;
  label: string;
  value: string;
  hint: string;
}) {
  return (
    <Card>
      <CardHeader className="flex-row items-center justify-between space-y-0 pb-2">
        <p className="text-sm font-medium text-muted-foreground">{label}</p>
        {icon}
      </CardHeader>
      <CardContent>
        <p className="text-2xl font-bold">{value}</p>
        <p className="text-xs text-muted-foreground">{hint}</p>
      </CardContent>
    </Card>
  );
}

function sentimentLabel(score: number): string {
  const word =
    score >= 0.2 ? "Positive" : score <= -0.2 ? "Negative" : "Neutral";
  return `${word} (${score >= 0 ? "+" : ""}${score.toFixed(2)})`;
}

/** Pure-SVG daily bar chart with a sparkline overlay. No chart dependency. */
function DailyBars({ counts, labels }: { counts: number[]; labels: string[] }) {
  const W = 640;
  const H = 160;
  const PAD = 8;
  const max = Math.max(1, ...counts);
  const n = Math.max(1, counts.length);
  const slot = (W - PAD * 2) / n;
  const barW = Math.max(2, Math.min(24, slot * 0.7));
  const points = counts
    .map((c, i) => {
      const x = PAD + slot * i + slot / 2;
      const y = H - PAD - (c / max) * (H - PAD * 2);
      return `${x},${y}`;
    })
    .join(" ");

  return (
    <div>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        className="h-40 w-full"
        role="img"
        aria-label="Bookings per day"
      >
        {counts.map((c, i) => {
          const x = PAD + slot * i + (slot - barW) / 2;
          const h = (c / max) * (H - PAD * 2);
          return (
            <rect
              key={labels[i]}
              x={x}
              y={H - PAD - h}
              width={barW}
              height={Math.max(h, c > 0 ? 2 : 0)}
              rx={1.5}
              className="fill-primary/70"
            >
              <title>{`${labels[i]}: ${c} booking${c === 1 ? "" : "s"}`}</title>
            </rect>
          );
        })}
        <polyline
          points={points}
          fill="none"
          className="stroke-foreground/60"
          strokeWidth={1.5}
        />
      </svg>
      <div className="mt-1 flex justify-between text-[10px] text-muted-foreground">
        <span>{labels[0]}</span>
        <span>{labels[labels.length - 1]}</span>
      </div>
    </div>
  );
}
