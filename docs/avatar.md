# Avatar presence (SPEC-W9 Part A)

Optional visual avatar for voice sessions. When enabled, an avatar provider
joins a **video track** into the session's LiveKit room (`site-{slug}`); the
admin web app renders any remote video track as a warm-styled avatar tile
above the call controls
(`components/voice-session-button.tsx`,
`app/app/[orgSlug]/call/call-client.tsx`). Audio-only behavior is unchanged —
with `AVATAR_PROVIDER=none` (default) nothing joins, nothing renders.

Design rules:

- **Never blocks the session.** `/voice/session` mints the token, fires the
  provider join as a background task, and answers immediately. Any provider
  failure degrades to a warning log + audio-only session.
- **Additive response.** `POST /voice/session` gains
  `avatar: {"provider", "status", "detail?"} | null` (`status`:
  `joining` | `unavailable`; `null` when off).
- **Provider per deployment (or tenant).** `AVATAR_PROVIDER=none|tavus|musetalk`;
  a per-tenant override is read defensively from the tenant context
  (`getattr(tenant_ctx, "avatar_provider", ...)`) when a future tenant
  payload carries it — unknown/empty values fall back to the env setting.

## Provider comparison

| | **Tavus CVI** (hosted) | **Open lip-sync** (MuseTalk sidecar) |
|---|---|---|
| Code | `app/avatar/tavus.py` | `app/avatar/musetalk.py` + `services/avatar-renderer` |
| Quality | Photoreal Phoenix replicas, production-grade lip-sync | Good lip-sync, limited to a prepared reference face; quality depends on model/weights |
| Latency | Tavus-optimized WebRTC pipeline; join takes seconds | Local network to LiveKit; inference latency is yours to tune |
| GPU | None (hosted) | **Required for real time** (see GPU honesty) |
| Cost | Per-minute hosted billing | Infra cost only (GPU instance) |
| Data | Audio leaves to Tavus | Everything stays in your network |
| Ops | API key + replica/persona IDs | Sidecar container, model weights, GPU scheduling |

## Tavus setup

Env on `voice-agent-runtime`:

```
AVATAR_PROVIDER=tavus
TAVUS_API_KEY=...
TAVUS_REPLICA_ID=...   # stock or trained replica
TAVUS_PERSONA_ID=...   # optional; omitted from the request when empty
```

On session start the runtime POSTs `https://api.tavus.io/v2/conversations`
(`x-api-key` header, 10s timeout, fire-and-forget) with:

```json
{"replica_id": "...", "persona_id": "...",
 "properties": {"livekit_room_name": "site-<slug>"}}
```

> **Flagged assumption (verified against Tavus docs, 2026-07):** current
> Tavus CVI docs
> (<https://docs.tavus.io/api-reference/conversations/create-conversation>)
> confirm the `v2/conversations` endpoint and that the legacy
> `replica_id`/`persona_id` fields remain accepted aliases of the renamed
> `face_id`/`pal_id`. However, the docs now steer LiveKit integrations at
> the **LiveKit Agents plugin** (`tavus.AvatarSession`,
> <https://docs.tavus.io/sections/integrations/livekit>) rather than
> documenting `properties.livekit_room_name` directly — that property is the
> documented v2/conversations shape for making a replica join an existing
> LiveKit room and is what we implement per SPEC-W9 A1. The request builder
> is isolated in `app/avatar/tavus.py::build_conversation_payload`; if Tavus
> rejects/retires the property, that single function (or a switch to
> instantiating `tavus.AvatarSession` inside the LiveKit worker) is the only
> change needed.

## Open lip-sync setup (MuseTalk)

Two halves:

1. **Intent provider** (`app/avatar/musetalk.py`) in the voice runtime —
   reports `joining` and logs the intent when:
   ```
   AVATAR_PROVIDER=musetalk
   AVATAR_RENDERER=enabled
   MUSETALK_ROOM_AGENT=true
   ```
2. **Renderer sidecar** (`services/avatar-renderer`) — a LiveKit worker that
   polls the RoomService API for active `site-*` rooms, joins, subscribes to
   the agent's audio track, and publishes generated video. Frame generation
   sits behind a `Renderer` protocol with two implementations:
   - `MockRenderer` (default): solid warm-color 15fps test pattern with a
     brightness pulse. **No GPU** — validates the whole pipeline.
   - `MuseTalkRenderer`: documented stub (audio ring buffer → MuseTalk
     inference → RGB frames). Enablement steps are ready-to-uncomment in
     `services/avatar-renderer/Dockerfile`.

Run the sidecar (mock mode) as a compose override:

```sh
docker compose -f docker-compose.yml \
  -f services/voice-agent-runtime/docker-compose.fragment.yml \
  -f infra/compose/avatar.compose.yml --profile voice up -d avatar-renderer
```

## GPU honesty

`AVATAR_RENDERER_MODE=mock` needs **no GPU** and is the default everywhere.

Real MuseTalk lip-sync inference is a video diffusion/UNet workload:

- **CPU is not viable** for real time — it lands far below the 15fps floor
  the renderer publishes at. Do not ship `musetalk` mode on CPU.
- Target a modern **NVIDIA GPU with ≥8 GB VRAM** (MuseTalk's own README
  reports near-real-time throughput on datacenter-class cards at 256×256;
  budget headroom for the 320×240 stream plus the voice agent if colocated).
- The container must run with GPU access (`--gpus all` or the commented
  `deploy.resources.reservations.devices` block in
  `infra/compose/avatar.compose.yml`), and the image must switch to a CUDA
  base per the Dockerfile enablement block.

## Mock-renderer testing (e2e without GPU)

1. Voice runtime env: `AVATAR_PROVIDER=musetalk AVATAR_RENDERER=enabled
   MUSETALK_ROOM_AGENT=true`.
2. Start the sidecar in mock mode (compose command above).
3. Open a tenant site, click **Talk to receptionist**. Within one discovery
   interval (~5s) the sidecar joins `site-<slug>` and publishes the warm
   test-pattern tile; the tile appears above the call controls.
4. Hang up: the room closes, the sidecar's next sweep cancels its session.

Unit-level: `services/voice-agent-runtime/tests/test_avatar.py` covers the
registry, Tavus request shape + failure degradation (mocked httpx), the
MuseTalk intent gates, and the additive `avatar` response field.

