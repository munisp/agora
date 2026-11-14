"use client";

import * as React from "react";
import Link from "next/link";
import { Mic, MicOff, PhoneOff } from "lucide-react";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

const LIVEKIT_URL = process.env.NEXT_PUBLIC_LIVEKIT_URL ?? "ws://localhost:7880";

type CallState = "joining" | "connected" | "ended" | "error";

/**
 * Warm-styled avatar tile (SPEC-W9 Part A). Renders the remote video track
 * an avatar provider (Tavus or the open avatar-renderer sidecar) publishes
 * into the LiveKit room. Audio-only rooms never produce a video track, so
 * nothing renders and the audio fallback is unchanged.
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
 * Staff warm-handoff join page (innovation 1). Reached from the
 * EscalationRequested dashboard toast with the LiveKit room name and the
 * staff join token minted by the voice runtime.
 */
export function CallClient({
  orgSlug,
  room,
  token,
}: {
  orgSlug: string;
  room: string;
  token: string;
}) {
  const [state, setState] = React.useState<CallState>("joining");
  const [muted, setMuted] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [avatarTrack, setAvatarTrack] =
    React.useState<import("livekit-client").RemoteVideoTrack | null>(null);
  const roomRef = React.useRef<import("livekit-client").Room | null>(null);

  React.useEffect(() => {
    if (!room || !token) {
      setError("Missing room or join token — open this page from an escalation toast.");
      setState("error");
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const { Room, RoomEvent, Track } = await import("livekit-client");
        const lk = new Room();
        roomRef.current = lk;
        lk.on(RoomEvent.Disconnected, () => {
          setAvatarTrack(null);
          setState("ended");
        });
        // SPEC-W9 Part A: avatar presence — render a provider-published
        // remote VIDEO track as the avatar tile when one appears.
        lk.on(RoomEvent.TrackSubscribed, (track) => {
          if (track.kind === Track.Kind.Video) {
            setAvatarTrack(track as import("livekit-client").RemoteVideoTrack);
          }
        });
        lk.on(RoomEvent.TrackUnsubscribed, (track) => {
          if (track.kind === Track.Kind.Video) setAvatarTrack(null);
        });
        await lk.connect(LIVEKIT_URL, token);
        await lk.localParticipant.setMicrophoneEnabled(true);
        if (!cancelled) setState("connected");
      } catch (e) {
        if (!cancelled) {
          setError(
            `Could not join the escalation room: ${e instanceof Error ? e.message : String(e)}. Check microphone permissions and that LiveKit is running.`,
          );
          setState("error");
        }
      }
    })();
    return () => {
      cancelled = true;
      void roomRef.current?.disconnect();
      roomRef.current = null;
    };
  }, [room, token]);

  const toggleMute = async () => {
    const lk = roomRef.current;
    if (!lk) return;
    const next = !muted;
    await lk.localParticipant.setMicrophoneEnabled(!next);
    setMuted(next);
  };

  const hangUp = async () => {
    await roomRef.current?.disconnect();
    roomRef.current = null;
    setState("ended");
  };

  return (
    <div className="max-w-xl">
      <PageHeader
        title="Escalation call"
        description={`Warm handoff from the AI receptionist · room ${room || "—"}`}
      />
      {error ? <ErrorNote message={error} /> : null}
      <Card>
        <CardHeader>
          <CardTitle>
            {state === "connected"
              ? "You are live with the caller"
              : state === "joining"
                ? "Connecting…"
                : state === "ended"
                  ? "Call ended"
                  : "Could not join"}
          </CardTitle>
          <CardDescription>
            {state === "connected"
              ? "The AI receptionist stays on the line as whisper-copilot with suggested replies."
              : "The caller was told a human is joining."}
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col items-start gap-3">
          {/* Avatar tile above the call controls when a provider publishes
              video into the room; audio-only rooms skip this entirely. */}
          {state === "connected" && avatarTrack ? (
            <AvatarVideoTile track={avatarTrack} />
          ) : null}
          <div className="flex items-center gap-3">
            {state === "connected" ? (
              <>
                <Button variant="outline" onClick={() => void toggleMute()}>
                  {muted ? <MicOff className="h-4 w-4" /> : <Mic className="h-4 w-4" />}
                  {muted ? "Unmute" : "Mute"}
                </Button>
                <Button variant="destructive" onClick={() => void hangUp()}>
                  <PhoneOff className="h-4 w-4" /> End call
                </Button>
              </>
            ) : state === "ended" || state === "error" ? (
              <Link href={`/app/${orgSlug}/bookings`}>
                <Button variant="outline">Back to bookings</Button>
              </Link>
            ) : null}
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
