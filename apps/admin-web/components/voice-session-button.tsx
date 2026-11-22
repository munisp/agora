"use client";

import * as React from "react";
import { useVisitor, VisitorForm, type VisitorDetails } from "@/components/visitor-form";
import { playRemoteAudio, unlockCallAudio } from "@/lib/call-audio";
import { ReceptionistVideo, type CallActivity } from "@/components/receptionist-video";
import { Languages, Mic, PhoneOff } from "lucide-react";
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

/** Languages a caller can choose for a call, in their own names (the voice runtime's CALL_LANGUAGES). */
const CALL_LANGUAGES = [
  { code: "en", label: "English" },
  { code: "pcm", label: "Pidgin" },
  { code: "yo", label: "Yorùbá" },
  { code: "ha", label: "Hausa" },
  { code: "ig", label: "Igbo" },
];

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
  // 2026-11-21: name/phone/email asked once before the first call (components/visitor-form.tsx).
  const [visitor, setVisitor] = useVisitor(tenant);
  const [askDetails, setAskDetails] = React.useState(false);
  // The language the caller speaks; the receptionist hears, thinks and answers in it (voice runtime CALL_LANGUAGES).
  const [language, setLanguage] = React.useState("en");
  const [error, setError] = React.useState<string | null>(null);
  // The receptionist's voice: drives the animated avatar (mouth follows the audio).
  const [voiceTrack, setVoiceTrack] = React.useState<{ mediaStreamTrack: MediaStreamTrack } | null>(null);
  // What the receptionist is doing (worker data topic "agora.activity"): typing while it uses a tool.
  const [activity, setActivity] = React.useState<CallActivity | null>(null);
  // The language the call is in right now: starts as the caller's choice, follows them if they switch mid-call.
  const [callLanguage, setCallLanguage] = React.useState("en");
  // Live captions: the last lines said on the call (worker data topic "agora.activity", state "caption").
  const [captions, setCaptions] = React.useState<{ role: string; text: string }[]>([]);
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
      setVoiceTrack(null);
      setActivity(null);
      setCaptions([]);
      setState("idle");
    }
  }, []);

  React.useEffect(() => {
    return () => {
      void roomRef.current?.disconnect();
      void convaiRef.current?.stop();
    };
  }, []);

  const start = async (who: VisitorDetails | null = visitor) => {
    setAskDetails(false);
    setState("joining");
    setError(null);
    setCallLanguage(language);
    setCaptions([]);
    try {
      // 1. Mint a session via the voice runtime (through APISIX).
      const session = await api.post<VoiceSession>("/voice/session", {
        tenant,
        site_slug: siteSlug,
        ...(language !== "en" ? { language } : {}),
        ...(who ? { visitor: who } : {}),
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
      playRemoteAudio(room, { RoomEvent, Track });
      room.on(RoomEvent.Disconnected, () => {
        setAvatarTrack(null);
        setVoiceTrack(null);
        setActivity(null);
        setState("idle");
      });
      room.on(RoomEvent.DataReceived, (payload, _participant, _kind, topic) => {
        if (topic !== "agora.activity") return;
        try {
          const msg = JSON.parse(new TextDecoder().decode(payload)) as CallActivity & {
            language?: string;
            role?: string;
            text?: string;
          };
          if (msg.state === "caption") {
            // One line per turn: the receptionist's sentences arrive one by one, so join them onto its current line
            // (keeping the latest ~220 characters) — otherwise the caller's words scroll away immediately.
            if (msg.text) {
              const role = msg.role ?? "";
              const text = msg.text as string;
              setCaptions((prev) => {
                const last = prev[prev.length - 1];
                if (last && last.role === role) {
                  const joined = `${last.text} ${text}`;
                  return [...prev.slice(0, -1), { role, text: joined.length > 220 ? `…${joined.slice(-220)}` : joined }];
                }
                return [...prev.slice(-1), { role, text }];
              });
            }
          } else if (msg.state === "language") {
            if (msg.language) setCallLanguage(msg.language);
          } else {
            setActivity(msg);
          }
        } catch {
          // not ours / malformed: ignore
        }
      });
      // SPEC-W9 Part A: avatar presence — when a provider (Tavus / open
      // renderer) publishes a remote VIDEO track into the room, surface it
      // as the avatar tile. Audio-only sessions never emit these events.
      room.on(RoomEvent.TrackSubscribed, (track) => {
        if (track.kind === Track.Kind.Video) {
          setAvatarTrack(track as import("livekit-client").RemoteVideoTrack);
        }
        if (track.kind === Track.Kind.Audio) setVoiceTrack(track);
      });
      room.on(RoomEvent.TrackUnsubscribed, (track) => {
        if (track.kind === Track.Kind.Video) setAvatarTrack(null);
      });
      // Relay through TURN when the runtime offers it (LiveKit may take no inbound UDP from the internet).
      await room.connect(
        session.url,
        session.token,
        session.ice_servers?.length ? { rtcConfig: { iceServers: session.ice_servers } } : undefined,
      );
      await unlockCallAudio(room);
      // DTX off: with DTX the browser sends almost no packets while the caller is silent (listening to a long answer),
      // LiveKit rated the connection "lost" and the browser did a full reconnect — after which the receptionist could
      // no longer hear the caller. A steady packet flow costs ~30 kbps and keeps the call stable.
      await room.localParticipant.setMicrophoneEnabled(true, undefined, { dtx: false });
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
        ) : (
          <ReceptionistVideo
            track={state === "connected" ? voiceTrack : null}
            activity={activity}
            accent={accent}
            language={CALL_LANGUAGES.find((l) => l.code === callLanguage)?.label}
          />
        )}
        {state === "connected" && captions.length > 0 ? (
          <div
            className="w-[280px] max-w-full space-y-1 rounded-xl px-3 py-2 text-[13px] leading-snug"
            style={{ backgroundColor: "rgba(0,0,0,0.55)", color: "#fff" }}
            aria-live="polite"
            aria-label="Live captions"
            data-captions
          >
            {captions.map((c, i) => (
              <p key={i} className={i < captions.length - 1 ? "opacity-70" : ""}>
                <span className="font-semibold" style={{ color: c.role === "caller" ? "#c9e7ff" : "#f3d9a4" }}>
                  {c.role === "caller" ? "You" : "Receptionist"}:
                </span>{" "}
                {c.text}
              </p>
            ))}
          </div>
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
      <span className="inline-flex flex-wrap items-center gap-2">
        <button
          onClick={() => (visitor ? void start(visitor) : setAskDetails(true))}
          className="inline-flex items-center gap-2 rounded-full px-5 py-2.5 text-sm font-medium cursor-pointer"
          style={{ backgroundColor: accent, color: readableOn(accent) }}
        >
          <Mic className="h-4 w-4" />
          Talk to receptionist
        </button>
        <label className="inline-flex items-center gap-1.5 text-xs" style={{ color: "inherit", opacity: 0.85 }}>
          <Languages className="h-3.5 w-3.5" aria-hidden="true" />
          <span className="sr-only">Language for the call</span>
          <select
            value={language}
            onChange={(e) => setLanguage(e.target.value)}
            aria-label="Language for the call"
            className="cursor-pointer rounded-full border bg-transparent px-2 py-1 text-xs"
            style={{ borderColor: "currentColor" }}
          >
            {CALL_LANGUAGES.map((l) => (
              <option key={l.code} value={l.code} style={{ color: "#2b2119" }}>
                {l.label}
              </option>
            ))}
          </select>
        </label>
      </span>
      {askDetails ? (
        <div className="mt-2 w-full max-w-sm rounded-xl border border-border bg-card text-foreground shadow-lg">
          <VisitorForm
            submitLabel="Start call"
            intro="Before the call, tell us who you are so the receptionist can help you faster."
            onSubmit={(v) => {
              setVisitor(v);
              void start(v);
            }}
          />
        </div>
      ) : null}
      {state === "error" && error ? (
        <span className="text-xs text-destructive">{error}</span>
      ) : null}
    </span>
  );
}

/** Dark text on light accents (e.g. tan), white on dark ones, so the label stays readable. */
function readableOn(hex: string): string {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return "#ffffff";
  const n = parseInt(m[1], 16);
  const [r, g, b] = [(n >> 16) & 255, (n >> 8) & 255, n & 255].map((c) => {
    const v = c / 255;
    return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b > 0.3 ? "#2b2119" : "#ffffff";
}
