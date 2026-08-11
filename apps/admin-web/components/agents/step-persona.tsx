"use client";

import { Label, Textarea } from "@/components/ui/input";
import { VoicePicker } from "./voice-picker";
import type { AgentDraft } from "./agent-draft";

/** Wizard step 3 / edit section — persona prompt + Piper voice & language. */
export function StepPersona({
  draft,
  patch,
}: {
  draft: AgentDraft;
  patch: (p: Partial<AgentDraft>) => void;
}) {
  return (
    <div className="grid gap-4">
      <div className="grid gap-1.5">
        <Label htmlFor="agent-persona">Persona</Label>
        <Textarea
          id="agent-persona"
          value={draft.persona}
          onChange={(e) => patch({ persona: e.target.value })}
          placeholder="e.g. You are Amara, the friendly front-desk receptionist for Adeyemi Clinic. Speak calmly, keep answers short, and always offer to book an appointment."
          className="min-h-28"
        />
        <p className="text-xs text-muted-foreground">
          Replaces the industry-pack persona when set (definition.persona,
          SPEC-W38 F2).
        </p>
      </div>
      <VoicePicker
        voiceId={draft.voiceId}
        language={draft.language}
        onVoiceChange={(voiceId) => patch({ voiceId })}
        onLanguageChange={(language) => patch({ language })}
      />
    </div>
  );
}
