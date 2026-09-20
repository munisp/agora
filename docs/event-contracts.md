# Event contracts — public tenant & operator event streams

This document is the **public contract** for the platform's Kafka event
streams, including the topics that previously had a producer but no in-repo
consumer (ORPH O7/O8/O9/O10, resolved **DOCUMENT-AS-EXTERNAL**: the streams
below are stable, versioned contracts and external consumers — tenant
integrations, operators, analytics — are welcome).

## Envelope & transport (all streams)

- **CloudEvents 1.0** JSON envelope:
  `{specversion, id, source, type, subject, time, tenantid, data}`.
  `tenantid` is the CloudEvents tenant extension; `subject` is the entity id
  (or tenant slug for tenant-scoped lifecycle events).
- **Kafka**: 6 partitions, RF 1 in dev (`infra/kafka/create-topics.sh`;
  broker auto-create is OFF — a topic not declared there does not exist).
  Raise RF in prod.
- **Retention**: broker default (`log.retention.hours`, Bitnami default
  **7 days**) — no per-topic overrides are set today. Topics are a delivery
  mechanism, **not durable storage**; consumers that need history must
  persist it. Operators should set explicit per-topic retention in prod
  (longer for the audit/erasure topics, §5).
- **Delivery**: at-least-once. Consumers MUST be idempotent; event `id`s are
  deterministic where replays are expected (e.g. billing outbox relay,
  graph erasure re-drives), so keying dedupe on `id` is safe.
- **PII**: events carry ids and operational metadata, not free-text PII
  (W28); phone numbers appear hashed where they appear at all. Post bodies
  never travel on the social stream (§3).
- **Evolution**: additive fields only within a `.v1` topic; breaking changes
  ship as a new topic version (`…​.v2`).

## 1. `opendesk.identity.events` — identity lifecycle

Producer: identity-service (Dapr `pubsub-kafka`; env `IDENTITY_EVENTS_TOPIC`).
Authoritative reference: `services/identity-service/README.md` §"Event
stream contracts". Types (all `com.opendesk.identity.*`):

| Type | When | `data` payload |
|---|---|---|
| `TenantProvisioned` | `POST /v1/tenants` commits | `{tenant_id, slug, name, plan, industry}` |
| `MemberInvited` | member invited (K8) | `{tenant_slug, email, display_name, role, invited_by, invite_ts}` |
| `TenantDeleted` | tenant deleted (K9) | `{tenant_slug, tenant_id, deleted_at, actor}` |
| `TenantPlanChanged` | plan PATCH commits | `{tenant_slug, tenant_id, old_plan, new_plan, actor}` |
| `MemberRemoved` | member deleted (K16) | `{tenant_slug, user_id, actor, removed_at}` |
| `MemberRoleChanged` | member role PATCH (K16) | `{tenant_slug, user_id, old_role, new_role, actor}` |

Known consumers: notification-worker (`MemberInvited` → invite email, K8);
booking-service, conversation-service, graph-sync, crm-sync (`TenantDeleted`
purge cascade, K9 — idempotent, best-effort with error surfacing).

> Comment drift (harmless, reported): `infra/kafka/create-topics.sh` lists
> this topic's comment as `TenantProvisioned, MemberInvited, RoleChanged` —
> the authoritative type list is the table above (the file is owned by
> another workstream and intentionally not edited here).

## 2. `opendesk.apps.lifecycle.v1` — app entitlement lifecycle (ORPH O9)

Producer: identity-service (`internal/apps/publisher.go`; env
`APPS_LIFECYCLE_TOPIC`). Payload `{tenant_id, app_id, status, actor, ts}`:

| Type | When |
|---|---|
| `com.opendesk.apps.AppProvisioned` | first provision of an app for a tenant |
| `com.opendesk.apps.AppStatusChanged` | enable / disable / suspend transitions, incl. re-enable and DELETE (an enabled→enabled replay publishes nothing) |

Consumers: welcome (portal/app-catalog, entitlement caches, audit).

> Comment drift (harmless, reported): `infra/kafka/create-topics.sh:32`
> comments this topic as `AppProvisioned/Enabled/Disabled/Suspended`; the
> emitted types are exactly the two above (the enable/disable/suspend
> transitions are the `status` field of `AppStatusChanged`). Code is the
> source of truth; the infra file is owned by another workstream.

## 3. App lifecycle streams from booking-service (ORPH O7)

Producer: booking-service domain packages via the **transactional outbox**
(event rows commit with the domain write; a relay publishes). Topic env
overrides exist per domain (`<DOMAIN>_EVENTS_TOPIC`; empty disables
emission). All types are `com.opendesk.<domain>.<Type>`:

| Topic | Types | Key `data` fields |
|---|---|---|
| `opendesk.helpdesk.events.v1` | `helpdesk.TicketEvent` | `event_name` ∈ `ticket_created`/`ticket_resolved`, `tenant_id`, `ticket_id`, `subject`, `channel`, `priority`, `status`, optional `contact_id`/`conversation_id`/`assignee_id`/`resolved_at` |
| `opendesk.fsm.events.v1` | `fsm.WorkOrderAssigned`, `fsm.WorkOrderCompleted` | work-order id, assignee, status timestamps (package `internal/workorders`) |
| `opendesk.loyalty.events.v1` | `loyalty.PointsIssued`, `loyalty.PointsRedeemed` | contact id, points, reason/booking ref |
| `opendesk.studio.events.v1` | `studio.JourneyEnrolled`, `studio.JourneyCompleted` | journey/enrollment ids, contact id |
| `opendesk.crm.events.v1` | `crm.NoteCreated`, `crm.NoteUpdated`, `crm.TagAdded`, `crm.TagRemoved` | note/tag ids, entity refs (package `internal/crm360`) |
| `opendesk.surveys.events.v1` | `surveys.InviteSent`, `surveys.ResponseReceived` | survey/response ids, channel |
| `opendesk.workforce.events.v1` | `workforce.ShiftAssigned`, `workforce.LeaveDecided` | shift/leave ids, team-member id, decision |
| `opendesk.social.events.v1` | `social.PostPublished`, `social.AdLaunched`, `social.AdRejected` | post/ad ids, provider, status/reason. **Privacy contract: the post `Body` never appears in events or metering payloads** (`internal/socialpub/provider`). |

These were producer-only until this wave: they are now declared public.
Consumers-welcome examples: analytics rollups, tenant webhooks, audit
archives. Field-level detail: the `events.go` of each package under
`services/booking-service/internal/`.

## 4. `opendesk.billing.events` — billing engine (ORPH O8)

Producer: billing-engine (`BILLING_EVENTS_TOPIC`, default
`opendesk.billing.events`) via a **durable outbox**: the event row is
INSERTed in the same transaction as the invoice transition (RS-001), and the
relay republishes with backoff until Kafka accepts it — publication failure
never rolls back the money transition and is visible via
`billing_events_published_total` / `billing_events_failed_total` on
`/metrics`.

| Type | `data` payload |
|---|---|
| `com.opendesk.billing.InvoicePaid` | `{invoiceId, tenantId, period, subtotalCents, currency, paymentRef, paystackReference}` |
| `com.opendesk.billing.InvoiceVoided` | `{invoiceId, tenantId, period, previousStatus, subtotalCents, currency}` |

Consumer (SPEC-W45): **notification-worker** consumes both types to send the
paid receipt / void notice emails. Recipient contract: invoice email is
addressed to the tenant contact's **`billing_email`** field (contacts carry
an optional `billing_email` for billing correspondence); a contact without
one receives no email — the skip is logged, never silently treated as sent.

## 5. `opendesk.graph.erasure.done.v1` — graph erasure audit (ORPH O10)

Producer: graph-sync (`GRAPH_ERASURE_DONE_TOPIC`), after applying a consent
erasure tombstone (`ErasureRequested`) to the tenant subgraph (`DETACH
DELETE`, idempotent). Type `com.opendesk.graph.ErasureDone`, `data`
`{person_id, found, erasure_event_id, tenant_id}`; event id is derived from
the source erasure event (`graph-erasure-done-<src id>`) so redelivery is
safe.

**Audit sink by design**: nothing in the platform is required to consume
this topic — it is the durable evidence trail that a GDPR erasure reached
the graph. Compliance/audit consumers are welcome. Operators SHOULD give
this topic a long retention in prod (it is an audit record, §"Retention").

## 6. Waitlist claim — public booking-widget contract (ORPH O17)

The booking waitlist endpoints (`services/booking-service`
`internal/httpapi/server.go`) are declared the **public contract for
booking widgets / claim pages** (DOCUMENT-AS-EXTERNAL resolution; their
in-repo caller is the admin-web claim page, SPEC-W45 K13):

| Endpoint | Auth | Purpose |
|---|---|---|
| `POST /v1/waitlist` | staff JWT, Permify `manage_bookings` | create a waitlist entry (staff backfill workflow) |
| `GET /v1/waitlist` | staff JWT | list entries |
| `POST /v1/waitlist/{id}/claim` | **token capability, not Permify** — the claimant is an end customer following the backfill link | claim an offered slot |
| `GET /p/{slug}/claim?token=…` | public (admin-web) | claim page that validates via the endpoints above and confirms the slot |

Integrators embedding a booking widget may rely on the same claim-link
shape: the token in the link is the entire authorization for one claim.
