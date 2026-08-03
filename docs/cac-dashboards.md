# CAC dashboards (admin-web)

Wave 13 (SPEC-W13, Agent D). Customer-acquisition-cost dashboards in the
admin app: blended CAC and payback overview cards, a by-channel table with
period-over-period trend, a by-LGA MapLibre choropleth, and top promo
codes / campaign spend tables.

## Route and gating

- Page: `app/app/[orgSlug]/cac/page.tsx` → `/app/{orgSlug}/cac`.
- Server-side role guard identical to the analytics page: `canViewAnalytics`
  (Keycloak realm roles `owner` / `admin` / `analyst`); everyone else is
  redirected to the org overview. The backing API is Permify
  `view_analytics`-gated on the service side, so the UI rule and the API
  rule match.
- Nav: one additive entry ("CAC", `CircleDollarSign` icon) in
  `components/org-nav.tsx`, hidden client-side under the same role rule.

## Data sources

All requests go through the app's established BFF proxy
(`app/api/[[...path]]/route.ts`) with the Keycloak access token and the
`x-tenant-slug` header attached (tenant passed as `?tenant=`).

| UI section | Endpoint | Notes |
| --- | --- | --- |
| Overview cards, by-channel table, by-LGA choropleth | `GET /api/analytics/v1/cac/summary?from&to` | SPEC-W13 contract §5. `/api/analytics/*` is the BFF special-case that forwards directly to the analytics service (port 7009), same path the billing page uses for `/api/analytics/v1/recommendations`. |
| Top promo codes | `GET /api/bookings/v1/promo` | Contract §6 list read. Optional: if unavailable the table shows a muted note instead of failing the page. |
| Campaign spend | `GET /api/bookings/v1/campaigns` | Contract §4 list read (spend enters via `POST /v1/campaigns/{id}/spend`). Same soft-failure treatment. |

### Contract §5 response

```json
{
  "by_channel": [{"channel": "whatsapp", "spend_ngn": 0, "leads": 0, "conversions": 0, "cac_ngn": 0}],
  "by_lga": [{"lga_id": 0, "lga_name": "…", "leads": 0, "conversions": 0, "cac_ngn": 0, "geom": {…}}],
  "blended_cac_ngn": 0,
  "payback_days_estimate": 0,
  "ltv_ngn": null
}
```

- `by_lga[].geom` is GeoJSON (object or JSON string, bare geometry or
  Feature, Polygon/MultiPolygon) carried through from
  `cac_gold.daily_cac_by_lga`. It may be `null` when the analytics service
  cannot resolve the LGA boundary — those rows render in the table below
  the map with a "No geometry" marker, never silently dropped.
- `ltv_ngn` is an optional extension agreed for this wave: when present, the
  LTV/CAC card shows the real ratio; when absent it renders "—" with an
  explanatory hint (no fabricated numbers).
- The API is the realtime rollup fed by the `cac.events` consumer; the
  lakehouse Iceberg gold tables are the batch-verified source (documented
  dual-path: realtime rollup + nightly reconcile — see
  `docs/cac-analytics-api.md` and `docs/cac-lakehouse.md`).

## UI structure

- `app/app/[orgSlug]/cac/cac-client.tsx` — period selector (7d/30d/90d),
  refresh, data loading, error/loading/empty states.
- `components/cac/cac-summary-cards.tsx` — blended CAC, payback-days
  estimate, LTV/CAC ratio, conversions (mirrors the Wave-7 KPI card
  language).
- `components/cac/cac-channel-table.tsx` — spend, leads, conversions,
  conversion rate, CAC and CAC trend per channel. The trend is computed by
  fetching the previous window of equal length from the same §5 endpoint
  (no extra backend surface); for CAC, down is good.
- `components/cac/cac-lga-section.tsx` + `components/cac/cac-lga-map.tsx` —
  MapLibre choropleth (metric toggle: leads / conversions / CAC, warm
  low-saturation ramp `#efe4d3 → #8a6d4b`, click-to-select with detail bar,
  legend) plus the full table fallback. The map reuses the Wave-8
  foundation (`OSM_RASTER_STYLE`, `DEFAULT_CENTER`, `DEFAULT_ZOOM` from
  `lib/geo`) and is loaded with `next/dynamic` `ssr:false` — WebGL never
  runs during SSR.
- `components/cac/cac-promo-table.tsx` — top promo codes by redemptions and
  campaigns by recorded spend (top 10 each).
- `components/cac/types.ts` — contract types and pure helpers (naira
  formatting, geometry normalisation, pct change).

## Conventions honoured

- No new dependencies: charts are hand-rolled (CSS/table/SVG-free here);
  the only map library is the already-present `maplibre-gl`. No charting
  package exists in `package.json` and none was added.
- Design language matches the existing dashboards: warm low-saturation
  palette, `Card`/`Table`/`PageHeader`/`ErrorNote` shadcn primitives,
  responsive grids (`sm:`/`lg:`/`xl:` breakpoints).
- Amounts are whole naira (contract fields are `*_ngn`), formatted via
  `Intl.NumberFormat("en-NG", { currency: "NGN" })` — distinct from the
  cents-based `formatMoney` helper used elsewhere.

## Verification

`npx tsc --noEmit` (app tsconfig) is clean. Runtime behaviour depends on
the Wave-13 backend pieces landing: analytics-service §5 route (Agent B)
and booking-service promo/campaign list reads (Agent A) — both degrade
gracefully in the UI if absent.
