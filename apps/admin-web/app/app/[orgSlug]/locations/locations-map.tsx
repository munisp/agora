"use client";

/**
 * Locations map (SPEC-W8 Part C; SPEC-W52 basemap): self-hosted basemap style
 * served by the geo-service (lib/geo fetchMapStyle), client-side clustered
 * booking/contact locations, toggleable service-area polygons, and polygon
 * drawing for new service areas. When the style fetch fails (geo-service
 * offline / tiles unprovisioned), the container renders an inline "map tiles
 * unavailable" notice instead of a blank canvas — no hotlink fallback.
 *
 * This component is loaded via next/dynamic with ssr:false, so the
 * maplibre-gl imports below never execute during SSR (WebGL is browser-only).
 *
 * Dependency note: the spec's intended draw package is
 * `@maplibre/maplibre-gl-draw` (^3), which is not published to the package
 * registry available in this environment. `@telida/maplibre-gl-draw` is the
 * same mapbox-gl-draw lineage with equivalent types/API; swapping back later
 * is a 2-line change (package.json + this import — same for target-map.tsx).
 */
import * as React from "react";
import maplibregl from "maplibre-gl";
import MapLibreDraw from "@telida/maplibre-gl-draw";
import "maplibre-gl/dist/maplibre-gl.css";
import "@telida/maplibre-gl-draw/dist/maplibre-gl-draw.css";
import type {
  Feature,
  FeatureCollection,
  MultiPolygon,
  Point,
  Polygon,
} from "geojson";
import { MapPinOff } from "lucide-react";
import {
  DEFAULT_CENTER,
  DEFAULT_ZOOM,
  GEO_STYLE_URL,
  boundsOf,
  fetchMapStyle,
} from "@/lib/geo";
import { Button } from "@/components/ui/button";
import type { LocationCluster, LocationPoint } from "@/lib/types";

/** Draw events are emitted by the control, not by maplibre's own event map. */
type DrawEventMap = {
  "draw.create": MapLibreDraw.DrawCreateEvent;
};

function onDraw<K extends keyof DrawEventMap>(
  map: maplibregl.Map,
  type: K,
  listener: (ev: DrawEventMap[K]) => void,
): void {
  (
    map as unknown as { on(t: K, l: (ev: DrawEventMap[K]) => void): void }
  ).on(type, listener);
}

const EMPTY_FC: FeatureCollection = { type: "FeatureCollection", features: [] };

export interface LocationsMapProps {
  /** Raw booking/contact points (client-side clustered by maplibre). */
  points: LocationPoint[];
  /** Server-computed clusters, rendered as weighted points when no raw points. */
  fallbackClusters: LocationCluster[];
  /** Service-area polygons (already normalised to GeoJSON features). */
  areas: Feature<Polygon | MultiPolygon>[];
  showAreas: boolean;
  selectedAreaId: string | null;
  onSelectArea: (id: string | null) => void;
  /** When true the draw_polygon mode is armed. */
  drawMode: boolean;
  /** A polygon was just drawn on the map. */
  onAreaDrawn: (geometry: Polygon | MultiPolygon) => void;
}

function pointsToFeatures(points: LocationPoint[]): Feature<Point>[] {
  return points.map((p) => ({
    type: "Feature",
    properties: {
      bookingId: p.booking_id ?? "",
      offeringId: p.offering_id ?? "",
      startsAt: p.starts_at ?? "",
      weight: 1,
    },
    geometry: { type: "Point", coordinates: [p.lng, p.lat] },
  }));
}

function clustersToFeatures(clusters: LocationCluster[]): Feature<Point>[] {
  return clusters.map((c) => ({
    type: "Feature",
    properties: { weight: c.count },
    geometry: { type: "Point", coordinates: [c.lng, c.lat] },
  }));
}

export default function LocationsMap({
  points,
  fallbackClusters,
  areas,
  showAreas,
  selectedAreaId,
  onSelectArea,
  drawMode,
  onAreaDrawn,
}: LocationsMapProps) {
  const containerRef = React.useRef<HTMLDivElement | null>(null);
  const mapRef = React.useRef<maplibregl.Map | null>(null);
  const drawRef = React.useRef<MapLibreDraw | null>(null);
  const [ready, setReady] = React.useState(false);
  // SPEC-W52 degraded mode: set when the self-hosted style fetch failed.
  const [styleError, setStyleError] = React.useState<string | null>(null);
  const [styleAttempt, setStyleAttempt] = React.useState(0);
  // Latest-callback refs so map event handlers never go stale.
  const onAreaDrawnRef = React.useRef(onAreaDrawn);
  onAreaDrawnRef.current = onAreaDrawn;
  const onSelectAreaRef = React.useRef(onSelectArea);
  onSelectAreaRef.current = onSelectArea;

  // ---- map lifecycle (per style attempt) ----
  React.useEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    let cancelled = false;
    setStyleError(null);

    // Fetch the style document first so a geo-service outage is a catchable
    // failure here, not a silent blank canvas inside maplibre.
    void fetchMapStyle()
      .then((style) => {
        if (cancelled || !containerRef.current) return;

    const map = new maplibregl.Map({
      container,
      style,
      center: DEFAULT_CENTER,
      zoom: DEFAULT_ZOOM,
      attributionControl: { compact: true },
    });
    mapRef.current = map;
    map.addControl(new maplibregl.NavigationControl(), "top-right");

    const draw = new MapLibreDraw({ displayControlsDefault: false });
    drawRef.current = draw;
    map.addControl(draw, "top-left");

    map.on("load", () => {
      // Booking/contact locations, clustered client-side (SPEC-W8 Part C).
      map.addSource("locations", {
        type: "geojson",
        data: EMPTY_FC,
        cluster: true,
        clusterRadius: 48,
        clusterMaxZoom: 14,
      });
      map.addLayer({
        id: "locations-clusters",
        type: "circle",
        source: "locations",
        filter: ["has", "point_count"],
        paint: {
          "circle-color": [
            "step",
            ["get", "point_count"],
            "#a78b6a",
            25,
            "#8a6d4b",
            100,
            "#6d5233",
          ],
          "circle-radius": [
            "step",
            ["get", "point_count"],
            14,
            25,
            19,
            100,
            26,
          ],
          "circle-opacity": 0.85,
          "circle-stroke-width": 2,
          "circle-stroke-color": "#ffffff",
        },
      });
      map.addLayer({
        id: "locations-cluster-count",
        type: "symbol",
        source: "locations",
        filter: ["has", "point_count"],
        layout: {
          "text-field": "{point_count_abbreviated}",
          "text-size": 12,
        },
        paint: { "text-color": "#ffffff" },
      });
      map.addLayer({
        id: "locations-points",
        type: "circle",
        source: "locations",
        filter: ["!", ["has", "point_count"]],
        paint: {
          "circle-color": "#8a6d4b",
          "circle-radius": 6,
          "circle-opacity": 0.9,
          "circle-stroke-width": 1.5,
          "circle-stroke-color": "#ffffff",
        },
      });

      // Service-area polygons (toggleable overlay).
      map.addSource("service-areas", { type: "geojson", data: EMPTY_FC });
      map.addLayer({
        id: "service-areas-fill",
        type: "fill",
        source: "service-areas",
        paint: {
          "fill-color": "#5b7c99",
          "fill-opacity": [
            "case",
            ["==", ["get", "areaId"], ""],
            0.42,
            0.16,
          ],
        },
      });
      map.addLayer({
        id: "service-areas-line",
        type: "line",
        source: "service-areas",
        paint: { "line-color": "#47657e", "line-width": 1.5 },
      });

      // Cluster click → zoom into the cluster.
      map.on("click", "locations-clusters", (e) => {
        const geometry = e.features?.[0]?.geometry;
        if (!geometry || geometry.type !== "Point") return;
        const clusterId = e.features?.[0]?.properties?.cluster_id;
        const source = map.getSource("locations");
        if (!(source instanceof maplibregl.GeoJSONSource)) return;
        void source
          .getClusterExpansionZoom(clusterId)
          .then((zoom) => {
            map.easeTo({
              center: geometry.coordinates as [number, number],
              zoom,
            });
          })
          .catch(() => undefined);
      });

      // Service-area click → select for deletion.
      map.on("click", "service-areas-fill", (e) => {
        const areaId = e.features?.[0]?.properties?.areaId;
        onSelectAreaRef.current(typeof areaId === "string" ? areaId : null);
      });

      for (const layer of [
        "locations-clusters",
        "locations-points",
        "service-areas-fill",
      ]) {
        map.on("mouseenter", layer, () => {
          map.getCanvas().style.cursor = "pointer";
        });
        map.on("mouseleave", layer, () => {
          map.getCanvas().style.cursor = "";
        });
      }

      setReady(true);
    });

    onDraw(map, "draw.create", (e) => {
      const geometry = e.features[0]?.geometry;
      draw.deleteAll();
      if (
        geometry &&
        (geometry.type === "Polygon" || geometry.type === "MultiPolygon")
      ) {
        onAreaDrawnRef.current(geometry as Polygon | MultiPolygon);
      }
    });
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setStyleError(
          e instanceof Error ? e.message : "The map style could not be loaded.",
        );
      });

    return () => {
      cancelled = true;
      drawRef.current = null;
      const map = mapRef.current;
      mapRef.current = null;
      setReady(false);
      if (map) map.remove();
    };
  }, [styleAttempt]);

  // ---- locations data ----
  React.useEffect(() => {
    const map = mapRef.current;
    if (!map || !ready) return;
    const source = map.getSource("locations");
    if (!(source instanceof maplibregl.GeoJSONSource)) return;
    // Prefer raw points (client-side clustering); when the server only
    // returned pre-clustered cells, render those as weighted single points.
    const features =
      points.length > 0
        ? pointsToFeatures(points)
        : clustersToFeatures(fallbackClusters);
    source.setData({ type: "FeatureCollection", features });
    const bounds = boundsOf(points.length > 0 ? points : fallbackClusters);
    if (bounds) {
      map.fitBounds(bounds, { padding: 64, maxZoom: 13, duration: 500 });
    }
  }, [ready, points, fallbackClusters]);

  // ---- service-area polygons ----
  React.useEffect(() => {
    const map = mapRef.current;
    if (!map || !ready) return;
    const source = map.getSource("service-areas");
    if (source instanceof maplibregl.GeoJSONSource) {
      source.setData({ type: "FeatureCollection", features: areas });
    }
    const visibility = showAreas ? "visible" : "none";
    for (const layer of ["service-areas-fill", "service-areas-line"]) {
      map.setLayoutProperty(layer, "visibility", visibility);
    }
  }, [ready, areas, showAreas]);

  // ---- selected-area highlight ----
  React.useEffect(() => {
    const map = mapRef.current;
    if (!map || !ready) return;
    map.setPaintProperty("service-areas-fill", "fill-opacity", [
      "case",
      ["==", ["get", "areaId"], selectedAreaId ?? ""],
      0.42,
      0.16,
    ]);
  }, [ready, selectedAreaId]);

  // ---- draw mode arming ----
  React.useEffect(() => {
    const draw = drawRef.current;
    if (!draw || !ready) return;
    // Separate calls — the changeMode overloads don't accept a union literal.
    if (drawMode) draw.changeMode("draw_polygon");
    else draw.changeMode("simple_select");
  }, [ready, drawMode]);

  return (
    <div className="relative h-[520px] w-full rounded-md border border-border bg-muted dark:brightness-90">
      <div ref={containerRef} className="h-full w-full rounded-md" />
      {styleError ? (
        <div className="absolute inset-0 flex items-center justify-center rounded-md bg-muted px-6">
          <div className="max-w-sm text-center">
            <MapPinOff className="mx-auto h-6 w-6 text-muted-foreground" />
            <p className="mt-2 text-sm font-medium text-foreground">
              Map tiles unavailable
            </p>
            <p className="mt-1 text-xs text-muted-foreground">
              The self-hosted basemap ({GEO_STYLE_URL}) could not be loaded —
              the geo-service may be offline or its tiles are not provisioned.
              The locations list and filters still work; drawing new service
              areas is disabled until the map is back.
            </p>
            <p className="mt-1 font-mono text-[11px] text-muted-foreground">
              {styleError}
            </p>
            <Button
              variant="outline"
              size="sm"
              className="mt-3"
              onClick={() => setStyleAttempt((n) => n + 1)}
            >
              Retry
            </Button>
          </div>
        </div>
      ) : null}
    </div>
  );
}
