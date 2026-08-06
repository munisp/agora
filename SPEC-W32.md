# SPEC-W32 — Civic Reporting: Citizen Intake → MDA Resolution

**Status:** contract · **Depends on:** W11 (incidents), W8 (geo/wards), W14 (fieldcapture), W28 (graph), W30 (fraud) · **Date:** 2026-08-05

## §0 Scope & rulings

1. **Extend, don't fork.** Civic cases build on the W11 incidents lifecycle (new→dispatched→acknowledged→closed) and store. Civic-specific fields (category, public reference, citizen contact, SLA deadlines, MDA routing, ward) are additive.
2. **Tenant = the government body** (LGA, state ministry, agency). Citizens are NOT users: no accounts, no login. Public intake is unauthenticated but abuse-protected; status lookup requires the case reference + the reporter's phone (possession, not identity).
3. **Every case event is a CloudEvent** on `opendesk.civic.events.v1` via the existing outbox — status notifications (WS-B), graph projection (WS-D), and analytics all consume from there.
4. **Consent posture:** civic reports are service requests, not marketing. Status notifications are transactional-class (not consent-gated like marketing), but the reporter's phone is still hashed in the graph and they can file NDPA erasure like anyone else. Marketing use of reporter contacts remains fully consent-gated (W28 rules unchanged).
5. **Media attachments:** v1 stores reporter-supplied photo as a URL field only (no object-storage pipeline in this wave). Deferred to a media wave.
6. **USSD/shortcode channels are OUT of scope** (NCC VAS Content licence prerequisite — see advisory); channels in v1: public web portal + field PWA (agents capturing on behalf of citizens) + WhatsApp inbound (existing intake, category via keyword).

## §1 Architecture

```
citizen ──> /p/[siteSlug]/report (public page)     field agent ──> PWA (civic_report kind)
    │ POST /api/civic/public/tenants/{slug}/reports        │ POST /v1/field/capture
    ▼ (APISIX public route: no OIDC, strict rate limit)    ▼ (existing pipe)
booking-service civic module ── Postgres (cases, categories, routing) ── outbox ──> opendesk.civic.events.v1
    │                                                            ├──> notification-worker: SLA timers + status SMS/WhatsApp
    │                                                            ├──> graph-sync: Case/Person/Location projection
    │                                                            └──> fraud-engine D8: report-velocity spam flags
citizen ──> /p/[siteSlug]/track (ref + phone) ──> GET /api/civic/public/tenants/{slug}/reports/{ref}
operators ──> admin-web /cases console (auth) ──> /api/civic/* (OIDC) triage/assign/merge/respond
public ──> /p/[siteSlug]/dashboard (aggregate-only stats + ward heatmap)
```

## §2 Domain model (additive to booking-service store)

**civic_categories** (per tenant): `{id, tenant_id, name, slug, mda_queue, ack_sla_hours, resolve_sla_hours, active}` — seeded defaults: roads, water, power, waste, health, security, education, environment, other. `mda_queue` = dispatch endpoint key (W11 dispatch-endpoint CRUD reused).
**civic_routing_rules** (optional overrides): `{id, tenant_id, ward?, category_id, mda_queue}` — ward-specific override wins over category default.
**civic_cases**: `{id, tenant_id, ref (unique per tenant: GOV-{LGA}-{WARD}-YYYY-{seq6}), category_id, status (new|triaged|assigned|in_progress|resolved|closed — mapped onto incidents lifecycle at the store layer), description, ward, lga, lat, lon, location_text, reporter_phone_e164?, reporter_name?, anonymous bool, photo_url?, channel (web|pwa|whatsapp), mda_queue, assigned_to?, ack_due_at, resolve_due_at, acked_at?, resolved_at?, closed_at?, merged_into?, sla_breach_ack bool, sla_breach_resolve bool, created_at, updated_at}`.
- Anonymous = phone retained for status updates but hidden from operator list views (masked; detail view reveals only to owner/admin role).
- `merged_into` — duplicate merge: case stays readable, points at canonical case; notifications follow the canonical case.

## §3 Workstreams & ownership

### WS-A — booking-service civic module (Agent A, Go)
Owns: `services/booking-service/internal/civic/**` (new), store additions, httpapi registration, fieldcapture kind extension, migration file (follow existing migration mechanism), events via outbox.
- Public endpoints (registered WITHOUT tenant auth middleware — the APISIX route supplies rate limiting; handler validates tenant slug exists):
  - `POST /v1/civic/public/tenants/{slug}/reports` — body `{category_slug, description (10..2000 chars), ward?, lat?, lon?, location_text?, reporter_phone_e164?, reporter_name?, anonymous?, photo_url?}`; validation: category exists & active; phone optional but required for status notifications (flag `wants_updates`); honeypot field `website` (must be empty); per-IP+per-phone throttling in-memory (10/hr, 50/day, configurable); returns `{ref, ack_due_at}`. Emits `com.opendesk.civic.ReportReceived`.
  - `GET /v1/civic/public/tenants/{slug}/reports/{ref}?phone=` — ref+phone must match (or anonymous lookup with phone); returns `{ref, category, status, ward, created_at, acked_at, resolved_at, mda_queue}` — NO operator notes, NO other cases.
  - `GET /v1/civic/public/tenants/{slug}/categories` — active categories for the intake form.
  - `GET /v1/civic/public/tenants/{slug}/stats` — aggregate-only: open/resolved counts by category and ward (no person data), for the public dashboard.
- Operator endpoints (tenant auth + manage_bookings perm, same pattern as incidents routes):
  - `GET /v1/civic/cases?status=&category=&ward=&sla_breach=&q=` (list with SLA countdown fields), `GET /v1/civic/cases/{id}` (detail incl. reporter; masked unless role owner/admin), `POST /v1/civic/cases/{id}/triage {category_id?, ward?, mda_queue?}` (→ status triaged), `POST /v1/civic/cases/{id}/assign {assignee}` (→ assigned; emits StatusChanged), `POST /v1/civic/cases/{id}/status {status: in_progress|resolved|closed, note?}`, `POST /v1/civic/cases/{id}/merge {canonical_id}` (sets merged_into; emits Merged), `GET /v1/civic/cases/{id}/duplicates` (geo ≤500m + same category + ±72h candidates), category CRUD `GET/POST /v1/civic/categories`, `PATCH /v1/civic/categories/{id}`, routing rules CRUD.
  - On triage/assign: compute `ack_due_at`/`resolve_due_at` from category SLAs; emits `com.opendesk.civic.StatusChanged` (data: ref, status, tenantid ext, reporter_phone if wants_updates).
- fieldcapture: kind enum gains `civic_report` — payload = same shape as public report body; flows through the existing capture pipe, creates a civic case with channel=pwa (agent attribution via existing device context).
- Events: all on `opendesk.civic.events.v1` via outbox, tenantid extension, id `tenant:civic:ref:{seq}`.
- Tests (Go, ≥20): validation matrix, throttling, honeypot, ref format/uniqueness, routing precedence (ward override > category default), SLA due computation, merge semantics (status of merged case, notification follows canonical), tracking auth (ref+phone match/mismatch), stats aggregate-only (no phone leakage), masking by role, civic_report capture kind.

### WS-B — notification-worker SLA + status (Agent B, Go)
Owns: new files under `services/notification-worker/internal/` (activities + workflows + consumer wiring for the civic topic).
- Consume `opendesk.civic.events.v1`:
  - ReportReceived → start `CivicSLAWorkflow{case_ref, tenant, ack_due_at, resolve_due_at}` (deterministic workflow ID `civic-sla-{tenant}-{ref}`, continue-as-new safe).
  - StatusChanged → (a) cancel/satisfy pending SLA timers per new status; (b) if reporter wants updates: transactional-class notification ("Case {ref}: now {status}") via the EXISTING paced-send path (DND/quiet-hours apply to promotional only; transactional civic updates bypass DND but respect quiet hours 20:00–08:00 except status=resolved-for-security-category? No — keep simple: quiet hours apply, delivery ledger records everything).
- SLA timers: on ack_due_at with no ack → emit escalation event + notify mda_queue dispatch endpoint (W11 delivery path); on resolve_due_at unresolved → escalate + set `sla_breach_resolve` via internal callback to booking-service (`POST /v1/civic/internal/cases/{ref}/sla-breach {kind, notify_mda, mda_queue}` — internal route, X-Tenant-Slug; when `notify_mda:true`, booking-service synthesizes a deterministic W11 `civic_sla_breach` incident and dispatches signed webhooks to the tenant's active dispatch endpoints via the W11 delivery path + `incident_deliveries` ledger, idempotent on replay).
- Tests (Go, ≥12): timer fires on due, cancels on ack, escalation delivery recorded, quiet-hours hold, merged-case notification follows canonical, workflow ID determinism.

### WS-C — admin-web operator console + public pages (Agent C, TypeScript)
Owns ONLY:
- NEW `app/app/[orgSlug]/cases/{page.tsx, cases-client.tsx}` + `components/cases/{cases-table.tsx, case-detail.tsx, category-config.tsx, types.ts}` — triage queue (filters: status/category/ward/SLA-breach; SLA countdown chips sage→amber→terracotta; bulk assign), detail drawer (map pin from lat/lon, reporter masked per role, category/MDA edit, assign, status actions with note, merge-duplicate picker fed by /duplicates, timeline from events), category & routing config tab. Gated owner/admin/staff (cases are operational; same gate as locations).
- NEW `app/p/[siteSlug]/report/{page.tsx, report-client.tsx}` — PUBLIC intake: category select (from public categories endpoint), description, ward dropdown (from existing geo data), optional GPS ("use my location"), optional name/phone with wants_updates checkbox, anonymous toggle, honeypot, success screen showing the big REF + "save this number" + link to track. No auth. Brand tokens, mobile-first.
- NEW `app/p/[siteSlug]/track/{page.tsx, track-client.tsx}` — ref + phone form → status timeline (received → triaged → assigned → in progress → resolved) with due dates.
- NEW `app/p/[siteSlug]/dashboard/{page.tsx, dashboard-client.tsx}` — public aggregate stats: open vs resolved, avg resolution days by category, ward heat table (no map lib needed — table + bars fine). Consumes public stats endpoint only.
- Nav: operator item `{segment: "cases", label: "Cases", icon: FileWarning}` — **orchestrator wires org-nav**; Agent C leaves `// NAV:` marker.
- Validation: `npx tsc --noEmit` green.

### WS-D — graph projection + spam detector (Agent D)
Owns: graph-sync consumer additions (Go) + fraud-engine D8 (Python).
- graph-sync: consume `opendesk.civic.events.v1` (env GRAPH_SYNC_CIVIC_TOPIC, empty=skip) → `(cs:Case {case_id: ref, tenant_id, category, status, ward, created_at})`, `(p:Person)-[:REPORTED]->(cs)` (person resolved via existing phone-hash path when reporter phone present), `(cs)-[:AT]->(Location)` when geo present, status mirrored on StatusChanged, merged cases get `(cs)-[:MERGED_INTO]->(canonical)`. Erasure: W28 person-erasure already DETACH-deletes the Person; REPORTED edges die with it; Case nodes keep no PII (ref + category + ward only). Tests (Go, ≥8).
- fraud-engine D8 `report_spam`: same phone_hash (via HAS_CONTACT→Case REPORTED chains) opening > `CIVIC_REPORT_MAX_PER_DAY` (5) cases/day, or >3 open cases same category within 500m/24h across reporters (coordinated spam) → Alert type `report_spam`, severity medium (never auto-quarantines — citizens aren't banned from reporting; alerts inform operator triage). Env-tunable. Tests (Python, ≥6).

### Integrator (orchestrator)
- APISIX: public route `api-civic-public` (uris `/api/civic/public/*`, NO openid-connect, limit-req 20r/s burst 40 per IP, proxy-rewrite to booking-service) + authenticated route `api-civic` (`/api/civic/*`, OIDC, proxy-rewrite `/api/civic/?(.*)` → `/v1/civic/$1`). Check existing api-graph route as template.
- create-topics.sh: + `opendesk.civic.events.v1`.
- docs/civic.md: operator runbook (categories, SLAs, merge SOP, breach handling), public-page URLs, abuse posture, NCC note (USSD requires VAS Content licence — out of scope), monetization hooks (per-case metering fields present).
- tests/e2e/test_civic_wave.py (≥8, live-compose style): public report → ref returned → appears in operator list → triage/assign → SLA fields set → citizen track shows status → resolve → citizen notification ledger entry → stats endpoint reflects counts → spam throttle returns 429 → tracking with wrong phone 404s.
- org-nav.tsx: Cases item (FileWarning icon) + gate.

## §4 Quality gates
1. Public routes NEVER expose other tenants' or other citizens' data (ref+phone binding on track; stats aggregate-only; test proves no phone leakage in /stats).
2. SLA math correct across midnight/weekend (plain wall-clock hours, no business-calendar v1).
3. Merged cases: canonical carries notifications; merged case returns 200 with `merged_into` pointer on track.
4. Anonymous masking enforced by role on every operator endpoint (list + detail + search).
5. All events carry tenantid extension; graph projection tenant-scoped; Case nodes PII-free.
6. Public intake throttling + honeypot active; civic spam never auto-quarantines (D8 medium only).

## §5 Exclusions
No USSD/shortcode (licence-gated), no photo upload pipeline (URL field only), no multilingual portal UI (English v1; voice agent covers local languages), no business-calendar SLAs, no outbound voice status calls, no cross-tenant government benchmarks (W31 R4 covers that later).

## §6 Acceptance
WS tests green; e2e green; integrator wiring complete; independent gate PASS; blob-SHA push; full-tree audit clean.
