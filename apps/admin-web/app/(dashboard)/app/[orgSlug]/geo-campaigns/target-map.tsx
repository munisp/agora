"use client";

/**
 * Campaign target map (SPEC-W8 Part C): draw a polygon with gl-draw, or click
 * a centre point and let the parent render a radius circle. The effective
 * target (drawn polygon OR circle polygon) is owned by the parent and passed
 * back down as GeoJSON for rendering — one source of truth for the audience
 * preview and the launch payload.
 *
 * Loaded via next/dynamic with ssr:false — maplibre-gl never runs during SSR.
 * See locations-map.tsx for the `@telida/maplibre-gl-draw` substitution note
 * (upstream package: `@maplibre/maplibre-gl-draw`).
 */
import * as React from "react";
import maplibregl from "maplibre-gl";
import MapLibreDraw from "@telida/maplibre-gl-draw";
import "maplibre-gl/dist/maplibre-gl.css";
import "@telida/maplibre-gl-draw/dist/maplibre-gl-draw.css";
import type { Feature, FeatureCollection, Polygon } from "geojson";
import { DEFAULT_CENTER, DEFAULT_ZOOM, OSM_RASTER_STYLE } from "@/lib/geo";

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

export interface TargetMapProps {
  mode: "polygon" | "circle";
  /** Circle centre marker (circle mode only). */
  center: { lat: number; lng: number } | null;
  /** Effective target polygon to render (drawn polygon or radius circle). */
  target: Feature<Polygon> | null;
  onPolygon: (geometry: Polygon) => void;
  onCenter: (center: { lat: number; lng: number }) => void;
}

export default function TargetMap({
  mode,
  center,
  target,
  onPolygon,
  onCenter,
}: TargetMapProps) {
  const containerRef = React.useRef<HTMLDivElement | null>(null);
  const mapRef = React.useRef<maplibregl.Map | null>(null);
  const drawRef = React.useRef<MapLibreDraw | null>(null);
  const markerRef = React.useRef<maplibregl.Marker | null>(null);
  const [ready, setReady] = React.useState(false);

  const modeRef = React.useRef(mode);
  modeRef.current = mode;
  const onPolygonRef = React.useRef(onPolygon);
  onPolygonRef.current = onPolygon;
  const onCenterRef = React.useRef(onCenter);
  onCenterRef.current = onCenter;

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

    const draw = new MapLibreDraw({ displayControlsDefault: false });
    drawRef.current = draw;
    map.addControl(draw, "top-left");

    map.on("load", () => {
      map.addSource("target", { type: "geojson", data: EMPTY_FC });
      map.addLayer({
        id: "target-fill",
        type: "fill",
        source: "target",
        paint: { "fill-color": "#8a6d4b", "fill-opacity": 0.22 },
      });
      map.addLayer({
        id: "target-line",
        type: "line",
        source: "target",
        paint: { "line-color": "#6d5233", "line-width": 2 },
      });
      setReady(true);
    });

    // Circle mode: a map click sets the centre point.
    map.on("click", (e) => {
      if (modeRef.current !== "circle") return;
      onCenterRef.current({ lat: e.lngLat.lat, lng: e.lngLat.lng });
    });

    onDraw(map, "draw.create", (e) => {
      const geometry = e.features[0]?.geometry;
      // The parent re-renders the shape via the `target` source, so the draw
      // control can stay clean for the next polygon.
      draw.deleteAll();
      if (geometry && geometry.type === "Polygon") {
        onPolygonRef.current(geometry as Polygon);
      }
    });

    return () => {
      markerRef.current = null;
      drawRef.current = null;
      mapRef.current = null;
      setReady(false);
      map.remove();
    };
  }, []);

  // ---- mode arming ----
  React.useEffect(() => {
    const draw = drawRef.current;
    if (!draw || !ready) return;
    // Separate calls — the changeMode overloads don't accept a union literal.
    if (mode === "polygon") draw.changeMode("draw_polygon");
    else draw.changeMode("simple_select");
  }, [ready, mode]);

  // ---- target rendering ----
  React.useEffect(() => {
    const map = mapRef.current;
    if (!map || !ready) return;
    const source = map.getSource("target");
    if (!(source instanceof maplibregl.GeoJSONSource)) return;
    source.setData(
      target
        ? { type: "FeatureCollection", features: [target] }
        : EMPTY_FC,
    );
  }, [ready, target]);

  // ---- circle centre marker ----
  React.useEffect(() => {
    const map = mapRef.current;
    if (!map || !ready) return;
    if (center) {
      if (!markerRef.current) {
        markerRef.current = new maplibregl.Marker({ color: "#8a6d4b" });
      }
      markerRef.current.setLngLat([center.lng, center.lat]).addTo(map);
    } else {
      markerRef.current?.remove();
      markerRef.current = null;
    }
  }, [ready, center]);

  return (
    <div
      ref={containerRef}
      className="h-[420px] w-full rounded-md border border-border bg-muted dark:brightness-90"
    />
  );
}
