# eCall / NG eCall — Video Analysis & Platform Implications

Source video: *Next Generation eCall* — Rohde & Schwarz (https://www.youtube.com/watch?v=7q7sjcrXDq8)
Scope: automotive emergency-call systems and their conformance testing — NOT an AI receptionist
product. This document extracts the transferable design patterns and maps them onto Agora.

## 1. What eCall actually is

- **eCall** (EU regulation, mandatory since 2018): an in-vehicle system that **automatically**
  calls emergency services (112) when a crash is detected (airbag/inertial sensors), or manually
  via an SOS button.
- It transmits an **MSD — Minimum Set of Data** — over the call setup: precise GNSS position,
  vehicle identification (VIN), vehicle type, timestamp, direction of travel, number of occupants,
  propulsion type (relevant for EV fires).
- It then opens a **guaranteed voice channel** between occupants and the PSAP (Public Safety
  Answering Point — the emergency call center).
- 2G/3G eCall carries the MSD in-band over the voice modem; **NG eCall (mandatory in Europe from
  2026)** moves to 4G/5G + IMS (IP Multimedia Subsystem): faster setup, richer/extensible data,
  better reliability, and a path to sensor payloads and multi-data emergency sessions.
- The video's focus: **conformance testing** — GNSS constellation simulation in the lab, TCU
  (Telematics Control Unit) protocol conformance, and **vehicle-level voice-quality testing in
  anechoic chambers** (because in an emergency, intelligibility is life-critical).

## 2. The five transferable patterns

### P1 — Structured emergency data packet (the MSD idea)
eCall's genius is that the *data travels with the call* in a rigid, machine-readable schema.
**Agora analog:** our law-enforcement / civic-services / healthcare packs currently capture
incidents conversationally. An "MSD-analog" would emit a standardized **Incident Data Packet (IDP)**
JSON for every emergency-flavored interaction:
`{incident_id, schema_version, captured_at, channel, location:{lat,lng,accuracy,source,address_text},
callback_number, incident_type, severity, people_involved, hazards[], narrative_summary,
reference_number}`.
We already own every ingredient: PostGIS `contact_locations`, conversation turns, sentiment/NER
intel, booking references.

### P2 — Automatic location capture as the FIRST priority
eCall sends location before anything else. Our emergency flows should capture and *confirm*
location first (GPS from the web widget where permitted, address elicitation on voice/WhatsApp,
cell/landmark fallback), store it as a point, and attach it to the IDP — instead of treating
location as one more intake field.

### P3 — Automatic triggering (sensor → call, no human initiation)
eCall fires without human action. Agora analog: **IoT/event-triggered outreach** — Dapr input
bindings (MQTT/Kafka topics/webhooks) from telematics, PAYG-solar inverters, fleet trackers,
panic buttons → the platform auto-initiates a voice/WhatsApp session to the affected person or a
technician ("We've detected X — confirm you're OK / book a repair"), creating the booking or IDP
automatically. This is the inverse of our current human-initiated omnichannel model and is a real
differentiator for utilities-payg, logistics, transportation, and healthcare packs.

### P4 — Guaranteed, prioritized voice path + conformance-grade quality
eCall calls get network priority and the video shows voice quality being *lab-certified*.
Agora analogs:
- **Emergency priority lane**: emergency-intent sessions skip queue/pacing, get prewarmed agent
  processes first, and trigger immediate warm-handoff to humans (we have all the primitives:
  load gating, prewarming, escalation.py — they need an `emergency` priority class).
- **Voice-quality conformance harness**: extend the eval harness with audio-quality gates —
  latency budget (mouth-to-ear), STT WER on accented speech (incl. Nigerian English/Pidgin
  corpora), TTS intelligibility spot checks, MOS-proxy scoring — run in CI like eCall's chamber
  tests. We already track SessionMetrics; this turns quality into a *release gate*.

### P5 — Standards-shaped integration with real dispatch infrastructure
eCall works because PSAPs speak a defined protocol. For our public-safety packs to be more than
intake forms, expose an **IDP submission API** (authenticated webhook out + REST in) shaped for
dispatch systems — starting with pragmatic targets: state emergency lines (112/767 in Nigeria),
municipal 311-style ticket systems, and private security/control rooms (many accept structured
webhooks today). NG eCall's IMS evolution validates SIP/IMS as the long-term rails — our SIP
inbound (Wave 5) is already on that path.

## 3. Regulatory signal

NG eCall becomes mandatory in 2026 — regulation creates markets. The same wave is coming for AI
voice agents: the **EU AI Act transparency obligation** (people must be told they're talking to an
AI) and emerging robocall/AI-disclosure rules (US FCC, Nigeria NCC). Agora should treat
**AI-disclosure-at-call-start** and **recording-consent capture** as first-class, per-pack toggles
(we have consentText — extend to spoken disclosure). Being compliance-first is a sales feature,
not overhead.

## 4. Concrete benefit assessment for Agora

| Pattern | Platform benefit | Verticals impacted | Effort |
|---|---|---|---|
| Incident Data Packet (IDP) schema + emission | Emergency reports become dispatchable artifacts, not chat logs | law-enforcement, civic-services, healthcare, transportation, logistics | Medium |
| Location-first capture + IDP attach | Higher-quality emergency intake; feeds geo analytics | all public-safety + field-service packs | Small |
| IoT/event-triggered outreach (Dapr bindings) | New product surface: zero-touch incident response; sticky B2B value | utilities-payg, logistics, transportation, healthcare, isp-installer | Medium |
| Emergency priority lane (queue/pacing bypass + instant handoff) | Safety credibility; matches eCall's priority ethos | law-enforcement, healthcare, civic | Small |
| Voice-quality conformance gates in eval CI | Measurable call quality; enterprise/procurement trust | all | Medium |
| IDP submission API for dispatch/PSAP systems | Turns packs into infrastructure authorities can consume | government, law-enforcement, civic-services | Medium |
| Spoken AI-disclosure + recording consent toggles | EU AI Act / FCC / NCC readiness; sales enabler | all | Small |

**Bottom line:** eCall is not a competitor — it's a 25-year-refined blueprint for *machine-initiated,
data-first, quality-certified emergency communication*. Agora already has ~80% of the substrate
(omnichannel, PostGIS, SIP, eval harness, public-safety packs). Adopting the five patterns converts
our public-safety packs from "AI that takes a message" into "infrastructure that dispatches help" —
and differentiates every emergency-adjacent vertical we ship.

## 5. Proposed Wave 11 backlog (if approved)

1. `docs/schemas/incident-data-packet.json` + IDP emission from conversation-service on
   emergency-intent turns (schema versioned, Kafka topic `opendesk.incidents`).
2. Location-first emergency flow in the voice runtime + widget GPS capture (consent-gated).
3. Dapr input binding `bindings.mqtt`/`bindings.incident-webhook` → auto-session initiation
   (outbound voice/WhatsApp) with IDP creation.
4. Emergency priority class: bypass CPS pacer, first claim on prewarmed agents, instant warm
   handoff; `emergency` flag in SessionMetrics.
5. Eval harness v2: latency budget, WER on Nigerian-accented/Pidgin samples, MOS-proxy gate;
   CI wiring.
6. IDP submission API (booking-service): REST + signed webhook delivery to configured dispatch
   endpoints per tenant; delivery ledger.
7. Pack schema `disclosure` block (spoken AI disclosure + recording consent per pack/jurisdiction)
   + validator/Go passthrough; default ON for law-enforcement, healthcare, civic-services.
