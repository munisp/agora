/**
 * Shared CAC dashboard types and pure helpers (SPEC-W13 Agent D, contract §5).
 * Nothing here touches WebGL or the network — safe to import anywhere.
 */
import type { Feature, Geometry, MultiPolygon, Polygon } from "geojson";

/** Contract §5: one row of `by_channel`. */
export interface CacChannelRow {
  channel: string;
  spend_ngn: number;
  leads: number;
  conversions: number;
  cac_ngn: number;
}

/**
 * Contract §5: one row of `by_lga`. The gold table
 * (cac_gold.daily_cac_by_lga) carries `geom`; the realtime rollup should
 * pass it through so the choropleth can render LGA polygons. Rows without
 * usable geometry still appear in the table view.
 */
export interface CacLgaRow {
  lga_id: number;
  lga_name?: string | null;
  leads: number;
  conversions: number;
  cac_ngn: number;
  /** GeoJSON geometry — object, JSON string, bare geometry or Feature. */
  geom?: unknown;
  geojson?: unknown;
}

/**
 * Contract §5 response (GET /v1/cac/summary?from&to).
 * `ltv_ngn` is a tolerated extension: when the analytics service learns to
 * estimate customer lifetime value the LTV/CAC card fills in automatically;
 * until then it renders an honest empty state.
 */
export interface CacSummary {
  by_channel: CacChannelRow[];
  by_lga: CacLgaRow[];
  blended_cac_ngn: number;
  payback_days_estimate: number | null;
  ltv_ngn?: number | null;
}

/** booking-service promo_codes row (contract §6) as returned by GET /v1/promo. */
export interface PromoCodeRow {
  code: string;
  campaign_id?: string | null;
  campaign_name?: string | null;
  discount_ngn?: number | null;
  max_redemptions?: number | null;
  redeemed_count: number;
}

/** booking-service campaign row as returned by GET /v1/campaigns. */
export interface CampaignRow {
  id: string;
  name?: string | null;
  channel?: string | null;
  spend_ngn?: number | null;
}

/** Format a whole-naira amount (NGN has no kobo conversion here). */
export function formatNaira(amount: number, locale = "en-NG"): string {
  return new Intl.NumberFormat(locale, {
    style: "currency",
    currency: "NGN",
    maximumFractionDigits: 0,
  }).format(amount);
}

/** Compact number for counts (1,234). */
export function formatCount(n: number, locale = "en-NG"): string {
  return new Intl.NumberFormat(locale).format(n);
}

/**
 * Normalise a by_lga geometry into a GeoJSON polygon feature. Accepts the
 * same shapes as lib/geo's serviceAreaToFeature (object or JSON string,
 * bare geometry or Feature). Returns null when no usable polygon is present.
 */
export function lgaToFeature(
  row: CacLgaRow,
  metric: number,
): Feature<Polygon | MultiPolygon> | null {
  const raw = row.geom ?? row.geojson;
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
  const candidate = parsed as { type?: string; geometry?: Geometry };
  const geometry =
    candidate.type === "Feature" && candidate.geometry
      ? candidate.geometry
      : (candidate as unknown as Geometry);
  if (geometry.type !== "Polygon" && geometry.type !== "MultiPolygon") {
    return null;
  }
  return {
    type: "Feature",
    properties: {
      lgaId: row.lga_id,
      name: row.lga_name ?? `LGA ${row.lga_id}`,
      value: metric,
      leads: row.leads,
      conversions: row.conversions,
      cacNgn: row.cac_ngn,
    },
    geometry: geometry as Polygon | MultiPolygon,
  };
}

/** Signed % change between two periods; null when the base is zero/unknown. */
export function pctChange(current: number, previous: number): number | null {
  if (!Number.isFinite(previous) || previous <= 0) return null;
  return ((current - previous) / previous) * 100;
}
