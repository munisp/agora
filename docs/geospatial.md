# Geospatial: location intelligence, map dashboards and geo-targeted campaigns

Wave 8 (SPEC-W8) adds end-to-end geospatial capability to Agora: contacts
carry locations, demand is aggregated into analytical geo tables in the
lakehouse, operators explore everything on maps, and any audience you can draw
on a map can be activated with a paced omnichannel campaign.

This document covers the architecture, a "getting started" walkthrough, and
concrete geospatial use cases for **every industry pack** in
[`industries/index.json`](../industries/index.json).

## Architecture

The geospatial stack has four layers — operational, analytical, visualization
and activation:

```
 bookings + contacts          Apache Sedona (Spark)          MapLibre (admin-web)
        │                            │                            │
        ▼                            ▼                            ▼
 ┌─────────────────┐   ┌──────────────────────────┐   ┌──────────────────────┐
 │ PostGIS (booking│   │ gold.geo_demand_h3       │   │ Locations page       │
 │  DB)            │──▶│ gold.geo_service_area_   │──▶│ Geo-campaigns page   │
 │ contact_locations│  │   coverage               │   │ (draw → preview →    │
 │ service_areas   │   │ gold.geo_hourly_density  │   │  launch)             │
 │ geo_campaigns   │   └──────────────────────────┘   └──────────────────────┘
 └─────────────────┘              │                     GeoLibre workbench
        │                         ▼                     (/gis/* + geolibre
        │                  Trino / notebooks             Jupyter package)
        ▼
 GeoCampaignWorkflow (Temporal) → NotifyPaced → WhatsApp/Telegram/SMS
        │
        ▼
 usage event: geo_campaign_message (metered/billed)
```

### Operational geo — PostGIS in booking-service

The booking database runs on `postgis/postgis:16-3.4` with three RLS-scoped
tables (same `withTenant` pattern as every sibling table):

- **`contact_locations`** — one geography(Point, 4326) per
  `(tenant_id, contact_id)`, with a `source` of `booking_address`,
  `channel_share`, `manual` or `geocode`.
- **`service_areas`** — named geography(MultiPolygon, 4326) polygons with a
  `meta` jsonb bag (e.g. delivery zones, coverage territories).
- **`geo_campaigns`** — geo-targeted campaign records: channel, message,
  target geometry, `audience_count` and a
  `draft/running/completed/failed` status lifecycle.

All geom columns carry GiST indexes. The management API (tenant-scoped, same
auth/RLS as existing `/v1` routes):

| Endpoint | Purpose |
|---|---|
| `PUT /v1/contacts/{id}/location` | Upsert a contact's point (`{lat, lng, source?}`) |
| `GET /v1/locations/summary?from=&to=&offering_id=` | Booking-joined contact points (cap 5000) + server-side `ST_SnapToGrid` clusters when > 500 points |
| `GET/POST/DELETE /v1/service-areas[/{id}]` | Manage service-area polygons (GeoJSON in/out; Polygon promoted to MultiPolygon) |
| `POST /v1/geo/audience/preview` | Count + masked sample of contacts inside a polygon or radius (`ST_Within` / `ST_DWithin`) |
| `POST/GET /v1/geo/campaigns[/{id}]` | Launch and monitor geo-targeted campaigns |

Optional geocoding: when `GEOCODE_ENABLED=true`, a booking that carries an
address string is geocoded via Nominatim (1 req/s, descriptive User-Agent, 5s
timeout, cached by address hash) and stored as a contact location with
`source=geocode`. Off by default.

### Analytical geo — Apache Sedona in the lakehouse

The Spark job `infra/lakehouse/spark/jobs/geo_analytics.py` (Sedona context
from `jobs/sedona_common.py`) reads silver bookings plus contact-location
extracts and produces three Iceberg gold tables, Trino-visible like the rest
of gold:

- **`gold.geo_demand_h3`** — bookings per H3 resolution-8 cell per tenant per
  day, with cell geometry. The cell-level demand heatmap; the base layer for
  hotspot and site-selection analysis.
- **`gold.geo_service_area_coverage`** — bookings inside vs outside each
  service area (`ST_Within` join against a service-areas extract). Measures
  whether operations match declared territories.
- **`gold.geo_hourly_density`** — demand heat cells by hour-of-week. Drives
  staffing and shift planning.

dbt gold models + docs follow the existing `infra/lakehouse/dbt` patterns.

### Visualization — MapLibre + GeoLibre

- **Admin dashboards (MapLibre GL):** the admin-web **Locations** page renders
  `/v1/locations/summary` points/clusters on an OSM basemap with date-range
  and offering filters, plus a toggleable service-area layer and
  draw-to-create polygons (`@maplibre/maplibre-gl-draw`). The **Geo-campaigns**
  page (owner/admin) lets you draw a circle or polygon, preview the audience
  live, compose a message and launch.
- **GeoLibre workbench:** `ghcr.io/opengeos/geolibre` is exposed at `/gis/*`
  via APISIX (JWT-protected) for free-form GIS exploration, and the
  `geolibre` Jupyter package
  (`infra/lakehouse/notebooks/geolibre-exploration.ipynb`) renders
  `gold.geo_demand_h3` and service areas in an analyst notebook
  (`Map(center=...)`, `add_geojson(...)`). See
  [docs/geospatial-infra.md](geospatial-infra.md) for Sedona setup, the gold
  table reference and the Trino geo query cookbook.

### Activation — geo-targeted campaigns

`POST /v1/geo/campaigns` accepts a name, channel, message (with `{name}`
personalization token) and a target (polygon GeoJSON or center + radius). It
creates a running `geo_campaigns` row and starts a Temporal
**`GeoCampaignWorkflow`** (task queue `opendesk-main`) that batches the
audience (`GEO_CAMPAIGN_BATCH`, default 50), sends through the existing paced
notification path (`NotifyPaced`, paced kind `geo_campaign`) over the tenant's
configured channels — WhatsApp, Telegram, SMS — with heartbeats and idempotent
replay (contacts already sent for a campaign id are skipped). Every recipient
emits a **`geo_campaign_message`** usage event (value=1) on
`opendesk.usage.events` via the UsageExtra outbox, so geo campaigns are
metered and billed like any other omnichannel send.

## Getting started

1. **Start the stack.** The booking database now runs PostGIS
   (`postgis/postgis:16-3.4`) and the migration creates
   `contact_locations`, `service_areas` and `geo_campaigns` automatically.
2. **(Optional) Enable geocoding.** Set `GEOCODE_ENABLED=true` on
   booking-service (and optionally override `GEOCODE_BASE_URL`, default
   `https://nominatim.openstreetmap.org`). New bookings with an address
   string will backfill contact locations with `source=geocode`.
3. **Seed demo data.** After `scripts/seed-industries.sh`, run:

   ```bash
   scripts/seed-geo.sh
   ```

   This creates two demo service areas (Lagos Island and London Zones 1–2)
   and ~50 synthetic contact locations around Lagos (6.5244, 3.3792) and
   London (51.5074, -0.1278). See the script header for env overrides.
4. **Open the maps.** In the admin dashboard, open **Locations** to see
   demand points/clusters and service areas, and **Geo campaigns** to draw an
   audience and launch a campaign. For free-form GIS work, open **`/gis/`**
   (GeoLibre workbench) or the
   `infra/lakehouse/notebooks/geolibre-exploration.ipynb` notebook to explore
   the `gold.geo_*` tables.
5. **Query the gold tables.** Via Trino:
   `SELECT * FROM gold.geo_demand_h3 WHERE tenant_id = ... ORDER BY day DESC`.

## Use cases by industry pack

Each section below lists concrete geospatial analytics use cases for one pack
in `industries/index.json`, the exact feature that powers it
(`gold.geo_demand_h3`, `gold.geo_service_area_coverage`,
`gold.geo_hourly_density`, `POST /v1/geo/audience/preview`, geo campaigns, or
service-area coverage), and a realistic example.

### salon

Terminology: services, stylists, appointments, clients.

- **Neighbourhood demand heatmap for a second chair or branch** — powered by
  `gold.geo_demand_h3`. Example: appointment cells cluster in two
  neighbourhoods 4 km apart, justifying a Saturday pop-up chair in the
  underserved one.
- **Catchment-based rebooking campaigns** — powered by geo campaigns
  (`POST /v1/geo/campaigns`). Example: draw a 2 km radius around the salon
  and send "this week only" colour-slot offers to clients who live nearby.
- **Stylist shift planning from demand by hour** — powered by
  `gold.geo_hourly_density`. Example: Friday-evening cells near the business
  district peak at 17:00–19:00, so the salon rosters an extra stylist for
  that window.

### clinic

Terminology: treatments, practitioners, visits, patients.

- **Outreach-clinic site selection** — powered by `gold.geo_demand_h3`.
  Example: physiotherapy visit demand concentrates in an eastern suburb with
  no branch, so the clinic pilots a weekly outreach session there.
- **Home-visit catchment definition** — powered by service-area coverage
  (`POST /v1/service-areas` + `gold.geo_service_area_coverage`). Example:
  define a 5 km home-visit polygon and track what share of visits fall
  outside it before widening the catchment.
- **Vaccination-drive targeting** — powered by `POST /v1/geo/audience/preview`
  + geo campaigns. Example: preview patients within 3 km of the clinic, then
  send flu-shot reminders to that audience with `{name}` personalization.

### consultancy

Terminology: engagements, consultants, sessions, prospects.

- **Workshop venue planning from prospect clusters** — powered by
  `GET /v1/locations/summary` clusters. Example: discovery-call prospects
  cluster in two cities, so the next half-day workshop is booked near the
  denser cluster.
- **On-site engagement travel zones** — powered by service-area coverage.
  Example: draw a "no-travel-fee" polygon and flag sessions outside it for a
  travel surcharge using `gold.geo_service_area_coverage`.
- **Local networking event invites** — powered by geo campaigns. Example:
  preview prospects within 25 km of a conference venue and send a paced
  invite sequence ahead of the event.

### support-desk

Terminology: support slots, agents, tickets, customers.

- **On-site troubleshooting dispatch zones** — powered by service-area
  coverage. Example: split the metro into north/south service areas and
  measure ticket coverage per zone in `gold.geo_service_area_coverage` to
  balance field agents.
- **Regional outage blast-radius messaging** — powered by
  `POST /v1/geo/audience/preview` + geo campaigns. Example: when an outage
  hits one district, preview affected customers by polygon and proactively
  send "we're on it" updates before tickets spike.
- **Follow-the-sun staffing** — powered by `gold.geo_hourly_density`.
  Example: ticket heat by hour-of-week shows a Sunday-evening peak from one
  region, so a weekend agent shift is added.

### nigeria-sme

Terminology: services, staff, appointments, customers (Lagos market, NGN).

- **Callout-zone pricing for generator repair** — powered by service-area
  coverage. Example: define Lagos Mainland and Island service areas; jobs
  outside either polygon get a callout fee, tracked via
  `gold.geo_service_area_coverage`.
- **Market-day demand hotspots** — powered by `gold.geo_demand_h3`. Example:
  braiding and tailoring bookings spike in cells around Balogun market on
  Saturdays, so a mobile stylist is scheduled there.
- **Payday promo targeting** — powered by geo campaigns. Example: send
  end-of-month haircut offers via SMS/WhatsApp to customers within 3 km of
  the shop.

### banking

Terminology: services, relationship managers, appointments, customers.

- **Branch/ATM network gap analysis** — powered by `gold.geo_demand_h3`.
  Example: loan-consultation appointment demand clusters in a district with
  no branch, feeding the business case for a new micro-branch.
- **Relationship-manager territory coverage** — powered by service-area
  coverage. Example: assign each RM a polygon territory and audit
  appointments inside vs outside via `gold.geo_service_area_coverage`.
- **BVN/NIN enrollment drives near branches** — powered by geo campaigns.
  Example: target customers within 2 km of a branch with reminders to book a
  BVN/NIN update appointment before the compliance deadline.

### insurance

- **Claim-assessment visit routing** — powered by `GET /v1/locations/summary`
  + `gold.geo_hourly_density`. Example: cluster claim-assessment visits by
  cell and hour so assessors run dense morning routes instead of
  crisscrossing the city.
- **Underwriting territory performance** — powered by
  `gold.geo_service_area_coverage`. Example: compare new-policy consultations
  inside vs outside each agent's declared territory to rebalance assignments.
- **Weather-event proactive outreach** — powered by geo campaigns. Example:
  ahead of forecast flooding, draw the flood-risk polygon and send
  claim-process and prevention guidance to policyholders inside it.

### government

- **Enrollment-centre siting** — powered by `gold.geo_demand_h3`. Example:
  national-ID enrollment appointment demand maps show long-travel cells,
  justifying a mobile enrollment unit on specific days.
- **Service-area equity audits** — powered by
  `gold.geo_service_area_coverage`. Example: verify that passport-appointment
  demand is served proportionally across district polygons, flagging
  underserved wards.
- **Deadline awareness campaigns by district** — powered by geo campaigns.
  Example: send driver's-license renewal reminders to residents within each
  district ahead of its enforcement date.

### travel

- **Shuttle route and pickup-point planning** — powered by
  `gold.geo_demand_h3`. Example: airport-shuttle booking cells reveal that
  most riders start in three hotel districts, so pickup points are fixed
  there.
- **Tour departure catchments** — powered by service-area coverage. Example:
  define hotel-corridor polygons and measure city-tour bookings per corridor
  to decide where free pickup applies.
- **Last-minute seat fill** — powered by `POST /v1/geo/audience/preview` +
  geo campaigns. Example: preview contacts within 10 km of a departure point
  and send same-day discount offers for unfilled city-tour seats.

### ecommerce

- **Delivery-slot demand heatmapping** — powered by `gold.geo_demand_h3`.
  Example: delivery-slot bookings concentrate in evening cells on the
  mainland, so an evening dispatch wave is added there.
- **Delivery-zone boundary management** — powered by service-area coverage
  (`POST /v1/service-areas` + `gold.geo_service_area_coverage`). Example:
  orders just outside the free-delivery polygon keep failing; the zone is
  redrawn using the coverage data.
- **Hyperlocal flash-sale targeting** — powered by geo campaigns. Example:
  send a same-day-delivery promo to customers within 5 km of the fulfilment
  hub.

### healthcare

- **Specialist outreach and satellite-clinic planning** — powered by
  `gold.geo_demand_h3`. Example: specialist-consultation demand from a
  peri-urban corridor crosses the threshold to justify a monthly satellite
  clinic.
- **Vaccination coverage mapping** — powered by
  `gold.geo_service_area_coverage`. Example: map vaccination appointments
  against ward polygons to find low-uptake wards for targeted outreach.
- **Screening-camp invitations** — powered by geo campaigns. Example: draw a
  radius around a planned screening camp and invite nearby patients to book
  slots, metered as `geo_campaign_message` per recipient.

### education

- **Catchment-area admissions analysis** — powered by `gold.geo_demand_h3`.
  Example: admissions-interview demand cells show a new estate feeding the
  school, informing a school-bus route.
- **Campus-tour outreach by neighbourhood** — powered by geo campaigns.
  Example: invite families within 8 km to an open day with a paced WhatsApp
  sequence and track the audience count live.
- **Bus-route coverage vs demand** — powered by service-area coverage.
  Example: overlay current bus-route polygons on
  `gold.geo_service_area_coverage` to spot student clusters outside every
  route.

### agriculture

- **Farm-visit route planning for extension officers** — powered by
  `GET /v1/locations/summary` + `gold.geo_demand_h3`. Example: agronomy
  consultations cluster along one river valley, so farm visits are batched
  into weekly valley routes.
- **Produce-collection point siting** — powered by `gold.geo_demand_h3`.
  Example: collection/delivery bookings reveal the densest farmer cells,
  where the cooperative parks its collection truck on market days.
- **Seasonal advisory broadcasts by growing zone** — powered by geo
  campaigns. Example: send planting-window and rainfall advisories to farmers
  within each agro-ecological polygon at season start.

### stock-brokerage

- **Investor-onboarding seminar placement** — powered by
  `gold.geo_demand_h3`. Example: KYC/onboarding appointment demand clusters
  in business districts, so investor-education seminars are hosted there.
- **Relationship-manager territory design** — powered by service-area
  coverage. Example: draw RM territories over client density and rebalance
  using inside/outside appointment counts from
  `gold.geo_service_area_coverage`.
- **Local market-briefing invites** — powered by geo campaigns. Example:
  invite clients within 15 km of the office to a quarterly portfolio-review
  evening.

### transportation

- **Route demand heatmapping** — powered by `gold.geo_demand_h3`. Example:
  bus-seat reservations between two district cells justify a new express
  departure.
- **Depot and ticket-office catchments** — powered by
  `gold.geo_service_area_coverage`. Example: measure group-booking demand
  inside vs outside each depot's polygon to relocate the least-used ticket
  office.
- **Charter marketing around event venues** — powered by geo campaigns.
  Example: target contacts within 20 km of a stadium with luxury-bus charter
  offers ahead of a fixture.

### entertainment

- **Event-audience origin mapping** — powered by `gold.geo_demand_h3`.
  Example: concert-ticket bookings cluster along two transit corridors,
  guiding shuttle-partnership and poster placement.
- **Venue catchment comparison** — powered by service-area coverage. Example:
  compare bookings per catchment polygon across two venues before signing a
  residency.
- **Geo-fenced ticket drops** — powered by geo campaigns. Example: announce a
  last-minute comedy-night table release to contacts within 5 km of the
  venue two hours before doors.

### fashion

Terminology: services, designers, appointments, clients.

- **Aso-ebi group-order cluster targeting** — powered by
  `gold.geo_demand_h3`. Example: group-order consultations cluster in
  wedding-season neighbourhoods, where the house runs a fitting pop-up.
- **Tailor catchment and pickup zones** — powered by service-area coverage.
  Example: define a free-measurement-visit polygon; appointments outside it
  from `gold.geo_service_area_coverage` get a courier option instead.
- **Collection-viewing invitations** — powered by geo campaigns. Example:
  invite clients within the Island polygon to a private collection viewing
  with `{name}` personalization.

### microfinance

Terminology: services, field officers, appointments, members.

- **Field-agent route planning from member density** — powered by
  `gold.geo_demand_h3`. Example: savings-collection demand per H3 cell
  defines each field officer's daily route, cutting travel time between
  members.
- **Group-meeting (Esusu/Ajo/Chama) venue siting** — powered by
  `GET /v1/locations/summary` clusters. Example: member clusters show the
  optimal neutral venue for a new group meeting.
- **Financial-literacy session outreach** — powered by geo campaigns.
  Example: target members within walking distance of a community hall for a
  literacy-session invitation.

### pharmacy

Terminology: services, pharmacists, appointments, customers.

- **Refill reminders within the delivery radius** — powered by geo campaigns
  with a center+radius target. Example: send refill-reminder enrollment
  nudges to customers within the 4 km delivery radius around the store.
- **Delivery-zone performance** — powered by service-area coverage. Example:
  track prescription pickup/delivery appointments inside vs outside the
  delivery polygon to decide whether to extend it.
- **Health-screening camp placement** — powered by `gold.geo_demand_h3`.
  Example: BP/blood-sugar screening demand cells identify the neighbourhood
  where a weekend screening camp draws the most customers.

### agri-input

Terminology: services, field officers, bookings, farmers.

- **Seasonal input-order demand mapping** — powered by
  `gold.geo_demand_h3`. Example: fertilizer-order bookings per cell show
  which villages to pre-position stock in before the planting season.
- **Field-demo and training placement** — powered by
  `GET /v1/locations/summary` clusters. Example: cluster farmer bookings to
  pick demo-farm sites that minimize average travel for attendees.
- **Restock alerts by trade area** — powered by geo campaigns. Example: when
  improved maize seed lands, notify farmers within each depot's catchment
  polygon.

### religious

Terminology: programs, ministers, bookings, members.

- **New-campus siting from member distribution** — powered by
  `gold.geo_demand_h3`. Example: weekly-service bookings from a distant
  suburb pass the threshold for a new campus or viewing centre.
- **Bus/transport route planning for services** — powered by service-area
  coverage. Example: measure member bookings per estate polygon to set
  Sunday bus pickup routes.
- **Event-hall and programme announcements by parish zone** — powered by geo
  campaigns. Example: send midweek-programme reminders to members within 3 km
  of each campus.

### logistics

Terminology: services, riders, slots, customers.

- **Failed-delivery heat cells → hub rerouting** — powered by
  `gold.geo_demand_h3`. Example: failed-delivery rescheduling slots cluster
  in two estate cells, so a micro-hub and evening rider shift are placed
  there.
- **Rider territory coverage** — powered by
  `gold.geo_service_area_coverage`. Example: same-day delivery slots inside
  vs outside each rider's polygon expose overloaded territories to
  rebalance.
- **COD confirmation call scheduling by density** — powered by
  `gold.geo_hourly_density`. Example: COD confirmation calls are batched into
  the hours when each cell's customers most often confirm.

### legal-aid

Terminology: services, paralegals, appointments, clients.

- **Outreach-clinic siting for underserved areas** — powered by
  `gold.geo_demand_h3`. Example: case-intake demand from distant cells
  justifies a monthly mobile legal-aid clinic.
- **Means-tested service coverage audits** — powered by
  `gold.geo_service_area_coverage`. Example: verify consultations are
  distributed across district polygons, flagging wards with high demand but
  low coverage for partner-lawyer referrals.
- **Know-your-rights session invitations** — powered by geo campaigns.
  Example: invite residents within a host community's polygon to a free
  paralegal session.

### utilities-payg

Terminology: services, technicians, appointments, customers.

- **Solar-installation territory planning** — powered by
  `gold.geo_demand_h3`. Example: installation demand per cell defines each
  technician crew's weekly territory and the next sales-blitz area.
- **Fault-triage dispatch zones** — powered by service-area coverage.
  Example: outage/fault-triage appointments inside vs outside dispatch
  polygons drive zone redraws to cut response time.
- **Payment-plan enrollment drives** — powered by geo campaigns. Example:
  target off-grid estates within a new service polygon with PAYG enrollment
  offers ahead of a sales visit.

### recruitment

Terminology: services, recruiters, appointments, candidates.

- **Candidate-pool mapping for employer clients** — powered by
  `gold.geo_demand_h3`. Example: intake-slot demand cells show where
  vetted candidates live, informing a client's new depot location.
- **Job-fair venue selection** — powered by `GET /v1/locations/summary`
  clusters. Example: candidate clusters identify the most central venue for
  a hiring fair.
- **Commutable-radius vacancy alerts** — powered by
  `POST /v1/geo/audience/preview` + geo campaigns. Example: preview
  candidates within 15 km of a new role's site, then send paced interview
  invitations.

### isp-installer

Terminology: services, technicians, appointments, subscribers.

- **Installation demand vs network buildout** — powered by
  `gold.geo_demand_h3`. Example: new-installation bookings cluster just
  outside the fibre footprint, prioritizing the next trenching phase.
- **Technician dispatch-zone balancing** — powered by
  `gold.geo_service_area_coverage`. Example: fault-triage appointments per
  zone polygon show one technician overloaded, so zone boundaries shift.
- **Pre-launch interest campaigns in new estates** — powered by geo
  campaigns. Example: notify subscribers inside a newly-lit estate polygon
  that installations are open for booking.

### vocational

Terminology: programs, instructors, enrollments, students.

- **Cohort-centre siting from enrollment demand** — powered by
  `gold.geo_demand_h3`. Example: JAMB/UTME prep enrollments cluster in two
  suburbs, where weekend CBT mock-exam sittings are scheduled.
- **Catchment marketing for new trade cohorts** — powered by geo campaigns.
  Example: promote a new tailoring cohort to students within 5 km of the
  training centre with a free trial-class offer.
- **Exam-day logistics by hour** — powered by `gold.geo_hourly_density`.
  Example: mock-exam sittings per cell and hour guide invigilator and
  machine allocation across centres.

### law-enforcement

Terminology: services, officers, appointments, callers. (Non-emergency
only — emergencies route to the national emergency number first.)

- **Non-emergency report hotspot mapping** — powered by
  `gold.geo_demand_h3`. Example: report-intake clusters per cell inform
  patrol-post placement and community-policing beats.
- **Statement-appointment coverage by precinct** — powered by
  `gold.geo_service_area_coverage`. Example: follow-up appointments inside
  vs outside precinct polygons reveal travel burdens, justifying a satellite
  desk.
- **Victim-support session outreach by area** — powered by geo campaigns.
  Example: after a localised incident series, invite affected-area residents
  to victim-support referral sessions.

### neighborhood-watch

Terminology: activities, coordinators, signups, residents.

- **Patrol-shift planning from incident clusters** — powered by
  `gold.geo_demand_h3` + `gold.geo_hourly_density`. Example:
  suspicious-activity reports per cell and hour-of-week set patrol-shift
  routes and timings.
- **Watch-zone membership coverage** — powered by service-area coverage.
  Example: signups inside vs outside each street polygon show zones lacking
  coordinators, prompting recruitment drives.
- **Rapid local alerts** — powered by geo campaigns. Example: after a
  break-in cluster, send a paced alert and meeting invite to residents
  within the affected blocks.

### civic-services

Terminology: report types, inspectors, inspection slots, residents.

- **Pothole cluster heat → crew dispatch priority** — powered by
  `gold.geo_demand_h3`. Example: pothole-report cells are ranked by density,
  and road crews are dispatched to the hottest cells first; a geo campaign
  then notifies affected residents of the repair window.
- **Missed-collection route auditing** — powered by
  `gold.geo_service_area_coverage`. Example: waste reports inside vs outside
  each collection-round polygon expose a systematically missed street.
- **Service-disruption notifications by ward** — powered by geo campaigns.
  Example: ahead of planned water-main work, notify residents inside the
  affected ward polygon with timings and a status-call booking link.

## Notes

- All audience previews return masked samples only (`phone_masked`); campaign
  sends go through the same consent-aware paced notification path as every
  other channel send.
- Contact locations are tenant-scoped and RLS-protected; erasing a contact
  via the GDPR endpoints removes its location row.
- Every geo-campaign recipient is metered as a `geo_campaign_message` usage
  event, so per-vertical usage shows up in the standard billing pipeline.
