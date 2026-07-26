# avatar-renderer

Open lip-sync avatar sidecar (SPEC-W9 Part A / A3). A LiveKit worker that
watches for active `site-{slug}` rooms, joins as the `avatar-renderer`
participant, subscribes to the voice agent's audio track, and publishes a
generated **video** track back into the room. The admin web app renders any
remote video track as the warm avatar tile
(`components/voice-session-button.tsx`, `app/app/[orgSlug]/call/call-client.tsx`).

This is the self-hosted alternative to the hosted Tavus provider — see
`docs/avatar.md` for the full provider comparison and setup.

## Modes (`AVATAR_RENDERER_MODE`)

| Mode       | What it does                                                                 | Needs |
|------------|------------------------------------------------------------------------------|-------|
| `mock`     | Solid warm-color test pattern at 15fps (slow brightness pulse for liveness). | Nothing — e2e-testable without GPU. |
| `musetalk` | **Documented stub.** Audio buffer → MuseTalk inference → RGB frames.         | CUDA GPU + MuseTalk weights (Dockerfile enablement block). |

## Run (mock, no GPU)

```sh
docker build -t opendesk/avatar-renderer:dev services/avatar-renderer
# as a compose override on the root stack:
docker compose -f docker-compose.yml \
  -f services/voice-agent-runtime/docker-compose.fragment.yml \
  -f infra/compose/avatar.compose.yml --profile voice up -d avatar-renderer
```

Voice-runtime env to activate the provider:

```
AVATAR_PROVIDER=musetalk
AVATAR_RENDERER=enabled
MUSETALK_ROOM_AGENT=true
```

## Env

| Var | Default | Purpose |
|-----|---------|---------|
| `LIVEKIT_URL` / `LIVEKIT_API_KEY` / `LIVEKIT_API_SECRET` | `ws://livekit:7880` / `devkey` / `secret` | LiveKit connection + token minting |
| `AVATAR_RENDERER_MODE` | `mock` | `mock` \| `musetalk` |
| `AVATAR_ROOM_PREFIX` | `site-` | Only join rooms with this prefix |
| `AVATAR_DISCOVERY_INTERVAL_S` | `5` | Room polling interval |
| `AVATAR_FRAME_WIDTH` / `AVATAR_FRAME_HEIGHT` / `AVATAR_FPS` | `320` / `240` / `15` | Published video geometry |

## Layout

```
app/main.py   # Renderer protocol + MockRenderer + MuseTalkRenderer stub,
              # LiveKit room worker (discovery -> join -> publish loop)
Dockerfile    # mock build by default; MuseTalk steps ready to uncomment
```

