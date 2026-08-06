# Civic Reporting (SPEC-W32) — Citizen Intake → MDA Resolution

Citizens report issues (roads, water, power, waste, health, security…) to a government tenant; operators triage, route to the responsible MDA, track SLAs, and the citizen gets status updates. Built on the W11 incidents lifecycle — read this together with the incidents docs.

## 1. Surfaces

| Surface | URL | Auth |
|---------|-----|------|
| Citizen report form | `/p/{siteSlug}/report` | none (public) |
| Citizen status tracking | `/p/{siteSlug}/track` | none (ref + phone) |
| Public dashboard | `/p/{siteSlug}/dashboard` | none (aggregate-only) |
| Operator console | `/app/{orgSlug}/cases` | owner/admin/staff |
| Public API | `/api/civic/public/*` | none — APISIX rate-limited (60/min/IP) |
| Operator API | `/api/civic/*` | Keycloak OIDC via APISIX |

## 2. Case lifecycle

`new → triaged → assigned → in_progress → resolved → closed` (maps onto the W11 incidents statuses at the store layer). SLA deadlines (`ack_due_at`, `resolve_due_at`) come from the category and are set on triage. Breaches flip `sla_breach_ack` / `sla_breach_resolve` via the notification-worker's `CivicSLAWorkflow` timers and escalate to the MDA dispatch endpoint.

**Reference format:** `GOV-{LGA}-{WARD}-YYYY-{seq6}` (e.g. `GOV-LAG-IKD-2026-000123`) — per-tenant unique, safe to print and read aloud.

**Routing:** ward-specific rule wins; otherwise the category's default `mda_queue` (a W11 dispatch endpoint).

## 3. Operating the console

- **Triage queue:** filter by status / category / ward / SLA-breach. SLA chips run sage → amber → terracotta as deadlines approach.
- **Duplicates:** the detail drawer lists merge candidates (same category, ≤500 m, ±72 h). Merging sets `merged_into` — the canonical case carries all further notifications; the merged case's tracking page shows the pointer.
- **Anonymous reports:** reporter phone is masked in list/search/detail unless your role is owner/admin. Anonymity hides identity from operators, not from the system — status updates still work.
- **Spam:** fraud detector D8 (`report_spam`, medium severity) flags velocity abuse (>5 cases/day/phone, coordinated same-spot flooding). It never auto-quarantines — citizens are never banned from reporting; alerts are triage hints.

## 4. Citizen notifications

Transactional-class (not marketing): "Case GOV-LAG-IKD-2026-000123: now *assigned*". These bypass DND (service messages) but respect quiet hours (20:00–08:00 hold). Every attempt lands in the delivery ledger. Reporters opt in via `wants_updates` on the form (phone required).

## 5. Compliance posture

- Reporter phones hash into the graph like any contact (W28); NDPA erasure deletes the Person and their REPORTED edges — Case nodes carry no PII (ref/category/ward/status only), so the case record survives erasure as an anonymous statistic.
- Civic contacts are NOT marketing audiences: consent-gating for campaigns is unchanged and unaffected by case history.
- Public stats endpoint is aggregate-only (counts by category/ward); it cannot leak a person.

## 6. Abuse protections

Honeypot field, per-IP + per-phone throttling (10/hr, 50/day — configurable), APISIX limit-count 60/min/IP on the public route, D8 spam detection, ref+phone binding on tracking (possession, not identity).

## 7. Monetization hooks (built in, off by default)

`civic_cases` rows are the per-case metering unit for government subscriptions (deduplicated, geo-resolved, SLA-tracked cases are billable — see the monetization strategy). Category/ward aggregates feed the public dashboard and any future cross-LGA benchmark product (aggregates only, k-anonymized).

## 8. Out of scope (v1)

USSD/shortcode intake (requires the NCC VAS Content licence — see the NCC advisory; add when a state contract pays for it), photo upload pipeline (photo_url field only), multilingual portal UI, business-calendar SLAs, outbound voice status calls.

## 9. Failure modes

| Symptom | Cause | Action |
|---------|-------|--------|
| Public form 429s | throttle/APISIX limit | expected under attack; tune `CIVIC_PUBLIC_RATE_PER_HOUR`/`CIVIC_PUBLIC_RATE_PER_DAY` env or APISIX limit-count |
| SLA breach never fires | notification-worker not consuming civic topic | check `CIVIC_EVENTS_TOPIC` env + consumer group lag |
| Citizen can't track | phone mismatch | tracking binds ref+phone; anonymous reporters must use the same phone |
| Cases missing from graph | graph-sync civic topic unset | set `GRAPH_SYNC_CIVIC_TOPIC=opendesk.civic.events.v1` |
