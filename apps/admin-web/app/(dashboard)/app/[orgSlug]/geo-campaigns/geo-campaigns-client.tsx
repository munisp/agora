"use client";

import * as React from "react";
import dynamic from "next/dynamic";
import { Megaphone, RefreshCw, Send, Users } from "lucide-react";
import type { Feature, Polygon } from "geojson";
import { api, ApiError } from "@/lib/api";
import { circlePolygon, formatRadius } from "@/lib/geo";
import { formatDateTime } from "@/lib/utils";
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
import { Input, Label, Select, Textarea } from "@/components/ui/input";
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
import type {
  GeoAudiencePreview,
  GeoCampaign,
  ListResponse,
} from "@/lib/types";

// WebGL must never run during SSR — the map is client-only.
const TargetMap = dynamic(() => import("./target-map"), {
  ssr: false,
  loading: () => (
    <div className="flex h-[420px] w-full items-center justify-center rounded-md border border-border bg-muted text-sm text-muted-foreground">
      Loading map…
    </div>
  ),
});

const MESSAGE_MAX = 1000;
const RADIUS_MIN = 100;
const RADIUS_MAX = 20000;

type TargetPayload =
  | { polygon: Polygon }
  | { center: { lat: number; lng: number }; radius_m: number };

/**
 * Channel availability mirrors the Channels page config (SPEC-W6 §C stores
 * toggles in localStorage per tenant). WhatsApp/Telegram are disabled only
 * when a saved config explicitly turns them off; SMS has no local config and
 * is always offered.
 */
function loadChannelFlags(orgSlug: string): {
  whatsapp: boolean;
  telegram: boolean;
} {
  try {
    const raw = window.localStorage.getItem(`opendesk:channels:${orgSlug}`);
    if (!raw) return { whatsapp: true, telegram: true };
    const parsed = JSON.parse(raw) as {
      whatsapp?: { enabled?: boolean };
      telegram?: { enabled?: boolean };
    };
    return {
      whatsapp: parsed.whatsapp?.enabled !== false,
      telegram: parsed.telegram?.enabled !== false,
    };
  } catch {
    return { whatsapp: true, telegram: true };
  }
}

function StatusBadge({ status }: { status: string }) {
  switch (status) {
    case "running":
      return <Badge variant="info">Running</Badge>;
    case "completed":
      return <Badge variant="success">Completed</Badge>;
    case "failed":
      return <Badge variant="destructive">Failed</Badge>;
    case "draft":
      return <Badge variant="secondary">Draft</Badge>;
    default:
      return <Badge variant="secondary">{status}</Badge>;
  }
}

export function GeoCampaignsClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();

  // ---- target area ----
  const [mode, setMode] = React.useState<"polygon" | "circle">("circle");
  const [polygon, setPolygon] = React.useState<Polygon | null>(null);
  const [center, setCenter] = React.useState<{ lat: number; lng: number } | null>(
    null,
  );
  const [radiusM, setRadiusM] = React.useState(2000);

  // ---- live audience preview ----
  const [preview, setPreview] = React.useState<GeoAudiencePreview | null>(null);
  const [previewLoading, setPreviewLoading] = React.useState(false);
  const [previewError, setPreviewError] = React.useState<string | null>(null);

  // ---- composer ----
  const [channelFlags, setChannelFlags] = React.useState({
    whatsapp: true,
    telegram: true,
  });
  const [name, setName] = React.useState("");
  const [channel, setChannel] = React.useState("whatsapp");
  const [message, setMessage] = React.useState("");
  const [launching, setLaunching] = React.useState(false);

  // ---- campaign list ----
  const [campaigns, setCampaigns] = React.useState<GeoCampaign[]>([]);
  const [listLoading, setListLoading] = React.useState(true);
  const [listError, setListError] = React.useState<string | null>(null);

  React.useEffect(() => {
    setChannelFlags(loadChannelFlags(orgSlug));
  }, [orgSlug]);

  // Effective payload shared by the preview and the launch call.
  const payload: TargetPayload | null =
    mode === "polygon"
      ? polygon
        ? { polygon }
        : null
      : center
        ? { center, radius_m: radiusM }
        : null;

  const targetFeature: Feature<Polygon> | null =
    mode === "polygon"
      ? polygon
        ? { type: "Feature", properties: {}, geometry: polygon }
        : null
      : center
        ? circlePolygon(center, radiusM)
        : null;

  const payloadKey = JSON.stringify(payload);

  // Debounced live audience preview (POST /v1/geo/audience/preview). A
  // sequence number discards stale responses when the target changes
  // mid-flight (the api helper has no AbortSignal for POST).
  const previewSeq = React.useRef(0);
  React.useEffect(() => {
    if (!payload) {
      setPreview(null);
      setPreviewError(null);
      setPreviewLoading(false);
      return;
    }
    const seq = ++previewSeq.current;
    setPreviewLoading(true);
    const timer = setTimeout(() => {
      (async () => {
        try {
          const data = await api.post<GeoAudiencePreview>(
            "/api/bookings/v1/geo/audience/preview",
            payload,
            { tenant: orgSlug },
          );
          if (seq !== previewSeq.current) return;
          setPreview(data);
          setPreviewError(null);
        } catch (e) {
          if (seq !== previewSeq.current) return;
          setPreview(null);
          setPreviewError(
            e instanceof ApiError
              ? e.message
              : "Audience preview failed — the booking service may be offline.",
          );
        } finally {
          if (seq === previewSeq.current) setPreviewLoading(false);
        }
      })();
    }, 500);
    return () => clearTimeout(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [payloadKey, orgSlug]);

  const loadCampaigns = React.useCallback(
    async (silent = false) => {
      if (!silent) setListLoading(true);
      try {
        const data = await api.get<GeoCampaign[] | ListResponse<GeoCampaign>>(
          "/api/bookings/v1/geo/campaigns",
          { tenant: orgSlug },
        );
        const items = Array.isArray(data) ? data : (data.items ?? []);
        setCampaigns(
          items
            .slice()
            .sort((a, b) => b.created_at.localeCompare(a.created_at)),
        );
        setListError(null);
      } catch (e) {
        setListError(
          e instanceof ApiError
            ? e.message
            : "Failed to load campaigns — the booking service may be offline.",
        );
      } finally {
        setListLoading(false);
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    void loadCampaigns();
  }, [loadCampaigns]);

  // Poll while any campaign is still running.
  // SPEC-W46 AW-6: 4s → 10s — progress moves at send-batch cadence, so the
  // tighter interval only generated background traffic.
  const hasRunning = campaigns.some((c) => c.status === "running");
  React.useEffect(() => {
    if (!hasRunning) return;
    const timer = setInterval(() => void loadCampaigns(true), 10000);
    return () => clearInterval(timer);
  }, [hasRunning, loadCampaigns]);

  const switchMode = (next: "polygon" | "circle") => {
    setMode(next);
    // Clear the inactive target so the preview/launch never mix shapes.
    if (next === "polygon") setCenter(null);
    else setPolygon(null);
  };

  const canLaunch =
    !launching && payload !== null && name.trim() !== "" && message.trim() !== "";

  const launch = async () => {
    if (!canLaunch || !payload) return;
    setLaunching(true);
    try {
      await api.post(
        "/api/bookings/v1/geo/campaigns",
        {
          name: name.trim(),
          channel,
          message: message.trim(),
          target: payload,
        },
        { tenant: orgSlug },
      );
      toast({
        title: "Campaign launched",
        description: "Messages are being sent at a paced rate.",
        variant: "success",
      });
      setName("");
      setMessage("");
      await loadCampaigns(true);
    } catch (e) {
      toast({
        title: "Launch failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setLaunching(false);
    }
  };

  return (
    <div className="max-w-6xl">
      <PageHeader
        title="Geo campaigns"
        description="Target customers by geography — draw a polygon or drop a radius circle, preview the audience live, then launch a paced message blast."
        actions={
          hasRunning ? <Badge variant="info">Polling live status</Badge> : null
        }
      />

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Target area</CardTitle>
            <CardDescription>
              {mode === "circle"
                ? "Click the map to drop the centre, then tune the radius."
                : "Click to add vertices, double-click to finish the polygon."}
            </CardDescription>
          </CardHeader>
          <CardContent>
            <div className="mb-3 flex gap-2">
              <Button
                variant={mode === "circle" ? "secondary" : "outline"}
                size="sm"
                onClick={() => switchMode("circle")}
              >
                Circle (radius)
              </Button>
              <Button
                variant={mode === "polygon" ? "secondary" : "outline"}
                size="sm"
                onClick={() => switchMode("polygon")}
              >
                Draw polygon
              </Button>
            </div>

            <TargetMap
              mode={mode}
              center={center}
              target={targetFeature}
              onPolygon={setPolygon}
              onCenter={setCenter}
            />

            {mode === "circle" ? (
              <div className="mt-3 grid gap-1.5">
                <Label htmlFor="radius">Radius: {formatRadius(radiusM)}</Label>
                <Input
                  id="radius"
                  type="range"
                  min={RADIUS_MIN}
                  max={RADIUS_MAX}
                  step={100}
                  value={radiusM}
                  onChange={(e) => setRadiusM(Number(e.target.value))}
                  className="px-0 py-0 accent-primary"
                />
                <p className="text-xs text-muted-foreground">
                  {center
                    ? `Centre ${center.lat.toFixed(4)}, ${center.lng.toFixed(4)} — rendered as a 64-point polygon.`
                    : "Click the map to set the centre point."}
                </p>
              </div>
            ) : (
              <p className="mt-3 text-xs text-muted-foreground">
                {polygon
                  ? "Polygon captured — draw again to replace it."
                  : "No polygon drawn yet."}
              </p>
            )}

            <div className="mt-4 rounded-md border border-border bg-accent px-4 py-3">
              {payload === null ? (
                <p className="text-sm text-muted-foreground">
                  Define a target area to preview the audience.
                </p>
              ) : previewError ? (
                <p className="text-sm text-warning">{previewError}</p>
              ) : (
                <div className="flex items-start gap-3">
                  <Users className="mt-1 h-5 w-5 shrink-0 text-muted-foreground" />
                  <div className="min-w-0">
                    <p className="text-3xl font-semibold tracking-tight">
                      {previewLoading && !preview
                        ? "…"
                        : (preview?.count ?? 0).toLocaleString()}
                    </p>
                    <p className="text-xs text-muted-foreground">
                      reachable contacts in this area
                      {previewLoading ? " (updating…)" : ""}
                    </p>
                    {preview?.sample && preview.sample.length > 0 ? (
                      <p className="mt-2 text-xs text-muted-foreground">
                        Sample:{" "}
                        {preview.sample
                          .slice(0, 5)
                          .map((s) => s.phone_masked)
                          .join(" · ")}
                      </p>
                    ) : null}
                  </div>
                </div>
              )}
            </div>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Megaphone className="h-4 w-4" /> Message
            </CardTitle>
            <CardDescription>
              Sent at a paced rate through the notification pipeline and metered
              per recipient.
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-4">
            <div className="grid gap-1.5">
              <Label htmlFor="camp-name">Campaign name</Label>
              <Input
                id="camp-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="e.g. Weekend promo — downtown"
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="camp-channel">Channel</Label>
              <Select
                id="camp-channel"
                value={channel}
                onChange={(e) => setChannel(e.target.value)}
              >
                <option value="whatsapp" disabled={!channelFlags.whatsapp}>
                  WhatsApp
                  {channelFlags.whatsapp ? "" : " (disabled on Channels page)"}
                </option>
                <option value="telegram" disabled={!channelFlags.telegram}>
                  Telegram
                  {channelFlags.telegram ? "" : " (disabled on Channels page)"}
                </option>
                <option value="sms">SMS</option>
              </Select>
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="camp-message">Message</Label>
              <Textarea
                id="camp-message"
                value={message}
                onChange={(e) => setMessage(e.target.value)}
                maxLength={MESSAGE_MAX}
                rows={5}
                placeholder="Hi {name}, this weekend we're …"
              />
              <div className="flex items-center justify-between text-xs text-muted-foreground">
                <span>
                  Use {"{name}"} to personalise with the contact&apos;s name.
                </span>
                <span>
                  {message.length}/{MESSAGE_MAX}
                </span>
              </div>
            </div>
            <Button onClick={() => void launch()} disabled={!canLaunch}>
              <Send className="h-4 w-4" />
              {launching ? "Launching…" : "Launch campaign"}
            </Button>
            {payload === null ? (
              <p className="text-xs text-muted-foreground">
                A target area is required before launch.
              </p>
            ) : null}
          </CardContent>
        </Card>
      </div>

      <Card className="mt-4">
        <CardHeader className="flex-row items-center justify-between space-y-0">
          <div>
            <CardTitle>Campaigns</CardTitle>
            <CardDescription>
              Status refreshes automatically while a campaign is running.
            </CardDescription>
          </div>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void loadCampaigns()}
            disabled={listLoading}
          >
            <RefreshCw className="h-3.5 w-3.5" />
            {listLoading ? "Loading…" : "Refresh"}
          </Button>
        </CardHeader>
        <CardContent>
          {listError ? <ErrorNote message={listError} /> : null}
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Channel</TableHead>
                <TableHead className="text-right">Audience</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {campaigns.length === 0 ? (
                <TableEmpty colSpan={5}>
                  {listLoading
                    ? "Loading…"
                    : "No geo campaigns yet — define a target area and launch the first one."}
                </TableEmpty>
              ) : (
                campaigns.map((c) => (
                  <TableRow key={c.id}>
                    <TableCell className="font-medium">{c.name}</TableCell>
                    <TableCell className="capitalize">{c.channel}</TableCell>
                    <TableCell className="text-right">
                      {c.audience_count.toLocaleString()}
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={c.status} />
                    </TableCell>
                    <TableCell>{formatDateTime(c.created_at)}</TableCell>
                  </TableRow>
                ))
              )}
            </TableBody>
          </Table>
        </CardContent>
      </Card>
    </div>
  );
}
