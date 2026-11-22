# Telephony — SIP inbound (Wave 5 #1)

Inbound PSTN calls ring the AI receptionist through LiveKit SIP: the carrier
hands calls to a LiveKit **inbound trunk**, a **dispatch rule** creates one
room per dialed number (`call-{number}`), and the voice worker
(`services/voice-agent-runtime`) bootstraps the session from the SIP
participant instead of the web `site-{slug}` room convention.

```
PSTN caller ── carrier SBC ──> LiveKit SIP inbound trunk (numbers list)
                                   │ dispatch rule (callee, roomPrefix call-)
                                   ▼
                          room `call-+15551234567`
                                   │ app/sip.py bootstrap
                                   ▼
              tenant from TENANT_PHONE_MAP[dialed] → receptionist flow
              caller ID → session.confirmed_phone (carrier-asserted)
```

## Carrier checklist

What to order/verify with the carrier (see
[runbooks/capacity-planning.md](runbooks/capacity-planning.md) §4 for the
full capacity math — telephony is the one plane you **buy**, not deploy):

- **Channels (concurrent calls)** — hard cap, 1 SIP channel per active PSTN
  call. Order for peak concurrency + headroom; procurement lead time is
  *weeks*, so size ahead of campaigns.
- **CPS (calls per second, start rate)** — the carrier's limit on NEW call
  attempts per second. Inbound bursts (advertising, opening hours) can trip
  it just like outbound; ask for the CPS figure in writing.
- **Numbers** — E.164 DIDs per tenant (local presence) or shared numbers
  with tenant routing (see mapping below).
- **Signalling** — SIP over UDP/TCP 5060 or TLS 5061 to the LiveKit server's
  SIP ingress; restrict to the carrier's SBC IPs (`allowed_addresses` on the
  trunk) and enable digest auth when offered.
- **Codecs/media** — G.711 μ-law/A-law is the safe baseline; confirm RTP port
  range and NAT handling with the LiveKit deployment.
- **Compliance** — caller-ID passthrough policy, call-recording consent
  rules per jurisdiction, emergency-call disclaimer (the receptionist is not
  an emergency service).

## Provisioning

```bash
# 1. edit numbers + hardening, then create trunk + dispatch rule
deploy/livekit-sip/setup.sh           # uses lk (livekit-cli)
LK_URL=https://lk.example.com LK_KEY=... LK_SECRET=... deploy/livekit-sip/setup.sh

# 2. map numbers to tenants on the voice worker (dev-mode static map)
TENANT_PHONE_MAP='{"+15551234567":"acme","+15557654321":"glow"}'
# optional fallback for unmapped numbers (empty = reject with error log):
SIP_DEFAULT_SITE=front-desk

# 3. call the number — the room `call-+1555…` is dispatched to the agent
```

Files:

- `deploy/livekit-sip/trunk-config.example.yaml` — `SIPInboundTrunkInfo`
  (`lk sip inbound create`): name, `numbers` list, optional
  `allowed_addresses` / digest auth / krisp.
- `deploy/livekit-sip/dispatch-rule.yaml` — `CreateSIPDispatchRuleRequest`
  (`lk sip dispatch create`): `dispatchRuleCallee` with `roomPrefix: call-`
  and `randomize: false`, so rooms are deterministically `call-{dialed}`.
- `deploy/livekit-sip/setup.sh` — applies both via livekit-cli
  (`LK_URL`/`LK_KEY`/`LK_SECRET` env, dev defaults match the compose stack).

## Number → tenant mapping design

The dialed number (which of *our* numbers the customer called) selects the
tenant — the server, never the model (`app/sip.resolve_tenant`).

- **Dev mode (shipped)**: `TENANT_PHONE_MAP` JSON
  `{"+1555…": "tenant-slug"}` parsed at worker boot into
  `Settings.tenant_phone_map`; invalid entries drop out with warnings.
- **Production design**: a `phone_numbers` table owned by booking-service
  (`number E164 PK, tenant_id, site_slug, provisioned_at, carrier_ref`),
  managed by a provisioning API (`POST /v1/phone-numbers {number, tenant}`)
  and cached in the voice runtime with a short TTL. The env map mirrors the
  table lookup semantics 1:1, so swapping the source is a one-function
  change in `app/sip.py`.
- **Caller identity**: the SIP caller ID (`From` / `P-Asserted-Identity`,
  surfaced by LiveKit as the participant's `sip.phoneNumber` attribute) is
  carrier-asserted, so the session starts with `confirmed_phone` already set
  — the two-step read-back confirmation is bypassed **for SIP sessions
  only** (policy rationale in `app/sip.py` and `app/session_state.py`).
  Anonymous/withheld caller IDs get no bypass; the normal confirmation
  applies.

## Outbound calls (SPEC-W101)

Platform-dialed calls (waitlist backfill, no-show rebook, ops callbacks)
use the same trunk plane in the other direction:

```
POST /v1/calls (X-Internal-Token; notification-worker / ops)
        │  OutboundPacer token bucket (OUTBOUND_CPS, default 1/s)
        ▼
app/outbound.py — CreateSIPParticipant via SIP OUTBOUND trunk
        │  room `outcall-{site_slug}-{call_id}`
        ▼
agents worker entrypoint (C4: tenant from the ROOM NAME — the callee's
number is the customer's, never a registry/TENANT_PHONE_MAP key)
        ▼
receptionist session; SessionStarted carries direction="outbound"
```

### Provisioning

```bash
# 1. create the outbound trunk (see outbound-trunk-config.example.yaml —
#    carrier SBC address + OUR caller-ID numbers + optional digest auth)
deploy/livekit-sip/setup.sh          # also prints `lk sip outbound list`

# 2. configure the voice worker
OUTBOUND_TRUNK_ID=<trunk id from lk sip outbound list>
OUTBOUND_CPS=1                        # at/below the carrier's CPS limit
VOICE_ADMIN_INTERNAL_TOKEN=...        # the /v1/calls credential

# 3. dial (internal callers only — the gateway never routes /v1/calls)
curl -X POST http://voice-agent-runtime:8080/v1/calls \
  -H 'X-Internal-Token: ...' -H 'content-type: application/json' \
  -d '{"site_slug":"acme","phone_number":"+2348030000000"}'
# 200 {"room":"outcall-acme-<id>","call_id":"<id>","status":"dialing",...}
```

### Honest failure surface

| Condition | Status |
|---|---|
| `OUTBOUND_TRUNK_ID` unset | 503 (trunk not provisioned) |
| `VOICE_ADMIN_INTERNAL_TOKEN` unset | 503 (fail closed) |
| wrong/missing `X-Internal-Token` | 401 |
| number not E.164 / unsafe slug | 422 (nothing dialed) |
| LiveKit SIP API failure | 502 |

The 200 `status` is always `"dialing"` — answer, decline, voicemail and
carrier errors are asynchronous SIP outcomes surfaced through the room
lifecycle (SessionStarted/SessionEnded with `direction="outbound"`), never
invented at dial time.

### Capacity discipline

Carrier **CPS is the binding constraint** (capacity-planning §4): the
endpoint paces every call start through a token bucket (`OUTBOUND_CPS`,
default 1/s, capacity 1 — bursts serialize). Channels then cap concurrency
as usual. Campaign callers (e.g. the WaitlistBackfill workflow) loop the
endpoint and inherit the pacing for free; per-campaign number rotation for
spam-reputation lives at the trunk (`numbers` list).

## Failure modes

| Symptom | Likely cause | Check |
|---|---|---|
| Call rings out, no room | dispatch rule/trunk ids mismatch | `lk sip dispatch list`, trunk numbers list |
| Room created, agent errors | dialed number not in `TENANT_PHONE_MAP` and no `SIP_DEFAULT_SITE` | worker log `sip tenant resolution failed` |
| Agent asks caller to confirm phone | caller ID withheld/anonymous | expected: no carrier assertion → two-step flow |
| Calls rejected at peak | carrier channel cap or CPS | carrier portal; capacity-planning.md §4 |
