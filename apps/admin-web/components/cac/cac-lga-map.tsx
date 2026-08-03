"use client";

/**
 * CAC by-LGA choropleth (SPEC-W13 Agent D): LGA polygons from the contract
 * §5 `by_lga` rows (geom carried through from cac_gold.daily_cac_by_lga),
 * filled by the selected metric. Reuses the Wave-8 map foundation
 * (OSM_RASTER_STYLE / DEFAULT_* from lib/geo) and is loaded via
 * next/dynamic with ssr:false, so maplibre-gl never runs during SSR.
 */
import * as React from "react";
import maplibregl from "maplibre-gl";
import "maplibre-gl/dist/maplibre-gl.css";
import type {
  Feature,
  FeatureCollection,
  MultiPolygon,
  Polygon,
} from "geojson";
import { DEFAULT_CENTER, DEFAULT_ZOOM, OSM_RASTER_STYLE } from "@/lib/geo";
import { lgaToFeature, type CacLgaRow } from "@/components/cac/types";

export type LgaMetric = "leads" | "conversions" | "cac_ngn";

const EMPTY_FC: FeatureCollection = { type: "FeatureCollection", features: [] };

/**
 * Warm, low-saturation ramp matching the dashboard palette (cream → the
 * service-area brown used across the Wave-8 maps).
 */
const RAMP: [string, string] = ["#efe4d3", "#8a6d4b"];

function metricValue(row: CacLgaRow, metric: LgaMetric): number {
  return metric === "cac_ngn" ? row.cac_ngn : row[metric];
}

function boundsOfFeatures(
  features: Feature<Polygon | MultiPolygon>[],
): [[number, number], [number, number]] | null {
  let minLng = Infinity;
  let minLat = Infinity;
  let maxLng = -Infinity;
  let maxLat = -Infinity;
  const visit = (coords: unknown): void => {
    if (!Array.isArray(coords)) return;
    if (
      coords.length >= 2 &&
      typeof coords[0] === "number" &&
      typeof coords[1] === "number"
    ) {
      const [lng, lat] = coords as [number, number];
      if (lng < minLng) minLng = lng;
      if (lng > maxLng) maxLng = lng;
      if (lat < minLat) minLat = lat;
      if (lat > maxLat) maxLat = lat;
      return;
    }
    for (const c of coords) visit(c);
  };
  for (const f of features) visit(f.geometry?.coordinates);
  if (!Number.isFinite(minLng)) return null;
  return [
    [minLng, minLat],
    [maxLng, maxLat],
  ];
}

export interface CacLgaMapProps {
  rows: CacLgaRow[];
  metric: LgaMetric;
  selectedLgaId: number | null;
  onSelectLga: (id: number | null) => void;
}

export default function CacLgaMap({
  rows,
  metric,
  selectedLgaId,
  onSelectLga,
}: CacLgaMapProps) {
  const containerRef = React.useRef<HTMLDivElement | null>(null);
  const mapRef = React.useRef<maplibregl.Map | null>(null);
  const [ready, setReady] = React.useState(false);
  const onSelectLgaRef = React.useRef(onSelectLga);
  onSelectLgaRef.current = onSelectLga;

  const features = React.useMemo(
    () =>
      rows
        .map((r) => lgaToFeature(r, metricValue(r, metric)))
        .filter((f): f is NonNullable<typeof f> => f !== null),
    [rows, metric],
  );

  // ---- map lifecycle (once) ----
  React.useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    const map = new maplibregl.Map({
      container,
      style: OSM_RASTER_STYLE,
      center: DEFAULT_CENTER,
      zoom: DEFAULT_ZOOM,
      attributionControl: { compact: true },
    });
    mapRef.current = map;
    map.addControl(new maplibregl.NavigationControl(), "top-right");

    map.on("load", () => {
      map.addSource("cac-lga", { type: "geojson", data: EMPTY_FC });
      map.addLayer({
        id: "cac-lga-fill",
        type: "fill",
        source: "cac-lga",
        paint: {
          "fill-color": [
            "interpolate",
            ["linear"],
            ["get", "value"],
            0,
            RAMP[0],
            1,
            RAMP[1],
          ],
          "fill-opacity": 0.75,
        },
      });
      map.addLayer({
        id: "cac-lga-line",
        type: "line",
        source: "cac-lga",
        paint: { "line-color": "#6d5233", "line-width": 1 },
      });
      map.addLayer({
        id: "cac-lga-selected",
        type: "line",
        source: "cac-lga",
        filter: ["==", ["get", "lgaId"], -1],
        paint: { "line-color": "#3f5a0f", "line-width": 2.5 },
      });

      map.on("click", "cac-lga-fill", (e) => {
        const lgaId = e.features?.[0]?.properties?.lgaId;
        onSelectLgaRef.current(typeof lgaId === "number" ? lgaId : null);
      });
      map.on("mouseenter", "cac-lga-fill", () => {
        map.getCanvas().style.cursor = "pointer";
      });
      map.on("mouseleave", "cac-lga-fill", () => {
        map.getCanvas().style.cursor = "";
      });

      setReady(true);
    });

    return () => {
      mapRef.current = null;
      setReady(false);
      map.remove();
    };
  }, []);

  // ---- data + colour scale ----
  React.useEffect(() => {
    const map = mapRef.current;
    if (!map || !ready) return;
    const source = map.getSource("cac-lga");
    if (!(source instanceof maplibregl.GeoJSONSource)) return;
    source.setData({ type: "FeatureCollection", features });

    const max = Math.max(0, ...features.map((f) => Number(f.properties?.value) || 0));
    // Zero-max would make the interpolate stops degenerate; clamp to 1.
    map.setPaintProperty("cac-lga-fill", "fill-color", [
      "interpolate",
      ["linear"],
      ["get", "value"],
      0,
      RAMP[0],
      Math.max(1, max),
      RAMP[1],
    ]);

    const bounds = boundsOfFeatures(features);
    if (bounds) {
      map.fitBounds(bounds, { padding: 48, maxZoom: 11, duration: 500 });
    }
  }, [ready, features]);

  // ---- selection highlight ----
  React.useEffect(() => {
    const map = mapRef.current;
    if (!map || !ready) return;
    map.setFilter("cac-lga-selected", [
      "==",
      ["get", "lgaId"],
      selectedLgaId ?? -1,
    ]);
  }, [ready, selectedLgaId]);

  return (
    <div
      ref={containerRef}
      className="h-[480px] w-full rounded-md border border-border bg-muted dark:brightness-90"
    />
  );
}
