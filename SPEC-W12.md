# SPEC-W12 — Channels & Compliance Guards (CAC App, Wave 1 of 5)

Wave 12 of the CAC program (see CAC-FIT-ANALYSIS.md). Four builders, strict file ownership.
Work in /tmp, rsync ADDITIVELY (no --delete) to /mnt/agents/output/opendesk, md5-verify every file.
Go: /tmp/sdk/go/bin/go (install go1.23.4 if absent), GOPROXY=https://goproxy.cn,direct,
GOCACHE=/tmp/gocache, GOMODCACHE=/tmp/gomod. No Docker. Rust: no toolchain — static review only.

## Cross-agent contracts (bind everyone)

1. **USSD session**: Africa's Talking callback POST form fields `sessionId, serviceCode,
   phoneNumber, text` (text = cumulative `1*2*3` input, empty on first request). Response is
   `text/plain`, prefixed `CON ` (continue) or `END ` (terminate). Session state TTL 180s.
2. **Channel enum**: `ussd` joins voice|whatsapp|telegram|web|sms|webhook everywhere a channel
   is enumerated. conversation-service channel map: ussd→ussd (IDP channel enum gains "ussd"
   via additive enum value — docs/schemas/incident-data-packet.json NOT touched this wave;
   classifier treats ussd as web-like).
3. **DND/quiet-hours classification**: every paced send kind is classified
   `marketing` (geo_campaign, promo, broadcast, drip) or `transactional` (booking confirmations,
   reminders, incident_alert, otp). Marketing: DND-suppressed + quiet-hours deferred.
   Transactional: neither. incident_alert additionally bypasses quiet hours via existing
   Priority lane (unchanged).
4. **ConsentRecord** (identity-service): `{consent_id uuid, tenant_id, data_subject_id text
   (phone_e164 or contact uuid), purpose text, captured_ts, captured_channel, captured_locale,
   erasure_ts null}`. Erasure event: CloudEvent `com.opendesk.consent.ErasureRequested` on
   topic `opendesk.consent.erasure.v1`.
5. **KYC service**: `kyc-service` Go :7013, Dapr app-id `kyc`. `POST /v1/kyc/resolve`
   `{tenant_id, subject_phone, id_type:"bvn"|"nin", id_value}` →
   `{status:"verified"|"mismatch"|"pending", reference, latency_ms}`. KYC_MOCK=1 default
   (deterministic mock: id_value all digits & len>=10 → verified). Consent-gated: calls
   identity `GET /internal/consents/check?subject=&purpose=kyc` first; no consent → 403.
6. **Flutterwave**: payments-service (Rust) mirrors the existing Paystack module shape:
   `POST /v1/payments/flutterwave/initialize`, webhook `POST /webhooks/flutterwave` with
   `verif-hash` header == configured secret hash (constant-time compare). Same outbox/ledger
   codes as Paystack path.
7. **Topics** (infra/kafka/create-topics.sh, additive): `opendesk.consent.erasure.v1`,
   `opendesk.kyc.resolved.v1`, `cac.events`.
8. **Config naming**: env vars UPPER_SNAKE with service prefix where applicable:
   `AT_API_KEY/AT_USERNAME`, `TERMII_API_KEY`, `EBULK_API_KEY/EBULK_USERNAME`,
   `SMS_PROVIDER_CHAIN` (default "africastalking,termii,ebulksms"),
   `DND_ENFORCEMENT` (default true), `QUIET_HOURS_DEFAULT` (default "20:00-08:00"),
   `KYC_MOCK` (default 1), `FLUTTERWAVE_SECRET_HASH`, `FLUTTERWAVE_SECRET_KEY`.

## Agent A — messaging-gateway: USSD + NG SMS aggregators
Owns:
- services/messaging-gateway/internal/channel/ussd.go (NEW)
- services/messaging-gateway/internal/provider/africastalking.go, termii.go, ebulksms.go, failover.go (NEW)
- services/messaging-gateway/internal/httpapi/ussd.go (NEW)
- services/messaging-gateway/internal/httpapi/server.go (ADDITIVE route registration only)
- services/messaging-gateway/internal/config.go (ADDITIVE)
- services/messaging-gateway/cmd/server/main.go (ADDITIVE wiring)
- services/messaging-gateway/internal/httpapi/ussd_test.go, internal/provider/failover_test.go (NEW)
- docs/channels-ussd.md (NEW)
Requirements:
- AT USSD callback per contract §1; session store via existing Dapr state pattern (or Redis if
  that is the established pattern in this service — inspect and follow); forward each completed
  USSD interaction to conversation-service via the SAME Dapr invoke path other inbound channels
  use (inspect telegram/whatsapp inbound and mirror it), channel="ussd".
- USSD menus: if the tenant's pack defines `ussd.menu` (list of {key,label,action}), drive a
  numeric menu state machine; else pass-through text mode to conversation-service. Menu fetch
  via identity packs summary endpoint (same client pattern as other services).
- SMS providers: each implements the existing Provider interface (inspect provider/ dir).
  AT: POST https://api.africastalking.com/version1/messaging {username,to,message,from}
  header apiKey. Termii: POST https://api.ng.termii.com/api/sms/send {api_key,to,from,sms,type:"plain",channel:"generic"}.
  eBulks: POST https://api.ebulksms.com/sendsms {username,apikey,sender,messagetext,flash:0,recipients}.
  Mark every request/response shape as ASSUMPTION in code comments + docs (no live keys here).
- failover.go: ordered chain from SMS_PROVIDER_CHAIN; on 5xx/timeout → next provider;
  per-provider circuit breaker (mirror the voice runtime's CircuitBreaker concept, Go idiom);
  per-provider price-tier annotation (at=1.0, termii=1.0, ebulks=0.85 relative) used only for
  reporting, metering event `sms_send` gains `provider` label following existing metering.
- Tests: httptest-based for all 3 providers + failover (first-fails-second-succeeds,
  all-fail error), USSD session happy path (new session → CON menu → selection → END),
  TTL expiry, garbage form → 400. go build/vet/test green.

## Agent B — notification-worker: DND 2442 + quiet hours
Owns:
- services/notification-worker/internal/pacer/guards.go (NEW)
- services/notification-worker/internal/store/dnd.go (NEW: dnd_numbers table, RLS-optional
  global list + per-tenant opt-outs; follow existing store bootstrap pattern)
- services/notification-worker/internal/activities/paced.go (SURGICAL: pre-send guard call)
- services/notification-worker/internal/workflows/paced.go (SURGICAL: kind classification +
  deferral on quiet hours via workflow.Sleep until window opens; do not alter Priority lane)
- services/notification-worker/internal/httpapi/dnd.go (NEW: POST /v1/dnd/import (admin),
  DELETE /v1/dnd/{phone} (opt-out honor), GET /v1/dnd/check?phone=)
- services/notification-worker config (ADDITIVE: DND_ENFORCEMENT, QUIET_HOURS_DEFAULT,
  QUIET_HOURS_OVERRIDES json per-channel)
- tests: guards_test.go, dnd_test.go (+ store test w/ embedded-postgres pattern)
- docs/dnd-quiet-hours.md (NEW)
Requirements:
- Kind classification table per contract §3 in code (explicit map, tested).
- DND check order: per-tenant opt-out → global list. Marketing kinds suppressed → workflow
  completes with status "suppressed_dnd" + metered counter `notifications_suppressed_total{reason}`.
- Quiet hours: parse "HH:MM-HH:MM" (overnight windows supported), tenant tz (default Africa/Lagos);
  marketing sends deferred (Sleep) to window open, transactional pass. Priority lane untouched.
- All guard decisions logged + counted. go build/vet/test green.

## Agent C — consent registry + KYC service + Flutterwave
Owns:
- services/identity-service/internal/consent/ (NEW: model, store w/ RLS, handlers)
- services/identity-service internal wiring files (ADDITIVE only — routes, main)
- services/identity-service tests for consent
- services/kyc-service/ (NEW Go service: cmd/server/main.go, internal/{httpapi,store,config},
  go.mod, Dockerfile — follow booking-service layout; port 7013, Dapr app-id kyc)
- services/payments-service/src/flutterwave.rs (NEW — mirrors paystack.rs shape) +
  services/payments-service/src/main.rs (ADDITIVE mod+route registration only)
- docs/compliance-ndpa.md, docs/kyc.md (NEW)
Requirements:
- ConsentRecord per contract §4; RLS tenant isolation (follow identity-service table pattern);
  POST /v1/consents (capture, idempotent on (tenant,subject,purpose)), GET /v1/consents?subject=,
  GET /internal/consents/check (service-to-service, no auth middleware but tenant header),
  POST /v1/consents/erasure → sets erasure_ts + publishes contract §4 CloudEvent via the
  service's existing event/outbox pattern. Erasure is tombstone-only (data-subject records
  anonymized by downstream consumers; document consumer contract in docs).
- kyc-service per contract §5; audit table kyc_audit (tenant RLS, who/what/when/result);
  publish `com.opendesk.kyc.Resolved` on opendesk.kyc.resolved.v1; p95 target ≤8s documented;
  mock deterministic per contract.
- Flutterwave per contract §6 — RUST, NO TOOLCHAIN: mirror paystack.rs exactly (same client
  construction, same error idiom, same outbox codes). Constant-time verif-hash compare.
  Static self-review checklist in PR-style report; cargo check left to CI (documented caveat).
- go build/vet/test green for identity-service + kyc-service.

## Agent D — conversation USSD mapping + infra + docs
Owns:
- services/conversation-service/app/ussd.py (NEW: session→conversation mapping, menu-mode
  turn builder, low-literacy numeric option parsing "1"/"2"/"0=back"/"00=main menu")
- services/conversation-service/app/routes.py (SURGICAL: ussd inbound hook, mirrors existing
  channel hooks; channel="ussd" passes incident classifier unchanged)
- services/conversation-service/app/config.py (ADDITIVE)
- services/conversation-service/tests/test_ussd.py (NEW)
- infra/kafka/create-topics.sh (ADDITIVE: 3 topics per contract §7)
- infra/dapr/components/state.ussd.yaml (NEW if no generic state store exists — inspect first)
- docker-compose.yml (ADDITIVE: kyc-service block + AT/TERMII/EBULK env passthrough +
  FLUTTERWAVE_* env — MINIMAL diff, additive lines only)
- docs/runbook-wave12.md (NEW: end-to-end ops runbook incl. data-residency note)
Requirements:
- USSD mapping: sessionId → deterministic conversation key (uuid5(tenant, sessionId));
  each callback appends a user turn with the cumulative text parsed to the LAST selection;
  agent/system turns never incident-classified (existing rule). Menu mode: when pack menu
  present, conversation-service returns menu text via the existing outbound path (Dapr invoke
  back to messaging-gateway response is synchronous — COORDINATE: conversation-service returns
  the reply text in the invoke response body; messaging-gateway (Agent A) renders CON/END.
  Document this request/reply contract in BOTH docs/channels-ussd.md (A) and here).
- pytest green (existing 56 + new).
- bash -n infra/kafka/create-topics.sh; compose YAML parses (python yaml.safe_load).

## Delivery protocol (all agents)
1. rsync repo to /tmp workspace (exclude .git, node_modules, __pycache__).
2. Implement ONLY owned files. Never touch another agent's files. If you believe a foreign
   file needs a change, implement around it and FLAG it in your report.
3. Run the required gates in /tmp. Paste real tails.
4. rsync additively back to /mnt (no --delete), md5-verify every delivered file FROM /mnt.
5. Report: file list + md5s, test tails, assumptions, residual risks.
