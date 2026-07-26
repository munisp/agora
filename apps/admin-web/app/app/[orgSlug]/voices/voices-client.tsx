"use client";

import * as React from "react";
import {
  AudioLines,
  Copy,
  Loader2,
  Mic,
  Play,
  RefreshCw,
  Upload,
} from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input, Label, Select, Textarea } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import type {
  VoiceEnrollResponse,
  VoiceInfo,
  VoicesCatalog,
} from "@/lib/types";

/**
 * Wave 10 (SPEC-W10 Part C) — "Voices" studio: browse the TTS provider
 * catalog, preview any voice, and enroll a brand voice for XTTS cloning.
 *
 * All endpoints are reached same-origin via the Next.js `/voice/:path*`
 * rewrite (see next.config.ts — the same path the call client uses for
 * POST /voice/session), which forwards to the voice runtime control plane:
 *   GET  /voice/voices        → { providers: [{ name, available, voices }] }
 *   POST /voice/tts-preview   { text, language?, provider?, voice? } → audio/wav
 *   POST /voice/voices/enroll { name, sample_base64, tenant } → { voice_id }
 */

const MAX_SAMPLE_BYTES = 5 * 1024 * 1024; // 5 MB
const SAMPLE_EXTENSIONS = ["wav", "mp3", "m4a"];

type LoadState = "loading" | "ready" | "error";

function collectLanguages(voices: VoiceInfo[]): string[] {
  const set = new Set<string>();
  for (const v of voices) for (const l of v.languages ?? []) set.add(l);
  return [...set].sort((a, b) => a.localeCompare(b));
}

function fileToBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(new Error("Could not read the audio file."));
    reader.onload = () => {
      const result = typeof reader.result === "string" ? reader.result : "";
      const comma = result.indexOf(",");
      resolve(comma >= 0 ? result.slice(comma + 1) : result);
    };
    reader.readAsDataURL(file);
  });
}

function VoiceCard({ voice }: { voice: VoiceInfo }) {
  return (
    <div className="rounded-lg border border-border bg-card p-3 shadow-sm">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-sm font-medium">{voice.id}</span>
        {voice.gender ? <Badge variant="secondary">{voice.gender}</Badge> : null}
        {(voice.labels ?? []).map((label) => (
          <Badge key={label} variant="info">
            {label}
          </Badge>
        ))}
      </div>
      <div className="mt-2 flex flex-wrap gap-1">
        {(voice.languages ?? []).map((lang) => (
          <Badge key={lang} variant="outline">
            {lang}
          </Badge>
        ))}
      </div>
    </div>
  );
}

export function VoicesClient({
  orgSlug,
  canEnroll,
}: {
  orgSlug: string;
  /** owner/admin — gates the brand-voice enrollment card */
  canEnroll: boolean;
}) {
  const { toast } = useToast();

  /* ---------------- Voice browser (GET /voice/voices) ---------------- */
  const [catalog, setCatalog] = React.useState<VoicesCatalog | null>(null);
  const [loadState, setLoadState] = React.useState<LoadState>("loading");
  const [loadError, setLoadError] = React.useState<string | null>(null);

  const loadCatalog = React.useCallback(async (signal?: AbortSignal) => {
    setLoadState("loading");
    setLoadError(null);
    try {
      const data = await api.get<VoicesCatalog>(
        "/voice/voices",
        undefined,
        signal,
      );
      setCatalog(data);
      setLoadState("ready");
    } catch (e) {
      if (e instanceof DOMException && e.name === "AbortError") return;
      setCatalog(null);
      setLoadState("error");
      setLoadError(
        e instanceof ApiError
          ? e.message
          : "The voice runtime is unreachable. Check that the voice-agent-runtime is running, then retry.",
      );
    }
  }, []);

  React.useEffect(() => {
    const controller = new AbortController();
    void loadCatalog(controller.signal);
    return () => controller.abort();
  }, [loadCatalog]);

  const providers = React.useMemo(() => catalog?.providers ?? [], [catalog]);
  const usableProviders = React.useMemo(
    () => providers.filter((p) => p.available && p.voices.length > 0),
    [providers],
  );
  const allUsableVoices = React.useMemo(
    () => usableProviders.flatMap((p) => p.voices),
    [usableProviders],
  );

  /* --------------------- Preview composer state ---------------------- */
  const [text, setText] = React.useState(
    "Welcome! How can I help you today?",
  );
  const [providerName, setProviderName] = React.useState("");
  const [voiceId, setVoiceId] = React.useState("");
  const [language, setLanguage] = React.useState("");
  const [previewing, setPreviewing] = React.useState(false);
  const [previewError, setPreviewError] = React.useState<string | null>(null);
  const [audioUrl, setAudioUrl] = React.useState<string | null>(null);
  const audioUrlRef = React.useRef<string | null>(null);

  const replaceAudioUrl = React.useCallback((next: string | null) => {
    if (audioUrlRef.current) URL.revokeObjectURL(audioUrlRef.current);
    audioUrlRef.current = next;
    setAudioUrl(next);
  }, []);

  // Revoke the object URL on unmount so blobs never leak.
  React.useEffect(() => {
    return () => {
      if (audioUrlRef.current) URL.revokeObjectURL(audioUrlRef.current);
    };
  }, []);

  // Dependent dropdowns: voice list follows the provider, languages follow
  // the selected voice (or the union of all selectable voices).
  const voiceOptions = React.useMemo<VoiceInfo[]>(() => {
    if (!providerName) return allUsableVoices;
    return (
      usableProviders.find((p) => p.name === providerName)?.voices ?? []
    );
  }, [providerName, usableProviders, allUsableVoices]);

  const languageOptions = React.useMemo(() => {
    if (!voiceId) return collectLanguages(voiceOptions);
    const voice = voiceOptions.find((v) => v.id === voiceId);
    return voice ? collectLanguages([voice]) : collectLanguages(voiceOptions);
  }, [voiceId, voiceOptions]);

  React.useEffect(() => {
    if (voiceId && !voiceOptions.some((v) => v.id === voiceId)) setVoiceId("");
  }, [voiceId, voiceOptions]);

  React.useEffect(() => {
    if (language && !languageOptions.includes(language)) setLanguage("");
  }, [language, languageOptions]);

  const runPreview = async () => {
    if (!text.trim()) {
      setPreviewError("Enter some text to preview.");
      return;
    }
    setPreviewing(true);
    setPreviewError(null);
    try {
      const body: Record<string, string> = { text: text.trim() };
      if (language) body.language = language;
      if (providerName) body.provider = providerName;
      if (voiceId) body.voice = voiceId;
      // Raw fetch (not the JSON api wrapper): the response is audio/wav bytes.
      const res = await fetch("/voice/tts-preview", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(body),
        cache: "no-store",
      });
      if (!res.ok) {
        let message = `Preview failed with status ${res.status}.`;
        try {
          const data = (await res.json()) as { message?: unknown; error?: unknown };
          if (typeof data.message === "string") message = data.message;
          else if (typeof data.error === "string") message = data.error;
        } catch {
          // Non-JSON error body — keep the status-line message.
        }
        throw new Error(message);
      }
      const blob = await res.blob();
      replaceAudioUrl(URL.createObjectURL(blob));
    } catch (e) {
      replaceAudioUrl(null);
      setPreviewError(
        e instanceof Error
          ? e.message
          : "Preview failed — the voice runtime may be offline.",
      );
    } finally {
      setPreviewing(false);
    }
  };

  /* ------------------- Brand-voice enrollment state ------------------ */
  const [voiceName, setVoiceName] = React.useState("");
  const [sampleFile, setSampleFile] = React.useState<File | null>(null);
  const [consent, setConsent] = React.useState(false);
  const [enrolling, setEnrolling] = React.useState(false);
  const [enrollError, setEnrollError] = React.useState<string | null>(null);
  const [enrolledVoiceId, setEnrolledVoiceId] = React.useState<string | null>(
    null,
  );

  const sampleError = React.useMemo(() => {
    if (!sampleFile) return null;
    const ext = sampleFile.name.split(".").pop()?.toLowerCase() ?? "";
    if (!SAMPLE_EXTENSIONS.includes(ext))
      return "Unsupported format — upload a .wav, .mp3 or .m4a file.";
    if (sampleFile.size > MAX_SAMPLE_BYTES)
      return "The sample is larger than 5 MB. Trim it to 6–30 seconds.";
    return null;
  }, [sampleFile]);

  const enroll = async () => {
    if (!sampleFile || sampleError) return;
    setEnrolling(true);
    setEnrollError(null);
    setEnrolledVoiceId(null);
    try {
      const sampleBase64 = await fileToBase64(sampleFile);
      const res = await api.post<VoiceEnrollResponse>("/voice/voices/enroll", {
        name: voiceName.trim(),
        sample_base64: sampleBase64,
        tenant: orgSlug,
      });
      setEnrolledVoiceId(res.voice_id);
      toast({
        title: "Brand voice enrolled",
        description: `Voice "${voiceName.trim()}" is now available to the XTTS provider.`,
        variant: "success",
      });
      // Refresh the catalog so the new voice shows up in the browser.
      void loadCatalog();
    } catch (e) {
      setEnrollError(
        e instanceof ApiError
          ? e.message
          : "Enrollment failed — the voice runtime or XTTS provider may be offline.",
      );
    } finally {
      setEnrolling(false);
    }
  };

  const copyVoiceId = async () => {
    if (!enrolledVoiceId) return;
    try {
      await navigator.clipboard.writeText(enrolledVoiceId);
      toast({ title: "Voice ID copied", variant: "success" });
    } catch {
      toast({
        title: "Copy failed",
        description: "Select the ID and copy it manually.",
        variant: "destructive",
      });
    }
  };

  /* ------------------------------ Render ----------------------------- */
  return (
    <div className="max-w-3xl">
      <PageHeader
        title="Voices"
        description="Browse the TTS provider catalog, preview voices, and enroll a brand voice."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => void loadCatalog()}
            disabled={loadState === "loading"}
          >
            <RefreshCw
              className={`h-3.5 w-3.5 ${loadState === "loading" ? "animate-spin" : ""}`}
            />
            Refresh
          </Button>
        }
      />

      {loadState === "error" && loadError ? (
        <ErrorNote
          message={`Could not load the voice catalog (${loadError}). The studio stays usable — fix the runtime and hit Refresh.`}
        />
      ) : null}

      <div className="space-y-6">
        {/* Voice browser */}
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <AudioLines className="h-4 w-4" /> Voice catalog
            </CardTitle>
            <CardDescription>
              Providers and voices reported by the voice runtime (GET
              /voice/voices). Unavailable providers are shown for reference —
              configure them and refresh.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            {loadState === "loading" ? (
              <p className="flex items-center gap-2 text-sm text-muted-foreground">
                <Loader2 className="h-4 w-4 animate-spin" /> Loading voices…
              </p>
            ) : providers.length === 0 ? (
              <div className="rounded-lg border border-dashed border-border bg-muted/50 p-6 text-center">
                <p className="text-sm font-medium">No voices configured</p>
                <p className="mt-1 text-xs text-muted-foreground">
                  Set <span className="font-mono">TTS_PROVIDER_CHAIN</span> on
                  the voice runtime (e.g. azure, mms, xtts, piper) and refresh.
                  See docs/voices.md.
                </p>
              </div>
            ) : (
              providers.map((provider) => (
                <div
                  key={provider.name}
                  className="rounded-lg border border-border bg-muted/40 p-3"
                >
                  <div className="mb-2 flex items-center justify-between gap-2">
                    <span className="font-mono text-sm font-semibold">
                      {provider.name}
                    </span>
                    {provider.available ? (
                      <Badge variant="success">available</Badge>
                    ) : (
                      <Badge variant="warning">unavailable</Badge>
                    )}
                  </div>
                  {provider.voices.length === 0 ? (
                    <p className="text-xs text-muted-foreground">
                      No voices reported by this provider.
                    </p>
                  ) : (
                    <div className="grid gap-2 sm:grid-cols-2">
                      {provider.voices.map((voice) => (
                        <VoiceCard
                          key={`${provider.name}:${voice.id}`}
                          voice={voice}
                        />
                      ))}
                    </div>
                  )}
                </div>
              ))
            )}
          </CardContent>
        </Card>

        {/* Preview composer */}
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Play className="h-4 w-4" /> Preview a voice
            </CardTitle>
            <CardDescription>
              Synthesize a short sample through the same fallback chain the
              voice agent uses (POST /voice/tts-preview).
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-4">
            <div className="grid gap-1.5">
              <Label htmlFor="preview-text">Text</Label>
              <Textarea
                id="preview-text"
                value={text}
                onChange={(e) => setText(e.target.value)}
                placeholder="Type something for the agent to say…"
              />
            </div>
            <div className="grid gap-4 sm:grid-cols-3">
              <div className="grid gap-1.5">
                <Label htmlFor="preview-provider">Provider</Label>
                <Select
                  id="preview-provider"
                  value={providerName}
                  onChange={(e) => setProviderName(e.target.value)}
                  disabled={usableProviders.length === 0}
                >
                  <option value="">Auto (fallback chain)</option>
                  {usableProviders.map((p) => (
                    <option key={p.name} value={p.name}>
                      {p.name}
                    </option>
                  ))}
                </Select>
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="preview-voice">Voice</Label>
                <Select
                  id="preview-voice"
                  value={voiceId}
                  onChange={(e) => setVoiceId(e.target.value)}
                  disabled={voiceOptions.length === 0}
                >
                  <option value="">Auto (default)</option>
                  {voiceOptions.map((v) => (
                    <option key={v.id} value={v.id}>
                      {v.id}
                    </option>
                  ))}
                </Select>
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="preview-language">Language</Label>
                <Select
                  id="preview-language"
                  value={language}
                  onChange={(e) => setLanguage(e.target.value)}
                  disabled={languageOptions.length === 0}
                >
                  <option value="">Auto</option>
                  {languageOptions.map((l) => (
                    <option key={l} value={l}>
                      {l}
                    </option>
                  ))}
                </Select>
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-3">
              <Button
                onClick={() => void runPreview()}
                disabled={previewing || !text.trim()}
              >
                {previewing ? (
                  <Loader2 className="h-4 w-4 animate-spin" />
                ) : (
                  <Play className="h-4 w-4" />
                )}
                {previewing ? "Synthesizing…" : "Preview"}
              </Button>
              {audioUrl ? (
                // eslint-disable-next-line jsx-a11y/media-has-caption -- synthesized speech preview has no captions
                <audio controls src={audioUrl} className="h-9 min-w-0 flex-1" />
              ) : null}
            </div>
            {previewError ? (
              <p className="text-sm text-destructive">{previewError}</p>
            ) : null}
          </CardContent>
        </Card>

        {/* Brand-voice enrollment (owner/admin only) */}
        {canEnroll ? (
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <Mic className="h-4 w-4" /> Enroll a brand voice
              </CardTitle>
              <CardDescription>
                Clone a voice for the XTTS provider from a clean reference
                sample (POST /voice/voices/enroll). Owner/admin only.
              </CardDescription>
            </CardHeader>
            <CardContent className="grid gap-4">
              <div className="grid gap-1.5">
                <Label htmlFor="voice-name">Voice name</Label>
                <Input
                  id="voice-name"
                  value={voiceName}
                  onChange={(e) => setVoiceName(e.target.value)}
                  placeholder="e.g. Amara — front desk"
                />
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="voice-sample">Reference sample</Label>
                <Input
                  id="voice-sample"
                  type="file"
                  accept=".wav,.mp3,.m4a,audio/wav,audio/mpeg,audio/mp4"
                  onChange={(e) =>
                    setSampleFile(e.target.files?.[0] ?? null)
                  }
                />
                <p className="text-xs text-muted-foreground">
                  6–30 seconds of clean speech, wav/mp3/m4a, up to 5 MB.
                </p>
                {sampleError ? (
                  <p className="text-xs text-destructive">{sampleError}</p>
                ) : null}
              </div>
              <label className="flex items-start gap-2 rounded-md border border-border bg-muted/40 p-3 text-sm">
                <input
                  type="checkbox"
                  checked={consent}
                  onChange={(e) => setConsent(e.target.checked)}
                  className="mt-0.5 h-4 w-4 accent-primary"
                />
                <span>
                  I confirm the speaker consented to voice cloning.
                  <span className="block text-xs text-muted-foreground">
                    Required under the Nigeria Data Protection Act (NDPA) —
                    voiceprints are biometric personal data. Keep a record of
                    the speaker&apos;s consent.
                  </span>
                </span>
              </label>
              {enrollError ? (
                <p className="text-sm text-destructive">{enrollError}</p>
              ) : null}
              <div className="flex flex-wrap items-center gap-3">
                <Button
                  onClick={() => void enroll()}
                  disabled={
                    enrolling ||
                    !consent ||
                    !voiceName.trim() ||
                    !sampleFile ||
                    !!sampleError
                  }
                >
                  {enrolling ? (
                    <Loader2 className="h-4 w-4 animate-spin" />
                  ) : (
                    <Upload className="h-4 w-4" />
                  )}
                  {enrolling ? "Enrolling…" : "Enroll voice"}
                </Button>
              </div>
              {enrolledVoiceId ? (
                <div className="flex items-center gap-2 rounded-md border border-success/40 bg-success-soft px-3 py-2">
                  <span className="text-sm text-success">
                    Enrolled voice ID:{" "}
                    <span className="font-mono font-medium">
                      {enrolledVoiceId}
                    </span>
                  </span>
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => void copyVoiceId()}
                  >
                    <Copy className="h-3.5 w-3.5" /> Copy
                  </Button>
                </div>
              ) : null}
            </CardContent>
          </Card>
        ) : (
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <Mic className="h-4 w-4" /> Brand voices
              </CardTitle>
              <CardDescription>
                Enrolling a cloned brand voice requires an owner or admin
                account. Ask your organisation owner to enroll one.
              </CardDescription>
            </CardHeader>
          </Card>
        )}
      </div>
    </div>
  );
}
