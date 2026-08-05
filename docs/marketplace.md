# Agora Pack Marketplace (Wave 5 #6)

Industry packs (`industries/*.yaml`, SPEC-CRM §C) configure terminology, the
agent persona, booking policy, Temporal workflow, catalog seed, reminders,
knowledge seed, dashboard labels, and — optionally — multi-agent crews
(`agents`, SPEC-W3 §4 innovation 6) and declarative HTTP plugin tools
(`customTools`, SPEC-W3 §4 innovation 15). This document is the publishing
guide for 3rd-party pack authors and the design note for the future signed
marketplace. Monetization context: STRATEGY.md §2 item 1 (integration
marketplace with 15–30% platform revenue share on paid packs).

## The registry: `industries/index.json`

```json
{
  "schemaVersion": 1,
  "packs": [
    {
      "id": "salon",
      "version": "1.0.0",
      "sha256": "<hex sha256 of salon.yaml>",
      "author": "opendesk",
      "signature": null,
      "path": "salon.yaml"
    }
  ]
}
```

- `id` — pack id; must equal the `id` inside the YAML and the file name
  (`<id>.yaml`).
- `version` — semantic version of the pack document.
- `sha256` — checksum of the pack file; verified by
  `make validate-packs` and at install time.
- `author` — `opendesk` for built-ins, publisher id for community packs.
- `signature` — `null` until sigstore signing lands (see below).
- `path` — pack file relative to the registry file.

## Pack schema (validated on install and on every service boot)

`scripts/validate_pack.py` enforces the same rules identity-service enforces
at load time (`services/identity-service/internal/packs/packs.go`), so an
invalid pack fails fast at install instead of crashing a service:

| Field | Rule |
|---|---|
| `id`, `displayName` | required, non-empty strings |
| `terminology.{offering,team_member,booking,contact}` | all four required |
| `agentPersona` | required, non-empty |
| `bookingPolicy` | optional; `depositPercent` 0–100, `noShowFeeCents` ≥ 0, `cancellationWindowHours` ≥ 0, `phoneConfirmation`/`intakeRequired` booleans |
| `temporalWorkflow` | enum: `SalonDepositWorkflow`, `ClinicIntakeWorkflow`, `ConsultancyFollowupWorkflow`, `SupportEscalationWorkflow` |
| `offerings[]` | `name` required, `duration_min` > 0, `buffer_min`/`price_cents`/`capacity` ≥ 0 |
| `knowledgeSeed[]` | `title` + `body` required |
| `dashboardLabels.{bookingSingular,bookingPlural,customerTerm}` | all three required |
| `agents[]` (optional) | `id` slug (`^[a-z][a-z0-9-]*$`, unique), `name`, `persona`, ≥ 1 non-empty `intents` |
| `customTools[]` (optional) | `name` (`^[a-zA-Z_][a-zA-Z0-9_]*$`, unique), `description`, `method` ∈ GET/POST/PUT/PATCH/DELETE, absolute http(s) `url`, optional `bodyTemplate` |

A new `temporalWorkflow` value requires the workflow to actually exist in
notification-worker first — the enum is deliberately closed.

## Installing a community pack

```bash
make install-pack PACK=https://publisher.example/packs/auto-detailer.yaml \
                  VERSION=1.2.0 AUTHOR=auto-detailer-inc
# or a local file
./scripts/install-pack.sh ./my-pack.yaml --version 0.1.0 --author me
```

The installer downloads the YAML, validates it against the schema above,
installs it as `industries/<id>.yaml`, and upserts the registry entry
(including the recorded sha256). Finish with `make validate-packs`, then
point a tenant at the new industry (`industry: <id>` at tenant creation —
see `scripts/seed-industries.sh`).

**Security posture today:** packs are data (persona text, seed catalog,
declarative HTTP tools), not code — but a malicious pack can still exfiltrate
tool-call arguments via a `customTools` URL, so install only packs you trust.
The voice runtime's SSRF guard applies on top of the validator's http(s)
check. There is **no signature verification yet**: every installed pack gets
`signature: null` and the installer prints a warning.

## Signing design (future: sigstore)

Marketplace v2 will sign packs with **sigstore** (keyless, Fulcio-issued
identity bound to the publisher's OIDC account, Rekor transparency log):

1. Publisher runs `cosign sign-blob --bundle pack.sigstore.json pack.yaml`.
2. The registry entry stores the bundle reference in `signature` (replacing
   `null`): `{"bundle": "<url-or-inline>", "identity": "<oidc-subject>",
   "issuer": "<oidc-issuer>"}`.
3. `install-pack.sh` verifies the bundle offline with the embedded Rekor key
   before installing; `validate_pack.py validate-index` gains a `--verify`
   mode that re-verifies every signed entry. Built-in packs are signed with
   the Agora release identity; unsigned community packs keep installing
   with a warning (policy flag `--require-signed` for regulated deployments).

## Revenue share (policy placeholder)

Paid packs sold through the marketplace are subject to the platform revenue
share defined in **STRATEGY.md §2 item 1** (15–30% to Agora, rest to the
publisher). Free packs are always permitted and drive adoption. Payout
cadence, refund policy and tax handling are TBD and will be documented with
the marketplace launch; pack authors should treat this section as a pointer,
not a contract.

## Checklist for pack authors

1. Copy the closest built-in pack (`industries/salon.yaml` etc.) as a starting point.
2. Validate locally: `python3 scripts/validate_pack.py validate my-pack.yaml`.
3. Smoke it on a dev stack: `make install-pack PACK=my-pack.yaml`, create a
   tenant with your industry, place a test call.
4. Publish the YAML at a stable URL and (once signing lands) sign it.
