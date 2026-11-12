/**
 * Geospatial helpers shared by the Locations and Geo campaigns dashboards
 * (SPEC-W8 Part C; SPEC-W52 self-hosted basemap). Nothing here touches WebGL,
 * so it is safe to import from server or client components.
 */
import type { Feature, Geometry, MultiPolygon, Polygon } from "geojson";
import type { StyleSpecification } from "maplibre-gl";
import type { ServiceArea } from "@/lib/types";

/**
 * SPEC-W52: self-hosted basemap style served by the geo-service (terrio)
 * through the APISIX gateway. This replaces the previous hot-linked
 * `tile.openstreetmap.org` raster source and `demotiles.maplibre.org` glyphs
 * (ToS + offline-hosting problem — RECON §1-C): the style document returned
 * by this URL points at the geo-service's own tile/glyph endpoints, so the
 * browser only ever talks to our gateway. The relative default resolves
 * through the admin-web BFF proxy, which attaches the Keycloak token before
 * forwarding to APISIX. Override with NEXT_PUBLIC_GEO_STYLE_URL.
 */
export const GEO_STYLE_URL: string =
  process.env.NEXT_PUBLIC_GEO_STYLE_URL ?? "/api/geo/style.json";

/**
 * @deprecated Use {@link GEO_STYLE_URL}. Kept as a same-typed alias so the
 * geo-campaigns target map and the CAC LGA map — which still import this name
 * and are outside the W52 Scope-C file set — keep compiling; its value is now
 * the self-hosted style URL, not the old OSM raster hot-link.
 */
export const OSM_RASTER_STYLE: string = GEO_STYLE_URL;

/**
 * Fetch and validate the basemap style document. Map components call this
 * BEFORE constructing `maplibregl.Map` so a missing/unreachable geo-service
 * (or unprovisioned tiles, answered 503 by the service) is a deterministic,
 * catchable failure — the caller renders a "map tiles unavailable" notice
 * instead of a blank canvas. Throws on network error, non-2xx, or a payload
 * that is not a MapLibre style document. No hotlink fallback: when the
 * self-hosted style is down, the basemap is honestly absent.
 */
export async function fetchMapStyle(
  signal?: AbortSignal,
): Promise<StyleSpecification> {
  const res = await fetch(GEO_STYLE_URL, { signal, cache: "no-store" });
  if (!res.ok) {
    throw new Error(`Map style request failed with status ${res.status}`);
  }
  const style: unknown = await res.json();
  const candidate =
    typeof style === "object" && style !== null
      ? (style as { version?: unknown; layers?: unknown })
      : null;
  if (!candidate || candidate.version !== 8 || !Array.isArray(candidate.layers)) {
    throw new Error("Map style endpoint did not return a valid style document");
  }
  return style as StyleSpecification;
}

/** Default world view before any data is fitted. */
export const DEFAULT_CENTER: [number, number] = [3.3792, 6.5244]; // Lagos
export const DEFAULT_ZOOM = 2;

const EARTH_RADIUS_M = 6371000;

/**
 * Approximate a geodesic circle as a 64-point GeoJSON polygon — the shape the
 * audience preview and campaign APIs accept for radius targeting.
 */
export function circlePolygon(
  center: { lat: number; lng: number },
  radiusM: number,
  points = 64,
): Feature<Polygon> {
  const ring: [number, number][] = [];
  const latRad = (center.lat * Math.PI) / 180;
  const lonRad = (center.lng * Math.PI) / 180;
  const angular = radiusM / EARTH_RADIUS_M;
  for (let i = 0; i <= points; i++) {
    const bearing = (i / points) * 2 * Math.PI;
    const lat2 = Math.asin(
      Math.sin(latRad) * Math.cos(angular) +
        Math.cos(latRad) * Math.sin(angular) * Math.cos(bearing),
    );
    const lon2 =
      lonRad +
      Math.atan2(
        Math.sin(bearing) * Math.sin(angular) * Math.cos(latRad),
        Math.cos(angular) - Math.sin(latRad) * Math.sin(lat2),
      );
    ring.push([(lon2 * 180) / Math.PI, (lat2 * 180) / Math.PI]);
  }
  return {
    type: "Feature",
    properties: {},
    geometry: { type: "Polygon", coordinates: [ring] },
  };
}

/** "500 m" / "2.5 km" label for the radius slider. */
export function formatRadius(radiusM: number): string {
  if (radiusM >= 1000) {
    const km = radiusM / 1000;
    return `${Number.isInteger(km) ? km : km.toFixed(1)} km`;
  }
  return `${radiusM} m`;
}

/** Bounds [[minLng, minLat], [maxLng, maxLat]] over a set of lng/lat pairs. */
export function boundsOf(
  coords: { lat: number; lng: number }[],
): [[number, number], [number, number]] | null {
  if (coords.length === 0) return null;
  let minLng = Infinity;
  let minLat = Infinity;
  let maxLng = -Infinity;
  let maxLat = -Infinity;
  for (const c of coords) {
    if (c.lng < minLng) minLng = c.lng;
    if (c.lng > maxLng) maxLng = c.lng;
    if (c.lat < minLat) minLat = c.lat;
    if (c.lat > maxLat) maxLat = c.lat;
  }
  return [
    [minLng, minLat],
    [maxLng, maxLat],
  ];
}

/**
 * Normalise a service-area row into a GeoJSON feature. The booking-service
 * may return the geometry under `geojson` or `geom`, as an object or a JSON
 * string, and as either a bare geometry or a full feature — accept them all.
 * Returns null when no usable polygon geometry is present.
 */
export function serviceAreaToFeature(
  area: ServiceArea,
): Feature<Polygon | MultiPolygon> | null {
  const raw = area.geojson ?? area.geom;
  if (!raw) return null;
  let parsed: unknown = raw;
  if (typeof raw === "string") {
    try {
      parsed = JSON.parse(raw);
    } catch {
      return null;
    }
  }
  if (typeof parsed !== "object" || parsed === null) return null;
  const candidate = parsed as {
    type?: string;
    geometry?: Geometry;
  };
  const geometry =
    candidate.type === "Feature" && candidate.geometry
      ? candidate.geometry
      : (candidate as unknown as Geometry);
  if (geometry.type !== "Polygon" && geometry.type !== "MultiPolygon") {
    return null;
  }
  return {
    type: "Feature",
    properties: { areaId: area.id, name: area.name },
    geometry: geometry as Polygon | MultiPolygon,
  };
}
