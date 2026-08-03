# SPEC-W15 — CAC Sector Packs + Showcase + i18n (CAC App, Wave 4 of 5)

Wave 15. Four builders, strict ownership. Same delivery protocol (/tmp workspace, additive
rsync, md5-verify FROM /mnt, real tails). Pack work follows the established pack system:
industries/*.yaml + scripts/validate_pack.py + industries/index.json (upsert-index subcommand,
sha256 regenerated) + identity-service packs.go passthrough. READ 2–3 existing packs first
(e.g. law-enforcement.yaml, healthcare.yaml, hospitality.yaml) and mirror the schema EXACTLY
(all required blocks: greeting, services/offerings, faqs, escalation, disclosure where
relevant, ussd menu where relevant, voice blocks where relevant).

## Cross-agent contracts (bind everyone)

1. **8 new packs** (filenames + slugs):
   - fintech-agent-banking.yaml (agent recruitment, POS float, referral ₦1,500 bounty flow)
   - ecommerce-d2c.yaml (WhatsApp-first catalog, COD, order status)
   - fmcg-retail.yaml (distributor/wholesaler lines, loyalty SMS, promo codes)
   - agritech.yaml (cooperative onboarding, USSD-first, Hausa-friendly prompts, input credit)
   - edtech.yaml (parent enrollment, school MoU follow-up, term payment reminders)
   - healthtech.yaml (clinic referral intake, NHIA-aware, teleconsult booking, disclosure REQUIRED)
   - b2b-saas.yaml (demo booking, BDR handoff, trial nurture)
   - logistics.yaml (rider recruitment, bike-inspection scheduling, referral ₦5,000 bounty)
2. Every pack MUST include: disclosure block (spokenAiDisclosure+recordingConsent+text —
   validated since W11), ussd.menu (3–6 items, actions from enum book|handoff|status|sos|info —
   validated since W12), and a `growth:` block {referral_bounty_ngn, primary_channels[],
   cac_target_ngn} (additive, free-form map — validator only checks presence+types).
3. **i18n**: every pack's user-facing strings in EN + PCM minimum; agritech also HA;
   healthcare/fintech also YO and IG greetings. Use the existing pack i18n convention
   (inspect how packs represent localized strings — if none exists, use
   `i18n: {pcm: {greeting: ...}, ha: {...}}` additive block, validator: locales from
   en|pcm|ha|yo|ig only, values non-empty strings).
4. index.json: upsert all 8 at version 1.0.0, sha256 refreshed, validate-index green;
   final total 39 packs.

## Agent A — packs 1–4
Owns: industries/fintech-agent-banking.yaml, ecommerce-d2c.yaml, fmcg-retail.yaml,
agritech.yaml (NEW). Full pack schema per existing packs + contract §2/§3. Content must
reflect the CAC playbook motions (field-rep handoff via handoff action, referral bounty
mentions in greetings/FAQs, promo-code awareness, COD for e-com, cooperative liaison for
agritech). Run validate_pack.py validate on your 4 files (green) but DO NOT touch index.json
(Agent B regenerates after both land).

## Agent B — packs 5–8 + index + validator extension
Owns: industries/edtech.yaml, healthtech.yaml, b2b-saas.yaml, logistics.yaml (NEW);
scripts/validate_pack.py (ADDITIVE: _validate_growth + _validate_i18n per contract §2/§3,
mirroring _validate_ussd style; must not break the 31+8 baseline); industries/index.json
(upsert ALL 8 packs v1.0.0 AFTER Agent A's files are present in /mnt — check first);
tests/packs/test_validate_pack.py (additive). Deliver index.json LAST (wait for A;
if A is late, deliver everything else first, then index in a follow-up rsync).
Go: identity-service go build/vet/test green (packs.go untouched unless passthrough needed
for growth/i18n — Summary passthrough: check whether Summary() passes unknown blocks;
if it drops them, add additive passthrough for ussd(already done W12)/growth/i18n —
packs.go IS yours for this purpose, additive only).

## Agent C — marketing-site CAC showcase
Owns: apps/marketing/ (ADDITIVE: new /growth (or /cac) page + section on the homepage
showcasing the CAC App: 8 sector cards (per-sector blended CAC targets from the playbook:
fintech ₦5–15k/agent, e-com ₦3.5–9.5k/customer, FMCG ₦300–900, agritech ₦4.5–9k,
edtech ₦2.5–5.5k, healthtech ₦5.5–12k, B2B SaaS ₦20–55k, logistics ₦5–9k), USSD/WhatsApp/
voice channel strip, geo-targeting + DND compliance badges, referral engine blurb, link to
demo/booking). Follow the existing marketing site design system EXACTLY (inspect first).
tsc/build verify with the marketing app's own tooling.

## Agent D — data-residency deployment + SLO docs + pack admin UX
Owns:
- docs/data-residency.md (NEW: Lagos co-lo primary + af-south-1 DR deployment guide for the
  compose stack: CBN June-2026 residency directive mapping per datastore, NDPA notes,
  MirrorMaker2/Postgres-replica/WAL-G pointers as configuration guidance — no new infra code)
- docs/slo-dashboards.md (NEW: Grafana panel list per SLO from the CAC NFRs — API p50≤300ms/
  p99≤1.5s, Kafka publish p95≤50ms, ingest availability 99.5%, financial writes 99.95%,
  CAC-by-channel refresh <30s — mapped to existing Prometheus metrics names in the repo
  (grep for _total/_seconds metrics; where a metric doesn't exist, mark "needs instrumentation"
  honestly))
- apps/admin-web/app/app/[orgSlug]/settings/packs/ (NEW or EXTEND existing pack-selection UI —
  inspect what exists first: tenant can browse the 39-pack catalog, preview greeting/ussd menu,
  activate a pack; wire to the identity packs endpoints that exist (GET /v1/packs etc. —
  inspect identity-service routes and use only real endpoints; soft-fail tolerant))
- tsc --noEmit clean for admin-web changes.

## Delivery protocol: identical to SPEC-W12 §Delivery.
