"use client";

import * as React from "react";
import { AnimatedReceptionist } from "@/components/animated-receptionist";

/** What the voice worker says the receptionist is doing (data topic "agora.activity", app/call_activity.py). */
export interface CallActivity {
  state: "speaking" | "listening" | "working" | string;
  tool?: string;
  label?: string;
}

type Clip = "listening" | "talking" | "typing";
const CLIPS: Clip[] = ["listening", "talking", "typing"];
const BASE = "/receptionist";
// A tool call that never reports back must not leave her typing for the rest of the call.
const WORKING_MAX_MS = 25_000;

/**
 * The receptionist on camera during a call: a person at a front desk who talks while the receptionist speaks, types
 * while it is using a tool (checking availability, looking up a booking — the label says which) and listens otherwise.
 *
 * Three short looping clips (public/receptionist/*.mp4, made from one portrait with an image-to-video model) are
 * preloaded and cross-faded, so switching is instant. Speaking is read from the live voice's loudness; tool use comes
 * from the worker's activity messages. If the clips cannot play, the illustrated receptionist is shown instead.
 */
export function ReceptionistVideo({
  track,
  activity,
  accent = "#7c5b3e",
  name = "Receptionist",
}: {
  /** The receptionist's remote audio track; null until it arrives. */
  track: { mediaStreamTrack: MediaStreamTrack } | null;
  activity: CallActivity | null;
  accent?: string;
  name?: string;
}) {
  const [speaking, setSpeaking] = React.useState(false);
  const [working, setWorking] = React.useState<string | null>(null);
  const [failed, setFailed] = React.useState(false);
  const videos = React.useRef<Partial<Record<Clip, HTMLVideoElement | null>>>({});

  // Loudness of the receptionist's voice -> speaking (fast to start, holds 400 ms so pauses between words don't flicker).
  React.useEffect(() => {
    if (!track) return;
    const AudioCtx =
      window.AudioContext ?? (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext;
    if (!AudioCtx) return;
    const ctx = new AudioCtx();
    const source = ctx.createMediaStreamSource(new MediaStream([track.mediaStreamTrack]));
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 512;
    source.connect(analyser); // analysis only: the call's own <audio> element plays it
    const data = new Uint8Array(analyser.fftSize);
    let lastLoud = -1e9;
    let shown = false;
    let raf = 0;
    const tick = (t: number) => {
      analyser.getByteTimeDomainData(data);
      let sum = 0;
      for (let i = 0; i < data.length; i++) {
        const v = (data[i] - 128) / 128;
        sum += v * v;
      }
      if (Math.sqrt(sum / data.length) > 0.02) lastLoud = t;
      const now = t - lastLoud < 400;
      if (now !== shown) {
        shown = now;
        setSpeaking(now);
      }
      raf = requestAnimationFrame(tick);
    };
    void ctx.resume().catch(() => undefined);
    raf = requestAnimationFrame(tick);
    return () => {
      cancelAnimationFrame(raf);
      source.disconnect();
      void ctx.close().catch(() => undefined);
    };
  }, [track]);

  // Tool use from the worker; cleared when it reports back (or after WORKING_MAX_MS).
  React.useEffect(() => {
    if (activity?.state !== "working") {
      setWorking(null);
      return;
    }
    setWorking(activity.label || "Looking that up");
    const timer = window.setTimeout(() => setWorking(null), WORKING_MAX_MS);
    return () => window.clearTimeout(timer);
  }, [activity]);

  const clip: Clip = working ? "typing" : speaking ? "talking" : "listening";

  // Only the visible clip plays (saves the phone's battery); each restarts from where it was.
  React.useEffect(() => {
    for (const c of CLIPS) {
      const v = videos.current[c];
      if (!v) continue;
      if (c === clip) void v.play().catch(() => undefined);
      else v.pause();
    }
  }, [clip]);

  if (failed) return <AnimatedReceptionist track={track} accent={accent} name={name} />;

  const status = !track ? "Connecting…" : working ? `${working}…` : speaking ? "Speaking…" : "Listening…";
  return (
    <div
      className="w-[280px] max-w-full overflow-hidden rounded-2xl border shadow-sm"
      style={{ borderColor: "#e6d8c8", backgroundColor: "#f7f1ea" }}
      role="img"
      aria-label={`${name}: ${status.replace("…", "")}`}
      data-clip={clip}
    >
      <div className="relative aspect-[4/3] w-full bg-black">
        {CLIPS.map((c) => (
          <video
            key={c}
            ref={(el) => {
              videos.current[c] = el;
            }}
            src={`${BASE}/${c}.mp4`}
            poster={c === "listening" ? `${BASE}/poster.jpg` : undefined}
            muted
            loop
            playsInline
            preload="auto"
            autoPlay={c === "listening"}
            onError={() => setFailed(true)}
            className="absolute inset-0 h-full w-full object-cover transition-opacity duration-300"
            style={{ opacity: c === clip ? 1 : 0 }}
          />
        ))}
      </div>
      <div className="flex items-center justify-between gap-2 px-3 py-2">
        <p className="text-sm font-medium" style={{ color: "#2b1d16" }}>
          {name}
        </p>
        <p className="flex items-center gap-1.5 text-xs" style={{ color: "#7a6656" }}>
          {working ? <span className="agora-typing" aria-hidden="true"><i /><i /><i /></span> : null}
          {status}
        </p>
      </div>
      <style>{`.agora-typing{display:inline-flex;gap:2px}.agora-typing i{width:4px;height:4px;border-radius:9999px;background:${accent};animation:agora-dot 1s infinite}
.agora-typing i:nth-child(2){animation-delay:.15s}.agora-typing i:nth-child(3){animation-delay:.3s}
@keyframes agora-dot{0%,60%,100%{opacity:.25}30%{opacity:1}}
@media (prefers-reduced-motion: reduce){.agora-typing i{animation:none;opacity:.7}}`}</style>
    </div>
  );
}
