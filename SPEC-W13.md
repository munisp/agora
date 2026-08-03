# SPEC-W13 — Growth Core: Leads, Attribution, Funnels, CAC Dashboards (CAC App, Wave 2 of 5)

Wave 13 of the CAC program. Five builders, strict file ownership. Same protocol as SPEC-W12
(/tmp workspace, additive rsync to /mnt, md5-verify, real test tails). Go at /tmp/sdk/go/bin/go,
GOPROXY=https://goproxy.cn,direct. Python: pytest available. TS: use the repo's node/npm if
present, else npx tsc --noEmit with the app's existing tsconfig; if deps cannot install, say so
and do a rigorous static review instead.

## Cross-agent contracts (bind everyone)

1. **Lead entity** (booking-service owns storage): `{lead_id uuid, tenant_id, phone_e164 text,
   channel_of_first_touch text (voice|whatsapp|telegram|web|sms|webhook|ussd|qr|promo|field),
   campaign_id uuid null, promo_code text null, utm jsonb null, lga_id int null, status text
   (new|contacted|qualified|converted|lost), consent_id uuid null, dedupe_key text unique per
   tenant, created_at, updated_at}`. `dedupe_key = sha256(tenant_id|lower(phone)|channel|YYYY-MM-DD)`
   — 24h dedup per CAC doc FR-009.
2. **Funnel event** envelope on Kafka topic `cac.events` (created in W12): CloudEvent
   `com.opendesk.cac.FunnelEvent`, data `{event_id, tenant_id, entity_type:"lead|customer|agent",
   entity_id, event_name:"lead_created|contacted|opted_in|qualified|converted|first_txn|lost",
   event_ts, channel, campaign_id, lga_id, amount_ngn null, idempotency_key}`.
3. **Attribution precedence**: explicit promo_code > UTM (utm_source/medium/campaign) >
   QR slug > channel_of_first_touch. First-touch wins; never overwritten (only `status` and
   `qualified/converted` transitions update the lead).
4. **Gold tables** (lakehouse, Iceberg — NOT Delta): `cac_gold.daily_cac_by_channel`
   `{day, tenant_id, channel, spend_ngn, leads, conversions, cac_ngn}` and
   `cac_gold.daily_cac_by_lga` `{day, tenant_id, lga_id, leads, conversions, cac_ngn, geom}`.
   Spend enters via `POST /v1/campaigns/{id}/spend` (booking-service; amount_ngn, channel, day).
5. **CAC dashboard API** (analytics-service): `GET /v1/cac/summary?from&to` →
   `{by_channel:[{channel,spend_ngn,leads,conversions,cac_ngn}], by_lga:[...], blended_cac_ngn,
   payback_days_estimate}`. Reads from analytics-service's own rollup tables (Postgres,
   RLS) fed by the `cac.events` consumer; lakehouse gold tables are the batch-verified source
   (documented dual-path: realtime rollup + nightly lakehouse reconcile).
6. **Promo codes**: booking-service table `promo_codes` `{code, tenant_id, campaign_id,
   discount_ngn null, max_redemptions, redeemed_count}` + `POST /v1/promo/redeem {code, phone}`
   (public, rate-limited, idempotent per code+phone) → creates/updates lead with attribution.
7. **Env/config**: `CAC_EVENTS_TOPIC=cac.events`, `CAC_EVENTS_GROUP=analytics-cac`,
   `LEAD_ATTRIBUTION_FIRST_TOUCH_ONLY=true`.

## Agent A — booking-service: leads + promo + campaigns spend
Owns:
- services/booking-service/internal/leads/ (NEW: model, store w/ RLS — follow store/incidents.go
  bootstrap + pg_policies pattern, service, handlers)
- services/booking-service internal wiring (ADDITIVE: server.go routes, main.go, config.go)
- services/booking-service tests: leads pkg, store, httpapi (embedded-postgres pattern)
- docs/leads-attribution.md (NEW)
Requirements: contract §1 storage (dedupe_key ON CONFLICT DO NOTHING → return existing);
CRUD + status transitions (new→contacted→qualified→converted|lost) each EMITTING contract §2
FunnelEvent via existing outbox/Kafka publisher pattern (inspect incidents service.go);
promo_codes per §6; campaigns spend endpoint per §4; all RLS'd, manage_bookings perm for
mutations, view_analytics for reads. go build/vet/test green.

## Agent B — analytics-service: cac.events consumer + CAC rollup API
Owns:
- services/analytics-service/** (additive module: cac consumer group analytics-cac per §2,
  rollup tables cac_rollup_channel/cac_rollup_lga with RLS, contract §5 REST)
- services/analytics-service tests
- docs/cac-analytics-api.md (NEW)
Requirements: idempotent consumer (idempotency_key); spend join: read campaign spend via Dapr
invoke booking `GET /internal/campaigns/{id}/spend-sum?from&to` — COORDINATE: Agent A must
expose this internal endpoint (flag in both reports if shape differs); payback_days_estimate =
blended_cac_ngn / (avg monthly gross margin per converted lead from payload amounts; null-safe).
Language: inspect analytics-service first (Go or Python) and follow its norm. Tests green.

## Agent C — lakehouse: cac_analytics.py Spark job (Iceberg)
Owns:
- infra/lakehouse/spark/jobs/cac_analytics.py (NEW — mirror geo_analytics.py structure:
  same Iceberg catalog config, same sedona registration, same CLI arg style)
- infra/lakehouse/spark/jobs/test_cac_analytics.py (NEW, py_compile + pure-function unit tests)
- docs/cac-lakehouse.md (NEW)
Requirements: read cac.events bronze → contract §4 gold tables (Iceberg, partitioned by day,
tenant-aware); LGA join via existing LGA boundary parquet/postgis export used by geo_analytics.py
(reuse its helpers); H3 res-8 cell column added for drill-down (reuse ST_H3 pattern);
idempotent overwrite partitions (INSERT OVERWRITE day partitions); document Delta→Iceberg
port mapping (cac.gold.daily_cac_by_channel → cac_gold.daily_cac_by_channel).

## Agent D — admin-web: CAC dashboards
Owns:
- apps/admin-web/app/app/[orgSlug]/cac/ (NEW pages: overview, by-channel table, by-LGA
  MapLibre choropleth, payback/LTV cards; follow analytics/ page patterns from Wave 7)
- apps/admin-web components for CAC (NEW dir components/cac/)
- nav: ADDITIVE link only in the existing nav component (surgical)
- docs/cac-dashboards.md (NEW)
Requirements: fetch contract §5 API via the app's established server-fetch + tenant header
pattern; MapLibre choropleth reuse the locations/geo-campaigns map modules (Wave 8);
Permify view_analytics gate like the analytics page; recharts or existing chart lib only —
NO new dependencies unless already in package.json; loading/empty states; mobile-responsive.
tsc --noEmit clean.

## Agent E — QR/promo landing attribution (embed + widget + landing)
Owns:
- apps/admin-web/public/embed.js (SURGICAL additive: capture URL UTM params + ?promo= +
  ?ref=qr slug on widget init → postMessage opendesk:attribution {utm, promo_code, ref})
- apps/admin-web/app/embed/ui-actions-bridge.tsx (SURGICAL additive: merge attribution into
  tapped /voice/chat bodies like client_location was)
- apps/admin-web/app/l/[slug]/route.ts (or pages equivalent — NEW minimal QR landing endpoint:
  302 to tenant booking page with utm_source=qr&utm_medium=offline&utm_campaign={slug} +
  records a funnel ping via analytics ingest; inspect app router layout first)
- docs/qr-attribution.md (NEW)
Requirements: attribution is first-touch (backend enforces; frontend merely forwards); no PII
beyond what widget already handles; keep embed.js framework-free; bridge change must not
break Wave 9 ui_actions or Wave 11 location merge (regression-test mentally + note).
tsc --noEmit clean.

## Delivery protocol: identical to SPEC-W12 §Delivery.
