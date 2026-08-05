# SPEC-W24 — Ops backlog fixes (6 independent workstreams)

Repo: /mnt/agents/output/opendesk (FUSE — md5 double-read every file you write). No git available locally.
Concurrent activity: a pusher is pushing W23 files to GitHub — it is READ-ONLY on the repo tree; do not touch w23 files, staging, or manifests.

## WS-A1 — workorders TZ flake (owner: Agent A)
- Files: `services/booking-service/internal/workorders/store_test.go` (+ `store.go` ONLY if the bug is in the bucketing logic).
- Symptom: `TestBoardAndToday` fails under `TZ=Asia/Shanghai`, passes under `TZ=UTC`, on a pristine baseline.
- Fix at the ROOT: "today"/board bucketing must be deterministic regardless of process TZ (use explicit time.Location — UTC or a configured tenant location — never implicit local time). If the code is correct and the test is TZ-sensitive, fix the test to pin its location.
- Gate: `go test ./services/booking-service/internal/workorders/...` passes under BOTH `TZ=UTC` and `TZ=Asia/Shanghai`.

## WS-A2 — lending decided_by JWT wiring (owner: Agent A)
- Files: `services/booking-service/internal/lending/handlers.go` only (events.go/lending.go read-only reference).
- Current: `DecidedBy *string` exists; approve/decline stamps `decided_by` only if supplied in the request body (handlers.go:402-427).
- Required: populate `decided_by` from the authenticated operator identity (follow the EXACT claims-extraction convention already used elsewhere in this codebase — find how httpapi/middleware surfaces the caller, e.g. a claims/principal context helper; reuse it, do not invent a new auth path). Explicit body-provided `decided_by` may remain as override only if that matches existing convention; otherwise JWT wins. Update/extend handler tests to cover: (a) JWT present → decided_by = JWT identity; (b) the override behavior per convention.
- Gate: `go test ./services/booking-service/internal/lending/...` green under TZ=UTC.

## WS-B1 — Temporal cron registration (owner: Agent B)
- Files: `services/booking-service/cmd/server/main.go` (+ at most one new file `internal/temporalclient/schedules.go` if a helper is warranted; the existing `tc.EnsureCommissionReconSchedule` pattern in main.go:205 is the template).
- Required: identify existing periodic workflows that are designed to run on a cadence but have NO schedule registration (candidates to investigate: booking reminders sweep, invoice dunning, campaignstudio AdvanceDue, loyalty expiry, metering/invoice rating run). Register a Temporal Schedule for each that genuinely exists, following the EnsureCommissionReconSchedule pattern: env-var cron spec (e.g. `REMINDERS_CRON`, `DUNNING_CRON`), empty env = skip registration (default off), log schedule_id on success. Do NOT create new workflow logic — only register schedules for workflows that already exist and are invoked elsewhere (prove each by citing the workflow function + its existing caller). If a candidate has no existing workflow, exclude it and note why.
- Also add the new `*_CRON` env vars to `.env.example` in the existing single-owner style (commented, empty default).
- Gate: `go build ./...` green; `go vet ./services/booking-service/cmd/...` clean.

## WS-B2 — TigerBeetle DisbursementIntent consumer (owner: Agent B)
- Files: ONE new file `services/booking-service/internal/consumer/lending_disbursements.go` (+ its test file). Read-only reference: `internal/lending/events.go` (event shape), existing consumer registration in `internal/consumer/consumer.go` — you MAY add the registration line to consumer.go if that's the established pattern; otherwise wire via main.go? NO — main.go is Agent B's own file too, fine, but prefer consumer.go's pattern.
- Required: consume the lending disbursement-intent event and create the corresponding TigerBeetle transfer through the EXISTING TB client/bridge used by payments (find it; reuse its account codes and idempotency conventions — idempotency key derived from the intent/event ID, exactly-once effect under redelivery). Disabled when TB is not configured (follow the existing disabled/mock posture of the payments ledger).
- Gate: `go test ./services/booking-service/internal/consumer/...` green incl. a redelivery-idempotency test; `go build ./...` green.

## WS-C1 — Keycloak redirectUris for field-pwa (owner: Agent C) — AMENDED
- Correction (Agent C finding, verified): there is NO field-pwa client; the empty arrays at ~line 93 belong to `service-accounts`. field-pwa is a dependency-free static PWA served per docs via `python3 -m http.server` → origin `http://localhost:8000`, and `apps/field-pwa/app.js` defaults to `clientId: "admin-web"`.
- File: `infra/keycloak/realm-opendesk.json` — the EXISTING **admin-web** client block.
- Required: add `"http://localhost:8000/*"` to `redirectUris` and `"http://localhost:8000"` to `webOrigins` of the admin-web client (path blessed by docs/pwa.md "Honest gap #2"; localhost only; no new client; app.js untouched).
- Gate: `python3 -c "import json;json.load(open('infra/keycloak/realm-opendesk.json'))"` parses; diff shows ONLY the admin-web client's redirectUris/webOrigins arrays changed.

## WS-C2 — Agora brand icons (owner: Agent C)
- Files: `apps/admin-web/public/icons/icon-192.png`, `apps/admin-web/public/icons/icon-512.png`, `apps/field-pwa/icons/icon-192.png`, `apps/field-pwa/icons/icon-512.png` (overwrite in place; check for any other icon PNGs referenced by manifest.webmanifest / app.json and cover those too).
- Required: regenerate as Agora brand marks with PIL (deterministic, no external assets): warm cream background #FAF6F0 (or transparent per existing format — MATCH the existing file's mode/format), terracotta #C0562F lettermark "A" (bold, centered, ~60% of canvas), optional thin amber #D99A4E underline accent. Match existing dimensions exactly (192×192, 512×512).
- Gate: files are valid PNGs of the exact required sizes; md5 double-read stable.

## Ownership boundaries (hard)
- Agent A: `internal/workorders/*`, `internal/lending/handlers.go`, `internal/lending/handlers_test.go` (or new test file in lending/).
- Agent B: `cmd/server/main.go`, `internal/consumer/*`, new `internal/temporalclient/*`, `.env.example`.
- Agent C: `infra/keycloak/realm-opendesk.json`, icon PNGs.
- No other writes. No shared-file edits across agents. If you need a change in another agent's file, report it instead.

## Universal gates
- `gofmt -l` on your Go files = empty; `go build ./...` and relevant `go test` green (TZ=UTC; WS-A1 also under TZ=Asia/Shanghai).
- Every written file md5 double-read stable; deliver a manifest `<md5> <size> <relpath>` for every file you created/modified.
