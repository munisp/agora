# W45 deferred register — honest roadmap for L-size items

Everything audit-sized in W45 shipped **fixed** this wave. The items below
are deliberately **documented-not-built**: each is L-size (multi-week),
cross-cutting, or gated by an external process. Each entry states what
exists today (so nobody mistakes the gap for a bug), the rationale for
deferral, and an implementation sketch.

| # | Item | Tracking | Foundation shipped |
|---|---|---|---|
| 1 | Franchise hierarchy | G15 | tenant model is flat by design |
| 2 | Multi-currency settlement | G36 | NGN-only guard documented |
| 3 | FIRS e-invoicing | G37 | `tax_bps` on rate cards/invoice lines (K18) |
| 4 | SSO / SCIM | G29/G30 | realm roles + groups model |
| 5 | Full 2FA realm policy | G31 | realm hardening (W34 GF5) |
| 6 | Cross-tenant customer identity | G46 | per-tenant contacts + dedupe |
| 7 | Outbound voice campaigns | U4 | inbound voice agent only |
| 8 | Marketplace pack signing | O17 | pack validator + catalog |
| 9 | Per-topic bus ACLs | OOS-19 | topics enumerated + contracts |
| 10 | Per-tenant LLM cost quotas | OOS-16 | rate limiting + usage metering |
| 11 | Recurring bookings / subscriptions | G4 | booking schema note only |

## 1. Franchise hierarchy (G15)

**Today:** tenants are flat; a franchise group is N independent tenants.
**Rationale:** touches identity (tenant parent/child), Permify schema
(inheritance), billing rollup, and every tenant-scoped query — an L-size
data-model change, unsafe to bolt on inside a hardening wave.
**Sketch:** `tenants.parent_id` + materialized path; Permify
`organization` gains a `parent` relation with recursive checks; rollups via
a read model (franchise dashboard) rather than cross-tenant queries;
booking stays leaf-scoped.

## 2. Multi-currency settlement (G36)

**Today:** money is kobo/NGN-centric (payments ledger amounts are minor
units; `currency` is a field, not a settlement rail). The NGN-only posture
is documented where amounts are handled; Flutterwave is the single rail.
**Rationale:** real multi-currency needs FX rates at charge time, ledger
accounts per currency, and settlement reconciliation — a payments redesign.
**Sketch:** currency-scoped ledger accounts (TigerBeetle ledger ids per
currency), rate capture on charge (`fx_rate`, `settled_amount`),
tenant settlement currency on the rate card, Flutterwave multi-currency
or a second rail.

## 3. FIRS e-invoicing (G37)

**Today:** invoices carry `tax_bps` (default 0, copied from `rate_cards` —
K18), which is the VAT-ready foundation; there is no FIRS submission.
**Rationale:** FIRS e-invoicing requires accreditation, their schema
(BIS Billing 3.0 / UBL), signing, and an integration account — an external
process plus a new service surface.
**Sketch:** invoice → UBL mapping in billing-engine, signing key in the
secrets manager, submission outbox (same durable pattern as
`billing_events`), status webhook consumer; `tax_bps` already feeds the
tax total.

## 4. SSO / SCIM (G29/G30)

**Today:** Keycloak realm `opendesk` with local users; enterprise IdP
federation and SCIM provisioning are not wired.
**Rationale:** per-customer IdP config is a tenant-level feature (brokering
per tenant, claim mapping, group sync) plus SCIM endpoints — L-size and
customer-driven.
**Sketch:** Keycloak identity providers per enterprise tenant (SAML/OIDC),
`tenant_slugs` group mapping via IdP group claims; SCIM 2.0 via Keycloak's
SCIM extension or a thin provisioning adapter in identity-service.

## 5. Full 2FA realm policy (G31)

**Today:** realm hardened in W34 GF5 (sslRequired, brute-force protection,
refresh-token revocation); no mandatory OTP policy.
**Rationale:** enforcing OTP realm-wide is a UX/policy decision (which
roles, recovery flow, support runbook) more than code.
**Sketch:** realm authentication flow with conditional OTP (role-based:
platform-admin + owner first), recovery codes, runbook entry in
`docs/runbooks/security.md` when enabled.

## 6. Cross-tenant customer identity (G46)

**Today:** contacts are strictly per-tenant (`UNIQUE(tenant_id, phone)`
dedupe shipped in K11 work); the same human across two tenants is two
contacts.
**Rationale:** a global customer graph raises consent/NDPA questions
(cross-tenant data sharing) that need a product decision before code.
**Sketch:** optional global identity resolution service keyed on consented
phone hash (W28 scheme), per-tenant projection stays authoritative, link
only with explicit consent records.

## 7. Outbound voice campaigns (U4)

**Today:** the voice agent is **inbound + text** only (LiveKit rooms,
`/voice/chat`); there is no outbound dialer, campaign scheduler, or
answer-machine detection. Any doc implying outbound campaigns exist is
wrong — this register is the authoritative status.
**Rationale:** outbound needs a telephony origination provider, per-market
calling-hour/consent compliance (NCC/NDPA), retry/pacing, and campaign UX
— L-size with legal review.
**Sketch:** notification-worker pacer pattern reused for call pacing;
SIP origination via the telephony provider; campaign model mirroring
campaign-studio journeys; consent gate on the contact record before any
dial.

## 8. Marketplace pack signing (O17)

**Today:** industry packs are validated (`scripts/validate_pack.py`,
`make validate-packs`) and installed from the repo — unsigned; there is no
third-party marketplace trust story.
**Rationale:** signing needs a key-management story (release keys,
revocation) and distribution format decisions.
**Sketch:** Sigstore/cosign-style detached signatures over the pack
manifest, `install-pack.sh` verifies against a pinned public key,
`--allow-unsigned` dev escape logged loudly.

## 9. Per-topic bus ACLs (OOS-19)

**Today:** all services share one Kafka cluster with full produce/consume
rights; the topic inventory and contracts are now enumerated
(`docs/event-contracts.md`, `infra/kafka/create-topics.sh`) — that
enumeration plus consumer allowlists where they already exist is the
interim control.
**Rationale:** Kafka ACLs require principal management for every service
(and Dapr sidecar scoping) — an infra-wide change with high blast radius.
**Sketch:** SASL/SCRAM principal per service, ACLs derived mechanically
from the producer/consumer tables in `docs/event-contracts.md`, staged
`permissive → enforced` rollout.

## 10. Per-tenant LLM cost quotas (OOS-16)

**Today:** LLM calls are rate-limited and metered (`opendesk.usage.events`
`UsageRecord` covers tokens/call-minutes — see analytics), so abuse is
bounded and visible; there is no hard per-tenant cost quota/cessation.
**Rationale:** quotas need a budget model (per plan?), a fast check in the
hot path, and a tenant-facing "quota exhausted" UX — product decisions
first.
**Sketch:** budget table + sliding-window usage read model fed by
`UsageRecord`, pre-call check in the voice/conversation LLM path with
graceful degradation (concierge fallback), admin-web budget page.

## 11. Recurring bookings / subscriptions (G4)

**Today:** bookings are single-shot; the schema carries no recurrence
fields (schema note only).
**Rationale:** recurrence touches availability search, reminders, no-show
handling, and payments (subscriptions) — L-size.
**Sketch:** `recurrence_rule` (RRULE) on a series entity expanding into
concrete bookings N weeks ahead; availability treats expanded instances as
normal bookings; subscription billing via billing-engine recurring
invoices.
