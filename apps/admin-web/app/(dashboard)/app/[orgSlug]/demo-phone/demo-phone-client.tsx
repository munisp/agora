"use client";

/**
 * Demo phone (2026-11-22): rings when a dashboard call button is used with "Ring the demo phone", and on Answer joins
 * the outbound call room as the contributor. Backend: voice-agent-runtime /voice-admin/demo-phone/{ring,answer,decline}
 * (same signed-in voice-admin route as the call buttons). The receptionist on the other end is the real outbound one.
 */
import * as React from "react";
import { Mic, MicOff, Phone, PhoneOff, Power } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { playRemoteAudio, unlockCallAudio } from "@/lib/call-audio";

type State = "off" | "waiting" | "ringing" | "connecting" | "in_call" | "ended";

interface Ring {
  ringing: boolean;
  call_id?: string;
  contact_name?: string;
  phone_number?: string;
  purpose?: string;
}

interface Answer {
  room: string;
  url: string;
  token: string;
  ice_servers?: RTCIceServer[];
}

const PURPOSE_HINT: Record<string, string> = {
  confirm_appointment: "Appointment confirmation",
  waitlist_offer: "Waitlist slot offer",
  no_show_rebook: "Missed appointment",
  callback: "Call back",
  general: "Call",
};

/** A phone-style ring (two tones, 2 s on / 4 s off) on Web Audio; vibrates too where the device can. */
function startRinging(ctx: AudioContext): () => void {
  let stopped = false;
  const gain = ctx.createGain();
  gain.gain.value = 0;
  gain.connect(ctx.destination);
  const oscs = [440, 480].map((f) => {
    const o = ctx.createOscillator();
    o.frequency.value = f;
    o.connect(gain);
    o.start();
    return o;
  });
  const cycle = () => {
    if (stopped) return;
    const t = ctx.currentTime;
    gain.gain.setValueAtTime(0.12, t);
    gain.gain.setValueAtTime(0, t + 2);
    navigator.vibrate?.([800, 200, 800]);
  };
  cycle();
  const timer = window.setInterval(cycle, 6000);
  return () => {
    stopped = true;
    window.clearInterval(timer);
    gain.gain.cancelScheduledValues(ctx.currentTime);
    gain.gain.setValueAtTime(0, ctx.currentTime);
    oscs.forEach((o) => o.stop());
    navigator.vibrate?.(0);
  };
}

export function DemoPhoneClient({ orgSlug, businessName }: { orgSlug: string; businessName: string }) {
  const [state, setState] = React.useState<State>("off");
  const [ring, setRing] = React.useState<Ring | null>(null);
  const [error, setError] = React.useState<string | null>(null);
  const [muted, setMuted] = React.useState(false);
  const [seconds, setSeconds] = React.useState(0);
  const [captions, setCaptions] = React.useState<{ role: string; text: string }[]>([]);
  const audioCtx = React.useRef<AudioContext | null>(null);
  const stopRing = React.useRef<(() => void) | null>(null);
  const roomRef = React.useRef<import("livekit-client").Room | null>(null);
  // Where the receptionist's WhatsApp confirmation goes during a demo (the demoer's own phone). Remembered here.
  const [whatsapp, setWhatsapp] = React.useState("");
  React.useEffect(() => {
    try {
      setWhatsapp(window.localStorage.getItem("agora.demoPhoneWhatsApp") ?? "");
    } catch {
      /* storage blocked */
    }
  }, []);
  const saveWhatsapp = (v: string) => {
    setWhatsapp(v);
    try {
      window.localStorage.setItem("agora.demoPhoneWhatsApp", v);
    } catch {
      /* ignore */
    }
  };

  const silence = () => {
    stopRing.current?.();
    stopRing.current = null;
  };

  // Poll for a ring while waiting (every 2 s).
  React.useEffect(() => {
    if (state !== "waiting") return;
    let cancelled = false;
    const tick = async () => {
      try {
        const r = await api.get<Ring>("/api/voice-admin/demo-phone/ring", { tenant: orgSlug });
        if (!cancelled && r.ringing && r.call_id) {
          setRing(r);
          setState("ringing");
        }
      } catch (e) {
        if (!cancelled) setError(e instanceof ApiError ? e.message : "Lost contact with the server; still trying.");
      }
    };
    void tick();
    const id = window.setInterval(() => void tick(), 2000);
    return () => {
      cancelled = true;
      window.clearInterval(id);
    };
  }, [state, orgSlug]);

  // Ring while ringing; give up after 45 s like the server does.
  React.useEffect(() => {
    if (state !== "ringing" || !audioCtx.current) return;
    stopRing.current = startRinging(audioCtx.current);
    const giveUp = window.setTimeout(() => {
      silence();
      setRing(null);
      setState("waiting");
    }, 45000);
    return () => {
      window.clearTimeout(giveUp);
      silence();
    };
  }, [state]);

  // Call timer.
  React.useEffect(() => {
    if (state !== "in_call") return;
    setSeconds(0);
    const id = window.setInterval(() => setSeconds((s) => s + 1), 1000);
    return () => window.clearInterval(id);
  }, [state]);

  const switchOn = async () => {
    setError(null);
    try {
      // Ask for the microphone now (a tap is needed anyway to allow sound), so Answer connects at once.
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      stream.getTracks().forEach((t) => t.stop());
      audioCtx.current = audioCtx.current ?? new AudioContext();
      await audioCtx.current.resume();
      setState("waiting");
    } catch {
      setError("Allow the microphone to use the demo phone.");
    }
  };

  const decline = async () => {
    silence();
    const id = ring?.call_id;
    setRing(null);
    setState("waiting");
    if (id) await api.post("/api/voice-admin/demo-phone/decline", { call_id: id }, { tenant: orgSlug }).catch(() => undefined);
  };

  const answer = async () => {
    if (!ring?.call_id) return;
    silence();
    setState("connecting");
    setCaptions([]);
    setError(null);
    try {
      const a = await api.post<Answer>(
        "/api/voice-admin/demo-phone/answer",
        { call_id: ring.call_id, ...(whatsapp.trim() ? { whatsapp: whatsapp.trim() } : {}) },
        { tenant: orgSlug },
      );
      const { Room, RoomEvent, Track } = await import("livekit-client");
      const room = new Room();
      roomRef.current = room;
      playRemoteAudio(room, { RoomEvent, Track });
      room.on(RoomEvent.Disconnected, () => {
        roomRef.current = null;
        setState("ended");
      });
      // The receptionist leaving (it says goodbye and ends the call) ends the call here too.
      room.on(RoomEvent.ParticipantDisconnected, () => void room.disconnect());
      room.on(RoomEvent.DataReceived, (payload, _p, _k, topic) => {
        if (topic !== "agora.activity") return;
        try {
          const msg = JSON.parse(new TextDecoder().decode(payload)) as { state?: string; role?: string; text?: string };
          if (msg.state !== "caption" || !msg.text) return;
          const role = msg.role ?? "";
          const text = msg.text;
          setCaptions((prev) => {
            const last = prev[prev.length - 1];
            if (last && last.role === role) {
              const joined = `${last.text} ${text}`;
              return [...prev.slice(0, -1), { role, text: joined.length > 240 ? `…${joined.slice(-240)}` : joined }];
            }
            return [...prev.slice(-1), { role, text }];
          });
        } catch {
          /* not ours */
        }
      });
      await room.connect(a.url, a.token, a.ice_servers?.length ? { rtcConfig: { iceServers: a.ice_servers } } : undefined);
      await unlockCallAudio(room);
      await room.localParticipant.setMicrophoneEnabled(true, undefined, { dtx: false });
      setMuted(false);
      setState("in_call");
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Could not connect the call.");
      roomRef.current?.disconnect();
      roomRef.current = null;
      setState("waiting");
    }
  };

  const hangUp = () => void roomRef.current?.disconnect();

  const toggleMute = async () => {
    const room = roomRef.current;
    if (!room) return;
    await room.localParticipant.setMicrophoneEnabled(muted, undefined, { dtx: false });
    setMuted(!muted);
  };

  React.useEffect(() => () => {
    silence();
    void roomRef.current?.disconnect();
  }, []);

  const caller = businessName;
  const mmss = `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;

  return (
    <div className="flex min-h-[80vh] items-center justify-center p-4">
      <div className="flex h-[640px] w-full max-w-[360px] flex-col overflow-hidden rounded-[2.5rem] border-[10px] border-neutral-900 bg-gradient-to-b from-[#2b2119] to-[#1a140f] text-[#faf7f1] shadow-2xl">
        <div className="flex items-center justify-between px-6 pt-4 text-xs text-[#e3d6c3]/70">
          <span>Demo phone</span>
          <span>{state === "off" ? "off" : state === "in_call" ? mmss : "on"}</span>
        </div>

        <div className="flex flex-1 flex-col items-center px-6 pt-10 text-center">
          {state === "off" ? (
            <>
              <p className="text-lg font-semibold">Demo phone</p>
              <p className="mt-2 text-sm text-[#e3d6c3]/80">
                Stands in for a contributor&apos;s phone. Switch it on, then use a call button on the dashboard with
                &ldquo;Ring the demo phone&rdquo; ticked.
              </p>
              <button onClick={() => void switchOn()} className="mt-10 flex h-20 w-20 items-center justify-center rounded-full bg-[#c9a574] text-[#2b2119]" aria-label="Switch the demo phone on">
                <Power className="h-8 w-8" />
              </button>
              <p className="mt-3 text-xs text-[#e3d6c3]/60">Switch on (allows sound and the microphone)</p>
              <label className="mt-8 w-full text-left text-xs text-[#e3d6c3]/80">
                Your WhatsApp number, for the confirmation message (optional)
                <input
                  type="tel"
                  inputMode="tel"
                  value={whatsapp}
                  onChange={(e) => saveWhatsapp(e.target.value)}
                  placeholder="e.g. 0705 225 6260"
                  className="mt-1 w-full rounded-md border border-[#c9a574]/40 bg-black/20 px-3 py-2 text-sm text-[#faf7f1] placeholder:text-[#e3d6c3]/40"
                />
              </label>
            </>
          ) : null}

          {state === "waiting" ? (
            <>
              <div className="mt-10 h-3 w-3 animate-pulse rounded-full bg-emerald-400" />
              <p className="mt-4 text-lg font-semibold">Waiting for a call</p>
              <p className="mt-2 text-sm text-[#e3d6c3]/80">Keep this screen open. It rings when {caller} calls.</p>
              {whatsapp.trim() ? (
                <p className="mt-4 text-xs text-[#e3d6c3]/60">WhatsApp confirmations go to {whatsapp.trim()}</p>
              ) : null}
            </>
          ) : null}

          {state === "ringing" && ring ? (
            <>
              <p className="mt-6 text-xs uppercase tracking-widest text-[#c9a574]">Incoming call</p>
              <p className="mt-3 text-2xl font-semibold">{caller}</p>
              <p className="mt-1 text-sm text-[#e3d6c3]/80">{PURPOSE_HINT[ring.purpose ?? "general"] ?? "Call"}</p>
              <p className="mt-6 text-xs text-[#e3d6c3]/60">
                Answering as {ring.contact_name || "the contributor"} · {ring.phone_number}
              </p>
            </>
          ) : null}

          {state === "connecting" ? <p className="mt-10 text-lg">Connecting…</p> : null}

          {state === "in_call" ? (
            <>
              <p className="text-2xl font-semibold">{caller}</p>
              <p className="mt-1 text-sm text-[#e3d6c3]/80">{mmss}</p>
              <div className="mt-6 w-full space-y-2 text-left text-sm" aria-live="polite">
                {captions.map((c, i) => (
                  <p key={i} className={c.role === "caller" ? "text-[#c9a574]" : "text-[#faf7f1]"}>
                    <span className="mr-1 text-xs uppercase opacity-60">{c.role === "caller" ? "You" : "Receptionist"}</span>
                    {c.text}
                  </p>
                ))}
              </div>
            </>
          ) : null}

          {state === "ended" ? (
            <>
              <p className="mt-10 text-lg font-semibold">Call ended</p>
              <p className="mt-2 text-sm text-[#e3d6c3]/80">The transcript is in Conversations.</p>
              <button onClick={() => setState("waiting")} className="mt-8 rounded-full border border-[#c9a574] px-5 py-2 text-sm">
                Wait for the next call
              </button>
            </>
          ) : null}

          {error ? <p className="mt-6 text-xs text-red-300">{error}</p> : null}
        </div>

        <div className="flex items-center justify-around px-10 pb-10">
          {state === "ringing" ? (
            <>
              <button onClick={() => void decline()} className="flex h-16 w-16 items-center justify-center rounded-full bg-red-600" aria-label="Decline">
                <PhoneOff className="h-7 w-7" />
              </button>
              <button onClick={() => void answer()} className="flex h-16 w-16 items-center justify-center rounded-full bg-emerald-600 ring-4 ring-emerald-400/40 animate-pulse" aria-label="Answer">
                <Phone className="h-7 w-7" />
              </button>
            </>
          ) : null}
          {state === "in_call" ? (
            <>
              <button onClick={() => void toggleMute()} className="flex h-14 w-14 items-center justify-center rounded-full bg-white/10" aria-label={muted ? "Unmute" : "Mute"}>
                {muted ? <MicOff className="h-6 w-6" /> : <Mic className="h-6 w-6" />}
              </button>
              <button onClick={hangUp} className="flex h-16 w-16 items-center justify-center rounded-full bg-red-600" aria-label="Hang up">
                <PhoneOff className="h-7 w-7" />
              </button>
            </>
          ) : null}
        </div>
      </div>
    </div>
  );
}
