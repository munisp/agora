"use client";

import * as React from "react";

/**
 * The receptionist's face during a call: an illustrated receptionist whose mouth follows the live voice (loudness of
 * the remote audio track, measured in the browser with an AnalyserNode), who blinks, and nods a little while talking.
 * Needs no server: it replaces the placeholder "avatar" video tile (the repo's lip-sync renderer was a stub that only
 * published a plain coloured frame). A real lip-synced video track, when one exists, is still shown instead.
 */
export function AnimatedReceptionist({
  track,
  accent = "#7c5b3e",
  name = "Receptionist",
}: {
  /** The receptionist's remote audio track (livekit RemoteAudioTrack); null until it arrives. */
  track: { mediaStreamTrack: MediaStreamTrack } | null;
  accent?: string;
  name?: string;
}) {
  const mouthRef = React.useRef<SVGEllipseElement | null>(null);
  const headRef = React.useRef<SVGGElement | null>(null);
  const [speaking, setSpeaking] = React.useState(false);

  React.useEffect(() => {
    if (!track) return;
    const AudioCtx = window.AudioContext ?? (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext;
    if (!AudioCtx) return;
    const ctx = new AudioCtx();
    const source = ctx.createMediaStreamSource(new MediaStream([track.mediaStreamTrack]));
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 512;
    analyser.smoothingTimeConstant = 0.6;
    source.connect(analyser); // analysis only: playback happens through the call's own <audio> element
    const data = new Uint8Array(analyser.fftSize);
    let level = 0;
    let lastLoud = 0;
    let shown = false;
    let raf = 0;
    const tick = (t: number) => {
      analyser.getByteTimeDomainData(data);
      let sum = 0;
      for (let i = 0; i < data.length; i++) {
        const v = (data[i] - 128) / 128;
        sum += v * v;
      }
      const rms = Math.sqrt(sum / data.length);
      // Fast to open, slower to close: reads as talking, not flicker.
      const target = Math.min(1, rms * 7);
      level += (target - level) * (target > level ? 0.55 : 0.25);
      if (mouthRef.current) mouthRef.current.setAttribute("ry", String(1.2 + level * 9));
      if (headRef.current) {
        const nod = level > 0.08 ? Math.sin(t / 180) * 1.6 * level : 0;
        headRef.current.setAttribute("transform", `rotate(${nod.toFixed(2)} 60 70)`);
      }
      if (level > 0.08) lastLoud = t;
      const now = t - lastLoud < 350;
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

  return (
    <div
      className="flex items-center gap-3 rounded-2xl border px-3 py-2 shadow-sm"
      style={{ borderColor: "#e6d8c8", backgroundColor: "#f7f1ea" }}
      role="img"
      aria-label={`${name} ${speaking ? "speaking" : "listening"}`}
    >
      <svg width="88" height="88" viewBox="0 0 120 120" aria-hidden="true">
        <circle cx="60" cy="60" r="58" fill={accent} opacity="0.18" />
        <path d="M18 120c3-24 20-36 42-36s39 12 42 36z" fill={accent} />
        <g ref={headRef}>
          <rect x="52" y="74" width="16" height="14" rx="5" fill="#8a5a3c" />
          <ellipse cx="60" cy="56" rx="26" ry="29" fill="#9b6646" />
          <path d="M33 54c-2-22 12-34 27-34s30 10 28 33c-5-9-13-15-27-16-13 1-23 7-28 17z" fill="#2b1d16" />
          <ellipse cx="34" cy="60" rx="4" ry="6" fill="#8a5a3c" />
          <ellipse cx="86" cy="60" rx="4" ry="6" fill="#8a5a3c" />
          <path d="M44 48q6-4 11 0" stroke="#2b1d16" strokeWidth="2.4" fill="none" strokeLinecap="round" />
          <path d="M65 48q5-4 11 0" stroke="#2b1d16" strokeWidth="2.4" fill="none" strokeLinecap="round" />
          <g className="agora-blink">
            <ellipse cx="50" cy="57" rx="3.4" ry="3.6" fill="#1f140e" />
            <ellipse cx="70" cy="57" rx="3.4" ry="3.6" fill="#1f140e" />
          </g>
          <path d="M58 61q2 6 4 0" stroke="#7a4a30" strokeWidth="1.6" fill="none" strokeLinecap="round" />
          <ellipse ref={mouthRef} cx="60" cy="72" rx="7" ry="1.2" fill="#5a2a1e" />
          <path d="M31 58c0-20 13-32 29-32s29 12 29 32" stroke="#3a3a3a" strokeWidth="3" fill="none" />
          <rect x="26" y="54" width="8" height="13" rx="3" fill="#3a3a3a" />
          <rect x="86" y="54" width="8" height="13" rx="3" fill="#3a3a3a" />
          <path d="M30 66q2 12 18 11" stroke="#3a3a3a" strokeWidth="2" fill="none" />
          <circle cx="49" cy="77" r="2.4" fill="#3a3a3a" />
        </g>
      </svg>
      <div className="min-w-0">
        <p className="text-sm font-medium" style={{ color: "#2b1d16" }}>
          {name}
        </p>
        <p className="text-xs" style={{ color: "#7a6656" }}>
          {track ? (speaking ? "Speaking…" : "Listening…") : "Connecting…"}
        </p>
      </div>
      <style>{`.agora-blink{transform-origin:60px 57px;animation:agora-blink 4.2s infinite}
@keyframes agora-blink{0%,92%,100%{transform:scaleY(1)}95%{transform:scaleY(0.1)}}
@media (prefers-reduced-motion: reduce){.agora-blink{animation:none}}`}</style>
    </div>
  );
}
