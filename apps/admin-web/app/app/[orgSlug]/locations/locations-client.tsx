"use client";

import * as React from "react";
import dynamic from "next/dynamic";
import {
  Layers,
  MapPinOff,
  PenLine,
  RefreshCw,
  Trash2,
  Upload,
} from "lucide-react";
import type { MultiPolygon, Polygon } from "geojson";
import { api, ApiError } from "@/lib/api";
import { serviceAreaToFeature } from "@/lib/geo";
import { addDays, formatDateTime, toISODate } from "@/lib/utils";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input, Label, Select } from "@/components/ui/input";
import { ConfirmDialog } from "@/components/ui/dialog";
import { useToast } from "@/components/ui/toast";
import { BoundaryImportDialog } from "./boundary-import";
import type {
  ListResponse,
  LocationCluster,
  LocationPoint,
  LocationsSummary,
  Offering,
  ServiceArea,
} from "@/lib/types";

// WebGL must never run during SSR — the map is client-only.
const LocationsMap = dynamic(() => import("./locations-map"), {
  ssr: false,
  loading: () => (
    <div className="flex h-[520px] w-full items-center justify-center rounded-md border border-border bg-muted text-sm text-muted-foreground">
      Loading map…
    </div>
  ),
});

function defaultFrom(): string {
  return toISODate(addDays(new Date(), -90));
}

export function LocationsClient({ orgSlug }: { orgSlug: string }) {
  const { toast } = useToast();

  // ---- filters ----
  const [from, setFrom] = React.useState(defaultFrom);
  const [to, setTo] = React.useState(() => toISODate(new Date()));
  const [offeringId, setOfferingId] = React.useState("");
  const [offerings, setOfferings] = React.useState<Offering[]>([]);

  // ---- locations summary ----
  const [points, setPoints] = React.useState<LocationPoint[]>([]);
  const [clusters, setClusters] = React.useState<LocationCluster[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [loaded, setLoaded] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  // ---- service areas ----
  const [areas, setAreas] = React.useState<ServiceArea[]>([]);
  const [areasError, setAreasError] = React.useState<string | null>(null);
  const [showAreas, setShowAreas] = React.useState(true);
  const [selectedAreaId, setSelectedAreaId] = React.useState<string | null>(null);
  const [deleteTarget, setDeleteTarget] = React.useState<ServiceArea | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [showImport, setShowImport] = React.useState(false);

  // ---- draw-new-area flow ----
  const [drawMode, setDrawMode] = React.useState(false);
  const [pendingGeometry, setPendingGeometry] = React.useState<
    Polygon | MultiPolygon | null
  >(null);
  const [areaName, setAreaName] = React.useState("");

  React.useEffect(() => {
    (async () => {
      try {
        const data = await api.get<Offering[] | ListResponse<Offering>>(
          "/api/bookings/v1/offerings",
          { tenant: orgSlug },
        );
        setOfferings(Array.isArray(data) ? data : (data.items ?? []));
      } catch {
        // The offering filter is optional — keep "All offerings" on failure.
      }
    })();
  }, [orgSlug]);

  const loadSummary = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<LocationsSummary>(
          "/api/bookings/v1/locations/summary",
          {
            tenant: orgSlug,
            from,
            to,
            offering_id: offeringId || undefined,
          },
          signal,
        );
        setPoints(data.points ?? []);
        setClusters(data.clusters ?? []);
        setLoaded(true);
      } catch (e) {
        if (e instanceof DOMException && e.name === "AbortError") return;
        setError(
          e instanceof ApiError
            ? e.message
            : "Failed to load locations — the booking service may be offline.",
        );
      } finally {
        setLoading(false);
      }
    },
    [orgSlug, from, to, offeringId],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    const timer = setTimeout(() => void loadSummary(controller.signal), 300);
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [loadSummary]);

  const loadAreas = React.useCallback(async () => {
    setAreasError(null);
    try {
      const data = await api.get<ServiceArea[] | ListResponse<ServiceArea>>(
        "/api/bookings/v1/service-areas",
        { tenant: orgSlug },
      );
      setAreas(Array.isArray(data) ? data : (data.items ?? []));
    } catch (e) {
      setAreasError(
        e instanceof ApiError
          ? e.message
          : "Failed to load service areas.",
      );
    }
  }, [orgSlug]);

  React.useEffect(() => {
    void loadAreas();
  }, [loadAreas]);

  const areaFeatures = React.useMemo(
    () =>
      areas
        .map(serviceAreaToFeature)
        .filter((f): f is NonNullable<typeof f> => f !== null),
    [areas],
  );

  const handleAreaDrawn = (geometry: Polygon | MultiPolygon) => {
    setDrawMode(false);
    setPendingGeometry(geometry);
    setAreaName("");
  };

  const saveArea = async () => {
    if (!pendingGeometry || !areaName.trim()) return;
    setBusy(true);
    try {
      await api.post(
        "/api/bookings/v1/service-areas",
        { name: areaName.trim(), geojson: pendingGeometry, meta: {} },
        { tenant: orgSlug },
      );
      toast({ title: "Service area saved", variant: "success" });
      setPendingGeometry(null);
      setAreaName("");
      setShowAreas(true);
      await loadAreas();
    } catch (e) {
      toast({
        title: "Could not save the service area",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const deleteArea = async () => {
    if (!deleteTarget) return;
    setBusy(true);
    try {
      await api.delete(`/api/bookings/v1/service-areas/${deleteTarget.id}`, {
        tenant: orgSlug,
      });
      toast({ title: "Service area deleted", variant: "success" });
      setDeleteTarget(null);
      if (selectedAreaId === deleteTarget.id) setSelectedAreaId(null);
      await loadAreas();
    } catch (e) {
      toast({
        title: "Could not delete the service area",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const selectedArea = areas.find((a) => a.id === selectedAreaId) ?? null;
  const empty =
    loaded && !loading && !error && points.length === 0 && clusters.length === 0;

  return (
    <div className="max-w-6xl">
      <PageHeader
        title="Locations"
        description="Where your bookings and contacts are — clustered on the map, filterable by date and offering, with service areas you can draw yourself."
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              onClick={() => setShowImport(true)}
            >
              <Upload className="h-3.5 w-3.5" />
              Import boundaries
            </Button>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void loadSummary()}
              disabled={loading}
            >
              <RefreshCw className="h-3.5 w-3.5" />
              {loading ? "Loading…" : "Refresh"}
            </Button>
          </>
        }
      />

      <BoundaryImportDialog
        orgSlug={orgSlug}
        open={showImport}
        onOpenChange={setShowImport}
      />

      {error ? <ErrorNote message={error} /> : null}

      <Card className="mb-4">
        <CardContent className="flex flex-wrap items-end gap-4 pt-6">
          <div className="grid gap-1.5">
            <Label htmlFor="loc-from">From</Label>
            <Input
              id="loc-from"
              type="date"
              value={from}
              onChange={(e) => setFrom(e.target.value)}
            />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="loc-to">To</Label>
            <Input
              id="loc-to"
              type="date"
              value={to}
              onChange={(e) => setTo(e.target.value)}
            />
          </div>
          <div className="grid min-w-48 gap-1.5">
            <Label htmlFor="loc-offering">Offering</Label>
            <Select
              id="loc-offering"
              value={offeringId}
              onChange={(e) => setOfferingId(e.target.value)}
            >
              <option value="">All offerings</option>
              {offerings.map((o) => (
                <option key={o.id} value={o.id}>
                  {o.name}
                </option>
              ))}
            </Select>
          </div>
          <p className="pb-2 text-xs text-muted-foreground">
            {loading
              ? "Loading locations…"
              : `${points.length.toLocaleString()} location${points.length === 1 ? "" : "s"}${
                  clusters.length > 0 && points.length === 0
                    ? ` in ${clusters.length} cluster${clusters.length === 1 ? "" : "s"}`
                    : ""
                }`}
          </p>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex-row items-start justify-between space-y-0">
          <div>
            <CardTitle>Demand map</CardTitle>
            <CardDescription>
              Self-hosted basemap (geo-service) · points cluster as you zoom
              out; click a cluster to zoom in.
            </CardDescription>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button
              variant="outline"
              size="sm"
              onClick={() => setShowAreas((v) => !v)}
            >
              <Layers className="h-3.5 w-3.5" />
              {showAreas ? "Hide service areas" : "Show service areas"}
            </Button>
            <Button
              variant={drawMode ? "secondary" : "outline"}
              size="sm"
              onClick={() => {
                setDrawMode((v) => !v);
                setPendingGeometry(null);
              }}
            >
              <PenLine className="h-3.5 w-3.5" />
              {drawMode ? "Cancel drawing" : "Draw new area"}
            </Button>
            <Button
              variant="outline"
              size="sm"
              disabled={!selectedArea}
              onClick={() => selectedArea && setDeleteTarget(selectedArea)}
            >
              <Trash2 className="h-3.5 w-3.5" />
              Delete selected
            </Button>
          </div>
        </CardHeader>
        <CardContent>
          {drawMode ? (
            <p className="mb-3 rounded-md border border-border bg-accent px-3 py-2 text-xs text-muted-foreground">
              Drawing a service area: click on the map to add vertices,
              double-click to finish the polygon.
            </p>
          ) : null}
          {pendingGeometry ? (
            <div className="mb-3 flex flex-wrap items-end gap-2 rounded-md border border-border bg-accent px-3 py-2">
              <div className="grid min-w-56 gap-1.5">
                <Label htmlFor="area-name">Name this service area</Label>
                <Input
                  id="area-name"
                  value={areaName}
                  onChange={(e) => setAreaName(e.target.value)}
                  placeholder="e.g. City centre delivery zone"
                />
              </div>
              <Button
                size="sm"
                onClick={() => void saveArea()}
                disabled={busy || !areaName.trim()}
              >
                {busy ? "Saving…" : "Save area"}
              </Button>
              <Button
                variant="outline"
                size="sm"
                onClick={() => setPendingGeometry(null)}
              >
                Discard
              </Button>
            </div>
          ) : null}

          <LocationsMap
            points={points}
            fallbackClusters={clusters}
            areas={areaFeatures}
            showAreas={showAreas}
            selectedAreaId={selectedAreaId}
            onSelectArea={setSelectedAreaId}
            drawMode={drawMode}
            onAreaDrawn={handleAreaDrawn}
          />

          {empty ? (
            <div className="mt-4 flex items-start gap-3 rounded-md border border-dashed border-border bg-accent px-4 py-3">
              <MapPinOff className="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground" />
              <div>
                <p className="text-sm font-medium">
                  No locations captured yet
                </p>
                <p className="mt-0.5 text-xs text-muted-foreground">
                  Locations come from bookings and contact addresses — when a
                  booking carries an address, or a customer shares their
                  location over a channel, it shows up here. Try widening the
                  date range if you expected data.
                </p>
              </div>
            </div>
          ) : null}
        </CardContent>
      </Card>

      <Card className="mt-4">
        <CardHeader>
          <CardTitle>Service areas</CardTitle>
          <CardDescription>
            Saved polygons ({areas.length}). Select one to highlight it on the
            map, or draw a new one above.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {areasError ? <ErrorNote message={areasError} /> : null}
          {areas.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No service areas yet — use “Draw new area” on the map to create
              the first one.
            </p>
          ) : (
            <ul className="divide-y divide-border">
              {areas.map((area) => (
                <li
                  key={area.id}
                  className="flex items-center justify-between gap-3 py-2"
                >
                  <button
                    type="button"
                    onClick={() =>
                      setSelectedAreaId((cur) =>
                        cur === area.id ? null : area.id,
                      )
                    }
                    className={`min-w-0 flex-1 cursor-pointer text-left text-sm ${
                      selectedAreaId === area.id
                        ? "font-semibold text-foreground"
                        : "text-muted-foreground hover:text-foreground"
                    }`}
                  >
                    <span className="block truncate">{area.name}</span>
                    {area.created_at ? (
                      <span className="block text-xs text-muted-foreground">
                        Created {formatDateTime(area.created_at)}
                      </span>
                    ) : null}
                  </button>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => setDeleteTarget(area)}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                    Delete
                  </Button>
                </li>
              ))}
            </ul>
          )}
        </CardContent>
      </Card>

      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => {
          if (!open) setDeleteTarget(null);
        }}
        title={`Delete “${deleteTarget?.name ?? ""}”?`}
        description="The service-area polygon is removed permanently. Campaigns that already ran are not affected."
        confirmLabel="Delete area"
        destructive
        busy={busy}
        onConfirm={() => void deleteArea()}
      />
    </div>
  );
}
