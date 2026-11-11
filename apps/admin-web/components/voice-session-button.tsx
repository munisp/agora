"use client";

import * as React from "react";
import { Mic, PhoneOff } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import type { VoiceSession } from "@/lib/types";
import type { ElevenLabsConvAiSession } from "@/lib/convai";

type CallState = "idle" | "joining" | "connected" | "error";

/**
 * Warm-styled avatar tile (SPEC-W9 Part A). Renders the remote video track
 * an avatar provider (Tavus or the open avatar-renderer sidecar) publishes
 * into the LiveKit room. Audio-only sessions never produce a video track,
 * so nothing renders and the audio fallback is unchanged.
 */
function AvatarVideoTile({
  track,
}: {
  track: import("livekit-client").RemoteVideoTrack;
}) {
  const videoRef = React.useRef<HTMLVideoElement | null>(null);
  React.useEffect(() => {
    const el = videoRef.current;
    if (!el) return;
    track.attach(el);
    return () => {
      track.detach(el);
    };
  }, [track]);
  return (
    <div
      className="overflow-hidden rounded-2xl border shadow-sm"
      style={{ borderColor: "#e6d8c8", backgroundColor: "#f7f1ea" }}
    >
      <video ref={videoRef} autoPlay playsInline className="h-44 w-64 object-cover" />
    </div>
  );
}

/**
 * "Talk to receptionist" voice button.
 *
 * Two backends (SPEC-W47 UIMOB-10): the runtime answers POST /voice/session
 * with either a LiveKit {url, token} pair (connected via `livekit-client`,
 * dynamically imported) or — when AGENT_BACKEND=elevenlabs — a ConvAI
 * {signed_url} handled by the dependency-free websocket client in
 * lib/convai.ts. Older server builds omit `backend` and always return
 * LiveKit fields; null/undefined url+token without a signed_url is an
 * honest error, never a crash.
 *
 * STUB (clearly bounded): the surrounding UX (waveform, transcripts, mute,
 * volume) is intentionally minimal — this is the integration seam for the
 * full voice UI.
 */
export function VoiceSessionButton({
  tenant,
  siteSlug,
  accent = "#7c5b3e",
}: {
  tenant: string;
  siteSlug: string;
  accent?: string;
}) {
  const [state, setState] = React.useState<CallState>("idle");
  const [error, setError] = React.useState<string | null>(null);
  const [avatarTrack, setAvatarTrack] =
    React.useState<import("livekit-client").RemoteVideoTrack | null>(null);
  const roomRef = React.useRef<import("livekit-client").Room | null>(null);
  const convaiRef = React.useRef<ElevenLabsConvAiSession | null>(null);

  const hangUp = React.useCallback(async () => {
    try {
      await roomRef.current?.disconnect();
      await convaiRef.current?.stop();
    } finally {
      roomRef.current = null;
      convaiRef.current = null;
      setAvatarTrack(null);
      setState("idle");
    }
  }, []);

  React.useEffect(() => {
    return () => {
      void roomRef.current?.disconnect();
      void convaiRef.current?.stop();
    };
  }, []);

  const start = async () => {
    setState("joining");
    setError(null);
    try {
      // 1. Mint a session via the voice runtime (through APISIX).
      const session = await api.post<VoiceSession>("/voice/session", {
        tenant,
        site_slug: siteSlug,
      });

      // 2a. ElevenLabs backend: connect the ConvAI signed URL directly
      //     (no LiveKit fields exist in this mode).
      if (session.backend === "elevenlabs" || session.signed_url) {
        if (!session.signed_url) {
          throw new ApiError(502, {
            message: "The voice runtime did not return a signed URL.",
          });
        }
        const { ElevenLabsConvAiSession } = await import("@/lib/convai");
        const convai = new ElevenLabsConvAiSession(session.signed_url, {
          onStatusChange: (s) => {
            if (s === "connected") setState("connected");
            else if (s === "ended") void hangUp();
          },
          onError: (message) => {
            setError(message);
            setState("error");
          },
        });
        convaiRef.current = convai;
        await convai.start();
        return;
      }

      // 2b. LiveKit backend (default, and pre-W47 builds with no `backend`).
      if (!session.url || !session.token) {
        throw new ApiError(502, {
          message:
            "The voice runtime returned an unrecognised session (no LiveKit credentials, no signed URL).",
        });
      }
      const { Room, RoomEvent, Track } = await import("livekit-client");
      const room = new Room();
      roomRef.current = room;
      room.on(RoomEvent.Disconnected, () => {
        setAvatarTrack(null);
        setState("idle");
      });
      // SPEC-W9 Part A: avatar presence — when a provider (Tavus / open
      // renderer) publishes a remote VIDEO track into the room, surface it
      // as the avatar tile. Audio-only sessions never emit these events.
      room.on(RoomEvent.TrackSubscribed, (track) => {
        if (track.kind === Track.Kind.Video) {
          setAvatarTrack(track as import("livekit-client").RemoteVideoTrack);
        }
      });
      room.on(RoomEvent.TrackUnsubscribed, (track) => {
        if (track.kind === Track.Kind.Video) setAvatarTrack(null);
      });
      await room.connect(session.url, session.token);
      await room.localParticipant.setMicrophoneEnabled(true);
      setState("connected");
    } catch (e) {
      setError(
        e instanceof ApiError
          ? e.message
          : "Could not start the voice session. Check microphone permissions.",
      );
      setState("error");
      roomRef.current = null;
      void convaiRef.current?.stop();
      convaiRef.current = null;
    }
  };

  if (state === "connected" || state === "joining") {
    return (
      <span className="inline-flex flex-col items-start gap-3">
        {/* Avatar tile sits above the call controls (this stub has no audio
            visualizer yet; when one lands the tile stays directly above it). */}
        {state === "connected" && avatarTrack ? (
          <AvatarVideoTile track={avatarTrack} />
        ) : null}
        <button
          onClick={() => void hangUp()}
          disabled={state === "joining"}
          className="inline-flex items-center gap-2 rounded-full bg-destructive px-4 py-2 text-sm font-medium text-destructive-foreground cursor-pointer disabled:opacity-60"
        >
          <PhoneOff className="h-4 w-4" />
          {state === "joining" ? "Connecting…" : "End call"}
        </button>
      </span>
    );
  }

  return (
    <span className="inline-flex flex-col items-start gap-1">
      <button
        onClick={() => void start()}
        className="inline-flex items-center gap-2 rounded-full px-4 py-2 text-sm font-medium text-white cursor-pointer"
        style={{ backgroundColor: accent }}
      >
        <Mic className="h-4 w-4" />
        Talk to receptionist
      </button>
      {state === "error" && error ? (
        <span className="text-xs text-destructive">{error}</span>
      ) : null}
    </span>
  );
}
