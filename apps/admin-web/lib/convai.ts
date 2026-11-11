/**
 * Minimal ElevenLabs Conversational-AI client over the native WebSocket
 * API (SPEC-W47 UIMOB-10) — no new npm dependency. The voice runtime
 * (AGENT_BACKEND=elevenlabs) mints a ConvAI signed URL server-side and the
 * browser connects to it directly:
 *
 *   server → client events (JSON, discriminated by `type`):
 *     conversation_initiation_metadata  — session up
 *     audio          { audio_event: { audio_base_64 } }        — PCM16 agent audio
 *     agent_response { agent_response_event: { agent_response } }
 *     user_transcript{ user_transcription_event: { user_transcript } }
 *     ping           { ping_event: { event_id } }              — must be ponged
 *     interruption   { interruption_event }                    — drop playback
 *   client → server:
 *     { "user_audio_chunk": <base64 PCM16 mono> }              — mic audio
 *     { "type": "pong", "event_id": <ping id> }
 */

export interface ConvAiCallbacks {
  onStatusChange?: (status: "connecting" | "connected" | "ended") => void;
  onAgentText?: (text: string) => void;
  onUserText?: (text: string) => void;
  onError?: (message: string) => void;
}

/** PCM16 mono sample rate used by the ConvAI websocket stream. */
const SAMPLE_RATE = 16000;

/**
 * Source type of AudioBuffer.copyToChannel, resolved from the DOM lib so the
 * cast below stays valid across the TS 5.7 TypedArray-generics boundary
 * (plain Float32Array on TS <= 5.6, Float32Array<ArrayBuffer> on TS >= 5.7).
 */
type CopyChannelSource = Parameters<AudioBuffer["copyToChannel"]>[0];

function floatTo16kPcmBase64(input: Float32Array, srcRate: number): string {
  // Downsample to 16 kHz by linear interpolation, then to PCM16 little-endian.
  const ratio = srcRate / SAMPLE_RATE;
  const outLen = Math.max(1, Math.floor(input.length / ratio));
  const bytes = new Uint8Array(outLen * 2);
  const view = new DataView(bytes.buffer);
  for (let i = 0; i < outLen; i++) {
    const pos = i * ratio;
    const idx = Math.floor(pos);
    const frac = pos - idx;
    const a = input[Math.min(idx, input.length - 1)];
    const b = input[Math.min(idx + 1, input.length - 1)];
    const sample = Math.max(-1, Math.min(1, a + (b - a) * frac));
    view.setInt16(i * 2, Math.round(sample * 0x7fff), true);
  }
  let bin = "";
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin);
}

function base64ToFloat32(b64: string): Float32Array {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  const view = new DataView(bytes.buffer);
  const out = new Float32Array(Math.floor(bytes.length / 2));
  for (let i = 0; i < out.length; i++) out[i] = view.getInt16(i * 2, true) / 0x8000;
  return out;
}

export class ElevenLabsConvAiSession {
  private ws: WebSocket | null = null;
  private audioCtx: AudioContext | null = null;
  private playbackCtx: AudioContext | null = null;
  private micStream: MediaStream | null = null;
  private nextPlayTime = 0;
  private stopped = false;

  constructor(
    private readonly signedUrl: string,
    private readonly cb: ConvAiCallbacks = {},
  ) {}

  async start(): Promise<void> {
    this.stopped = false;
    this.cb.onStatusChange?.("connecting");

    // Microphone first so a permission denial never opens a dead socket.
    this.micStream = await navigator.mediaDevices.getUserMedia({
      audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true },
    });

    await new Promise<void>((resolve, reject) => {
      const ws = new WebSocket(this.signedUrl);
      this.ws = ws;
      ws.onopen = () => resolve();
      ws.onerror = () => reject(new Error("Could not reach the voice agent."));
      ws.onclose = () => {
        if (!this.stopped) this.cb.onStatusChange?.("ended");
      };
      ws.onmessage = (ev) => this.handleMessage(ev);
    });

    // Mic capture: ScriptProcessorNode is deprecated but universal and needs
    // no worklet module plumbing — sufficient for a 16 kHz mono stream.
    this.audioCtx = new AudioContext();
    const srcRate = this.audioCtx.sampleRate;
    const source = this.audioCtx.createMediaStreamSource(this.micStream);
    const processor = this.audioCtx.createScriptProcessor(4096, 1, 1);
    processor.onaudioprocess = (ev) => {
      const ws = this.ws;
      if (!ws || ws.readyState !== WebSocket.OPEN) return;
      const chunk = floatTo16kPcmBase64(
        ev.inputBuffer.getChannelData(0),
        srcRate,
      );
      try {
        ws.send(JSON.stringify({ user_audio_chunk: chunk }));
      } catch {
        // socket closing — the next process tick is harmless
      }
    };
    source.connect(processor);
    processor.connect(this.audioCtx.destination);

    this.playbackCtx = new AudioContext({ sampleRate: SAMPLE_RATE });
    this.nextPlayTime = this.playbackCtx.currentTime;
    this.cb.onStatusChange?.("connected");
  }

  private handleMessage(ev: MessageEvent) {
    if (typeof ev.data !== "string") return;
    let msg: Record<string, unknown>;
    try {
      msg = JSON.parse(ev.data) as Record<string, unknown>;
    } catch {
      return;
    }
    const type = typeof msg.type === "string" ? msg.type : undefined;
    switch (type) {
      case "ping": {
        const eventId = (msg.ping_event as { event_id?: unknown } | undefined)
          ?.event_id;
        this.ws?.send(JSON.stringify({ type: "pong", event_id: eventId }));
        break;
      }
      case "audio": {
        const b64 = (msg.audio_event as { audio_base_64?: unknown } | undefined)
          ?.audio_base_64;
        if (typeof b64 === "string") this.playAudio(b64);
        break;
      }
      case "interruption": {
        // Drop queued agent audio immediately.
        this.nextPlayTime = this.playbackCtx?.currentTime ?? 0;
        break;
      }
      case "agent_response": {
        const text = (
          msg.agent_response_event as { agent_response?: unknown } | undefined
        )?.agent_response;
        if (typeof text === "string" && text.trim()) this.cb.onAgentText?.(text);
        break;
      }
      case "user_transcript": {
        const text = (
          msg.user_transcription_event as { user_transcript?: unknown } | undefined
        )?.user_transcript;
        if (typeof text === "string" && text.trim()) this.cb.onUserText?.(text);
        break;
      }
      case "error": {
        this.cb.onError?.("The voice agent reported an error.");
        break;
      }
      default:
        break;
    }
  }

  private playAudio(b64: string) {
    const ctx = this.playbackCtx;
    if (!ctx) return;
    let samples: Float32Array;
    try {
      samples = base64ToFloat32(b64);
    } catch {
      return;
    }
    if (samples.length === 0) return;
    const buffer = ctx.createBuffer(1, samples.length, SAMPLE_RATE);
    // CopyChannelSource keeps this compiling on both sides of the TS 5.7
    // TypedArray-generics boundary: it resolves to plain Float32Array on
    // TS <= 5.6 and Float32Array<ArrayBuffer> on TS >= 5.7 (W48 gate pins
    // tsc 5.6.3 while the repo toolchain is newer).
    buffer.copyToChannel(samples as CopyChannelSource, 0);
    const source = ctx.createBufferSource();
    source.buffer = buffer;
    source.connect(ctx.destination);
    const startAt = Math.max(this.nextPlayTime, ctx.currentTime);
    source.start(startAt);
    this.nextPlayTime = startAt + buffer.duration;
  }

  async stop(): Promise<void> {
    this.stopped = true;
    try {
      this.ws?.close();
    } catch {
      /* noop */
    }
    this.ws = null;
    this.micStream?.getTracks().forEach((t) => t.stop());
    this.micStream = null;
    try {
      await this.audioCtx?.close();
      await this.playbackCtx?.close();
    } catch {
      /* noop */
    }
    this.audioCtx = null;
    this.playbackCtx = null;
    this.cb.onStatusChange?.("ended");
  }
}
