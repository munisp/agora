import { Badge } from "@/components/ui/badge";

/**
 * SPEC-W52: small "auto" marker shown next to LGA/ward values that were
 * auto-filled by the geo-service point-in-polygon enrichment
 * (`geo_enrichment_source === 'geo-service'`). Renders nothing for manually
 * entered values, and — because the columns are new this wave — for older
 * API responses that omit the field entirely (undefined).
 */
export function GeoAutoBadge({ source }: { source?: string | null }) {
  if (source !== "geo-service") return null;
  return (
    <Badge
      variant="info"
      className="ml-1 px-1.5 py-0 text-[10px] font-semibold uppercase tracking-wide"
      title="Auto-filled from the capture coordinates by the geo-service boundary lookup"
    >
      auto
    </Badge>
  );
}
