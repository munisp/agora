"use client";

import * as React from "react";
import { X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Input, Label } from "@/components/ui/input";
import type { AgentDraft } from "./agent-draft";

/**
 * Wizard step 5 / edit section — knowledge packs as a tag input
 * (definition.knowledge_packs, SPEC-W38 F2). Enter or comma adds a tag.
 */
export function StepKnowledge({
  draft,
  patch,
}: {
  draft: AgentDraft;
  patch: (p: Partial<AgentDraft>) => void;
}) {
  const [entry, setEntry] = React.useState("");

  const add = (raw: string) => {
    const tag = raw.trim();
    if (!tag) return;
    if (!draft.knowledgePacks.includes(tag)) {
      patch({ knowledgePacks: [...draft.knowledgePacks, tag] });
    }
    setEntry("");
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" || e.key === ",") {
      e.preventDefault();
      add(entry);
    } else if (e.key === "Backspace" && entry === "" && draft.knowledgePacks.length > 0) {
      patch({ knowledgePacks: draft.knowledgePacks.slice(0, -1) });
    }
  };

  return (
    <div className="grid gap-3">
      <div className="grid gap-1.5">
        <Label htmlFor="agent-packs">Knowledge packs</Label>
        <Input
          id="agent-packs"
          value={entry}
          onChange={(e) => setEntry(e.target.value)}
          onKeyDown={onKeyDown}
          onBlur={() => add(entry)}
          placeholder="e.g. clinic-faq — press Enter to add"
        />
        <p className="text-xs text-muted-foreground">
          Pack slugs or search queries the agent grounds its answers on. Press
          Enter or comma to add each one.
        </p>
      </div>
      {draft.knowledgePacks.length > 0 ? (
        <div className="flex flex-wrap gap-1.5">
          {draft.knowledgePacks.map((pack) => (
            <Badge key={pack} variant="secondary" className="gap-1">
              {pack}
              <button
                type="button"
                aria-label={`Remove ${pack}`}
                className="rounded-full hover:text-destructive"
                onClick={() =>
                  patch({
                    knowledgePacks: draft.knowledgePacks.filter(
                      (p) => p !== pack,
                    ),
                  })
                }
              >
                <X className="h-3 w-3" />
              </button>
            </Badge>
          ))}
        </div>
      ) : (
        <p className="text-xs text-muted-foreground">
          No packs — the agent answers from the tenant knowledge base only.
        </p>
      )}
    </div>
  );
}
