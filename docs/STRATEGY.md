# Agora — Strategy: Use Cases, Monetization & Innovation Wave 5

## 1. Use cases (who this platform serves)

### Core verticals (industry packs already shipped)
| Vertical | Receptionist jobs | Why Agora wins |
|---|---|---|
| **Salons / barbers / spas** | Book cuts & treatments, deposits, stylist matching, no-show fees | Deposit policy engine + no-show fee workflow + waitlist backfill |
| **Clinics / dental / physio / mental health** | Patient intake, consent forms, triage questions, PII care | ClinicIntake workflow, PII redaction (Fluvio), self-hosted = data stays in-house |
| **Consultants / agencies / legal / accounting** | Discovery calls, scoping, proposal follow-up | Consultancy pack + CRM follow-up tasks + revenue intelligence |
| **Support desks / MSPs** | First-line triage, SLA timers, escalation | SupportEscalation workflow + warm human handoff |

### Adjacent verticals (same engine, new pack YAML)
- **Home services** (plumbing, HVAC, cleaning): emergency triage + dispatch booking
- **Restaurants**: table reservations, deposits for large parties, waitlist calls
- **Fitness / yoga studios**: class capacity booking (offerings already support capacity>1)
- **Automotive service**: MOT/repair slot booking, vehicle intake questions
- **Real estate**: viewing scheduling, lead qualification into CRM
- **Veterinary**: pet intake, vaccination reminders
- **Government / municipal services**: appointment queues, multilingual (Wave 5 #3)
- **Education**: office hours, admissions calls
- **Vets/dental chains & franchises**: multi-tenant by design — one deployment per brand, tenants per location

### Deployment shapes
- **Self-hosted SMB** (single docker host) — the open-source core
- **Multi-tenant SaaS** (agencies reselling to local businesses) — tenants + packs + white-label
- **Edge/on-prem** (clinics with data-residency needs) — k3s appliance profile + MirrorMaker store-forward
- **Embedded** — `embed.js` widget inside existing websites; LiveKit voice inside existing apps

## 2. Monetization with 3rd-party apps

Agora is open-source (Apache-2.0) — monetize the open-core way:

1. **Integration marketplace (revenue share)** — the plugin SDK (Wave 4) + pack registry (Wave 5 #6) let 3rd parties publish packs/tools (e.g. "Shopify order lookup", "HubSpot sync", "Stripe payments"). Platform takes 15–30% of paid-pack sales; free packs drive adoption.
2. **Usage-metered API (Wave 5 #9)** — gold.usage_daily tracks call-minutes, bookings, messages, AI tokens per tenant → sell API access to 3rd-party apps (scheduling aggregators, booking marketplaces) at $/1k calls with tiered rate limits (the APISIX plan-tier limits are already in place).
3. **Outbound webhooks / Zapier-style connector (Wave 5 #10)** — 3rd-party apps subscribe to booking/conversation events with HMAC-signed webhooks; premium tiers get higher event quotas + retry SLAs. Publish an official Zapier/Make app on top.
4. **Payments margin** — deposits/no-show fees flow through TigerBeetle + Mojaloop; hosted version can take 0.5–1% processing margin above rail cost.
5. **Telephony resale** — SIP trunks (Wave 5 #1) bundled per-tenant with markup on minutes; the CPS pacer is already the compliance feature carriers require.
6. **White-label / OEM for agencies** — marketing agencies deploy Agora under their brand for local-business clients; license = annual support + priority packs.
7. **Premium vertical packs** — regulated-industry packs (clinic HIPAA-style, legal intake) as paid add-ons with compliance documentation.
8. **Support & SLA** — production topology (ADR-0008), backup/restore, and upgrade runbooks as paid support contracts; trainings for self-hosters.
9. **Marketplace of AI models/personas** — tuned persona prompts + voice models per vertical (voice biometrics, whisper-copilot packs), sold per-tenant.
10. **Data/analytics add-on** — the text-to-SQL + revenue intelligence features as a paid "Insights" tier; benchmarking reports across anonymized tenants (opt-in only).

## 3. Wave 5 — 10 innovations ✅ ALL IMPLEMENTED & TESTED

| # | Innovation | Lang | Component |
|---|---|---|---|
| 1 | **SIP telephony inbound** — LiveKit SIP dispatch rules → inbound calls ring the AI receptionist; phone provisioning guide | Python/YAML | voice-agent-runtime, deploy/ |
| 2 | **Sentiment-enriched CRM notes** — avg call sentiment joins the call-quality Twenty note (closes documented gap) | Python/Go | conversation-service, crm-sync-service |
| 3 | **Multilingual receptionist** — whisper auto-language-detect → per-turn locale switch in LLM prompt + piper voice map | Python | voice-agent-runtime, packs |
| 4 | **Smart scheduling optimizer** — slot suggestions that minimize calendar fragmentation (gap-score algorithm) | Go | booking-service |
| 5 | **Resilient degraded mode** — serve-stale availability cache, Permify outage policy config, cached tenant context | Go | booking-service |
| 6 | **Pack registry & marketplace scaffold** — signed community packs, `make install-pack`, validation + docs | Python/shell | scripts/, industries/ |
| 7 | **Customer self-service portal** — magic-link (SMS/email token) → view/reschedule/cancel own bookings, no account | Go/TS | booking-service, admin-web |
| 8 | **A/B prompt testing** — digital-twin eval: two personas × scenarios, judge scores, promote winner | Python | voice-agent-runtime/eval |
| 9 | **Usage metering** — usage events → gold.usage_daily mart + metering API (monetization foundation) | Go/Python/dbt | booking, payments, analytics |
| 10 | **Outbound webhook platform** — per-tenant event subscriptions, HMAC-signed delivery, retry w/ backoff + UI | Go/TS | notification-worker, admin-web |

## 4. Recommendations closed this wave
- Telephony plane completed (SIP) — the last deferred voice-scaling item
- Sentiment in CRM notes (documented limitation)
- Monetization foundation (metering + webhooks) enabling §2 items 2–5
- Customer-facing self-service (baseline parity gap)
- Marketplace scaffolding enabling §2 item 1
