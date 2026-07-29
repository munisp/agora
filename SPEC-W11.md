# SPEC-W11 — Emergency-Grade Communication: IDP, Priority Lane, IoT Triggers, Disclosure, Quality Gates

Wave 11 contract (implements docs/ECALL-ANALYSIS.md backlog). Repo: `/mnt/agents/output/opendesk`
(flaky FUSE — work in `/tmp`, rsync ADDITIVELY, md5-verify).

## Canonical IDP schema (all agents code against this EXACT shape)

`Incident Data Packet` (JSON, schema doc owned by Agent A at `docs/schemas/incident-data-packet.json`):
```json
{
  "incident_id": "uuid",
  "schema_version": "1.0",
  "tenant_id": "uuid",
  "captured_at": "rfc3339",
  "channel": "voice|whatsapp|telegram|web|sms|webhook",
  "location": {"lat": 0.0, "lng": 0.0, "accuracy_m": 0, "source": "gps|address|caller_id|manual", "address_text": ""} | null,
  "callback_number": "+234…|null",
  "incident_type": "crime|medical|fire|crash|utility_fault|security|other",
  "severity": "critical|high|medium|low",
  "people_involved": 0,
  "hazards": ["weapons","injuries","fire","gas","traffic"],
  "narrative_summary": "≤500 chars",
  "reference_number": "tenant-facing ref (e.g. INC-2026-000123)",
  "contact_id": "uuid|null",
  "conversation_id": "uuid|null"
}
```

OWNERSHIP (collision-critical):
- **Agent A** (Python, conversation-service): `docs/schemas/incident-data-packet.json`,
  `services/conversation-service/app/incidents.py` (new), `app/intel.py` (surgical additive),
  `app/config.py` (conversation-service's own config, additive), `app/routes.py` (only if needed for
  turn-ingest hook — prefer wiring inside existing turn persistence), conversation-service tests.
- **Agent B** (Go): `services/booking-service/internal/incidents/**` + store + httpapi/server.go +
  temporalclient + cmd/server/main.go; `services/notification-worker` (paced kind `incident_alert` +
  pacer priority fast-lane); `services/messaging-gateway/internal/httpapi/webhooks.go` (new
  `/webhooks/incidents` route); `infra/kafka/create-topics.sh` (add opendesk.incidents);
  `infra/dapr/components/bindings.incident-webhook.yaml` (new); `docker-compose.yml` (ONLY if new
  env needed — sole compose editor this wave).
- **Agent C** (Python, voice-agent-runtime): `app/emergency.py` (new), `app/escalation.py`,
  `app/metrics.py` (additive), `app/livekit_worker.py`, `app/prompts.py`, `app/tools.py`
  (capture_location tool only), runtime tests. NOT chat.py, config.py, control_plane.py.
- **Agent D** (packs + widget): `scripts/validate_pack.py`, `services/identity-service/internal/packs/packs.go`,
  `industries/{law-enforcement,healthcare,civic-services}.yaml`, `industries/index.json`,
  `apps/admin-web/public/embed.js`, `apps/admin-web/app/embed/ui-actions-bridge.tsx` (GPS bridge only),
  `docs/incidents.md`.
- **Agent E** (eval): `services/voice-agent-runtime/eval/**`, `scripts/eval-quality-gate.sh` (new),
  `docs/eval-quality-gates.md`.

## Part A — IDP emission (conversation-service)

1. `docs/schemas/incident-data-packet.json` — JSON Schema (draft 2020-12) for the canonical shape above.
2. `app/incidents.py`: emergency-intent classifier (lexicon-first, reusing intel.py patterns: EN +
   PCM keywords — "emergency", "help me", "fire", "armed robbery", "accident", "thief dey", "blood",
   "dying", "kidnap", "rape", "collapse"…, severity weighting, hazard extraction); `build_idp(turn_ctx)`
   assembling the IDP from the turn's conversation + contact + channel; `emit_idp()` producing a
   CloudEvent `com.opendesk.incidents.IDPCreated` to Kafka topic `opendesk.incidents` via the
   service's existing producer pattern (check how other events are emitted in this service; if none,
   follow booking-service's outbox style — but prefer the simplest existing in-service pattern).
3. Wire into turn persistence: when a user turn in a conversation classified emergency (score
   threshold `INCIDENT_MIN_SCORE`, env) → build + emit IDP (idempotent per conversation+turn via
   incident dedupe key), non-blocking, failures logged never raised.
4. Reference number: `INC-{YYYY}-{seq:06d}` per tenant (existing redis or sequence pattern).
5. Tests: classifier scoring (EN+PCM), hazard extraction, IDP shape vs schema (jsonschema validate),
   threshold gating, idempotency.

## Part B — Incidents API + dispatch + IoT trigger (Go)

1. Migration `incidents` table (booking DB, RLS): `id uuid pk, tenant_id, reference_number,
   incident_type, severity, payload jsonb (full IDP), status text check in
   ('new','dispatched','acknowledged','closed') default 'new', created_at, dispatched_at` +
   `incident_deliveries(id uuid pk, tenant_id, incident_id, endpoint_url, status, attempts,
   last_status_code, last_error, created_at, delivered_at)` + `incident_dispatch_endpoints
   (tenant_id, url, secret, active, created_at, pk(tenant_id,url))`.
2. Consumer: Kafka consumer group `booking-incidents` on `opendesk.incidents` → persist payload
   (idempotent on incident_id) following the service's existing consumer pattern.
3. REST (existing auth/RLS patterns): `GET /v1/incidents?status=&from=&to=` (admin list),
   `GET /v1/incidents/{id}`, `POST /v1/incidents/{id}/dispatch` → delivers to all active tenant
   endpoints, `POST /v1/incidents/dispatch-endpoints` / `GET` / `DELETE` (manage_bookings role).
4. Signed delivery: POST IDP JSON to endpoint with `X-OpenDesk-Signature: HMAC-SHA256(secret, body)`
   + `X-OpenDesk-Incident: {id}`; per-endpoint delivery ledger row; retry via the EXISTING outbound
   webhook delivery Temporal workflow pattern from Wave 5 (reuse it — find it; add payload type
   "incident"); auto-dispatch on incident creation when tenant has active endpoints (env
   INCIDENT_AUTO_DISPATCH=true default).
5. Auto-outreach: when an incident carries callback_number/contact_id and severity in
   (critical, high) → NotifyPaced kind `incident_alert` (new paced kind, notification-worker:
   sms/whatsapp per tenant channel routing, message template from pack/incident payload) — BUT the
   pacer gets a **priority fast-lane**: `incident_alert` bypasses the CPS token bucket (direct
   dispatch) while staying metered. Implement as a `priority bool` on the paced request.
6. messaging-gateway `POST /webhooks/incidents` (public, APISIX-covered by the existing /webhooks/*
   route): body `{tenant_slug or tenant_id, secret?, incident: <IDP-ish partial>}` → validate
   minimal fields → Dapr invoke booking-service internal `POST /v1/incidents/ingest` (constructs
   full IDP, channel=webhook, persists, triggers auto-dispatch + outreach). Shared-secret per
   tenant via INCIDENT_WEBHOOK_SECRETS env JSON. Always 200 on valid JSON (400 on garbage).
7. create-topics.sh: add `opendesk.incidents` (additive; check for the known stray-backslash bug
   pattern). bindings.incident-webhook.yaml: Dapr http input binding → messaging-gateway.
8. Tests: ingestion idempotency, signature HMAC known-vector, dispatch workflow happy/retry,
   priority pacer bypass, gateway ingest validation. `go build/vet/test` green in all 3 services.

## Part C — Emergency lane + location-first + disclosure (voice runtime)

1. `app/emergency.py`: live-turn emergency detection (EN+PCM lexicon, same class list as Part A —
   duplicate the small lexicon deliberately, note the mirror in a comment); `is_emergency(text) ->
   (bool, severity, hazards)`; per-session `EmergencyState`.
2. Live-call behavior (livekit_worker.py, surgical): when is_emergency hits on a user turn →
   (a) set `session.emergency=True`, (b) emit metric `voice_emergency_sessions_total`, (c) trigger
   the EXISTING warm-handoff escalation path (escalation.py) immediately with reason "emergency"
   (first-claim on prewarmed agents is inherent — prewarming already shared; document), (d) inject a
   location-first prompt addendum: agent confirms exact location FIRST before anything else.
3. SessionMetrics: additive `emergency: bool` field flowing into the session-ended quality event
   payload (additive JSON key; downstream consumers tolerate unknown keys).
4. Tool `capture_location(address_text, lat?, lng?)` in tools.py: resolves contact from the
   conversation's caller (SIP caller-ID pattern / conversation contact_phone), then Dapr-invokes
   booking-service `PUT /v1/contacts/{id}/location` (Wave-8 contract); returns ack; never raises.
   Prompt addendum (prompts.py): emergency packs get location-first + reassurance behavior text.
5. Spoken AI disclosure: read pack `disclosure` from tenant context (`ctx.disclosure` dict —
   Part D contract: `{"spokenAiDisclosure": bool, "recordingConsent": bool, "text": str?}`).
   At session start, if spokenAiDisclosure → prepend disclosure line to the greeting
   ("You're speaking with an automated assistant…" + optional pack text); if recordingConsent →
   append recording notice. Default: no disclosure when block absent (byte-identical behavior).
6. Tests: emergency lexicon EN+PCM, escalation trigger on emergency, metrics flag, capture_location
   tool (mocked Dapr), disclosure injection on/off. Full pytest suite passes (309 baseline).

## Part D — Pack disclosure schema + widget GPS (packs/frontend)

1. validate_pack.py: optional `disclosure: {spokenAiDisclosure: bool, recordingConsent: bool,
   text: optional ≤200 chars}`; all 31 packs still pass. identity-service packs.go: passthrough
   (camelCase) mirroring voice/mcpServers pattern + Go test; go build/vet/test green.
2. Add `disclosure` blocks (spokenAiDisclosure: true, recordingConsent: true, sensible text) to
   law-enforcement, healthcare, civic-services packs; regenerate their index.json sha256;
   validate-index passes.
3. Widget GPS capture (embed.js + ui-actions-bridge.tsx): on widget init, if
   `data-location-consent="true"` attribute on the embed script tag → request
   navigator.geolocation once (graceful denial); include `{lat,lng,accuracy}` as
   `client_location` in each /voice/chat payload (additive JSON key — server tolerates unknown
   keys; bridge taps are pass-through so no bridge change needed beyond forwarding the field in
   the fetch tap's body passthrough — verify it passes body through untouched, edit only if it
   doesn't). Documented in docs/incidents.md.
4. docs/incidents.md: IDP schema usage, incidents API, dispatch endpoints + signature verification
   sample (python hmac), IoT webhook ingest format, emergency lane behavior, disclosure packs,
   widget GPS consent.

## Part E — Voice-quality conformance gates (eval harness)

1. `eval/quality_gates.py`: gates framework reading eval run results + SessionMetrics exports:
   - LATENCY: mouth-to-ear p95 ≤ 2500ms (configurable), measured from existing latency metrics.
   - STT: WER evaluation on sample corpora — `eval/corpora/README.md` + tiny committed samples
     manifest format (lang, transcript, audio-path-not-committed); WER via jiwer if installed,
     else a documented minimal Levenshtein implementation (no new hard deps).
   - TTS: intelligibility spot-check protocol (documented manual gate + automated duration/
     silence-ratio sanity on synthesized wav from the preview endpoint).
   - MOS-proxy: document the proxy formula from session metrics (latency, interruptions, WER)
     → 1-5 score; gate ≥ 3.5.
2. `scripts/eval-quality-gate.sh`: runs the eval harness + gates, exits non-zero on gate failure
   (CI-ready); docs/eval-quality-gates.md: gate reference + how to add corpora + CI wiring guide
   (note .github/workflows/ci.yml manual step).
3. Tests/validation: python -m py_compile; run gates against synthetic fixture results showing
   both pass and fail paths.

## Cross-agent contracts summary
- IDP JSON shape: canonical block above (A emits → B persists/serves/dispatches).
- `ctx.disclosure` camelCase `{"spokenAiDisclosure","recordingConsent","text"}` (D loader → C consumes).
- `PUT /v1/contacts/{id}/location` (Wave-8) — C's tool target.
- `incident_alert` paced kind + `priority: true` fast-lane (B internal contract).
- client_location additive key in /voice/chat payload (D sends; server tolerates).
