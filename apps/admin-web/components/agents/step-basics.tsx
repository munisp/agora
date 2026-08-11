"use client";

import { Input, Label, Textarea } from "@/components/ui/input";
import { slugify, type AgentDraft } from "./agent-draft";

/** Wizard step 1 / edit section — name, slug (auto-derived) and purpose. */
export function StepBasics({
  draft,
  patch,
  slugReadOnly = false,
}: {
  draft: AgentDraft;
  patch: (p: Partial<AgentDraft>) => void;
  /** edit page: slug is immutable once the agent exists */
  slugReadOnly?: boolean;
}) {
  const onNameChange = (name: string) => {
    // Keep the slug in sync until the user edits it by hand.
    const wasAuto = draft.slug === "" || draft.slug === slugify(draft.name);
    patch({ name, ...(wasAuto ? { slug: slugify(name) } : {}) });
  };

  return (
    <div className="grid gap-4">
      <div className="grid gap-1.5">
        <Label htmlFor="agent-name">Name</Label>
        <Input
          id="agent-name"
          value={draft.name}
          onChange={(e) => onNameChange(e.target.value)}
          placeholder="e.g. Front-desk receptionist"
        />
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="agent-slug">Slug</Label>
        <Input
          id="agent-slug"
          value={draft.slug}
          onChange={(e) => patch({ slug: slugify(e.target.value) })}
          placeholder="front-desk-receptionist"
          readOnly={slugReadOnly}
          disabled={slugReadOnly}
          className="font-mono"
        />
        <p className="text-xs text-muted-foreground">
          {slugReadOnly
            ? "The slug is fixed once the agent exists."
            : "Auto-derived from the name — edit it if you need a different one."}
        </p>
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="agent-purpose">Purpose</Label>
        <Textarea
          id="agent-purpose"
          value={draft.purpose}
          onChange={(e) => patch({ purpose: e.target.value })}
          placeholder="What should this agent do for callers? e.g. Answer calls, book consultations and capture caller details."
        />
      </div>
    </div>
  );
}
