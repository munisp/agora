"use client";

import * as React from "react";
import { api } from "@/lib/api";
import { Label, Select } from "@/components/ui/input";
import { STATIC_VOICES } from "./agent-draft";
import type { VoicesCatalog } from "@/lib/types";

interface VoiceOption {
  id: string;
  languages: string[];
}

/**
 * SPEC-W38 F4 — Piper voice picker. Starts from the curated static list and
 * merges in voices reported by the voice runtime (GET /voice/voices, same
 * same-origin rewrite the voices studio uses). When the runtime is
 * unreachable the static list stays usable — the fetch fails silently.
 */
export function VoicePicker({
  voiceId,
  language,
  onVoiceChange,
  onLanguageChange,
}: {
  voiceId: string;
  language: string;
  onVoiceChange: (voiceId: string) => void;
  onLanguageChange: (language: string) => void;
}) {
  const [voices, setVoices] = React.useState<VoiceOption[]>(
    STATIC_VOICES.map((v) => ({ id: v.id, languages: [v.language] })),
  );

  React.useEffect(() => {
    const controller = new AbortController();
    (async () => {
      try {
        const catalog = await api.get<VoicesCatalog>(
          "/voice/voices",
          undefined,
          controller.signal,
        );
        const piper = (catalog.providers ?? []).find(
          (p) => p.name.toLowerCase() === "piper" && p.available,
        );
        if (!piper || piper.voices.length === 0) return;
        setVoices((current) => {
          const byId = new Map(current.map((v) => [v.id, v]));
          for (const v of piper.voices) {
            const existing = byId.get(v.id);
            if (existing) {
              existing.languages = [
                ...new Set([...existing.languages, ...(v.languages ?? [])]),
              ];
            } else {
              byId.set(v.id, {
                id: v.id,
                languages: v.languages?.length ? v.languages : ["en"],
              });
            }
          }
          return [...byId.values()];
        });
      } catch {
        // Runtime offline or route missing — the static list is enough.
      }
    })();
    return () => controller.abort();
  }, []);

  const languageOptions = React.useMemo(() => {
    const selected = voices.find((v) => v.id === voiceId);
    const langs = selected
      ? selected.languages
      : [...new Set(voices.flatMap((v) => v.languages))];
    const sorted = [...langs].sort((a, b) => a.localeCompare(b));
    return sorted.length > 0 ? sorted : ["en"];
  }, [voices, voiceId]);

  React.useEffect(() => {
    if (language && !languageOptions.includes(language)) {
      onLanguageChange(languageOptions[0]);
    }
  }, [language, languageOptions, onLanguageChange]);

  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <div className="grid gap-1.5">
        <Label htmlFor="agent-voice">Voice</Label>
        <Select
          id="agent-voice"
          value={voiceId}
          onChange={(e) => onVoiceChange(e.target.value)}
        >
          {voices.map((v) => (
            <option key={v.id} value={v.id}>
              {v.id}
            </option>
          ))}
        </Select>
        <p className="text-xs text-muted-foreground">
          Piper voices from the voice runtime, or the curated defaults when it
          is offline.
        </p>
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="agent-language">Language</Label>
        <Select
          id="agent-language"
          value={language}
          onChange={(e) => onLanguageChange(e.target.value)}
        >
          {languageOptions.map((l) => (
            <option key={l} value={l}>
              {l}
            </option>
          ))}
        </Select>
      </div>
    </div>
  );
}
