"use client";

import { Input, Label } from "@/components/ui/input";
import { SchemaFieldEditor } from "./schema-field-editor";
import type { AgentDraft } from "./agent-draft";

/** Wizard step 6 / edit section — post-call capture schema (SPEC-W38 F3). */
export function StepCapture({
  draft,
  patch,
}: {
  draft: AgentDraft;
  patch: (p: Partial<AgentDraft>) => void;
}) {
  return (
    <div className="grid gap-4">
      <p className="text-sm text-muted-foreground">
        After each call, the capture pipeline extracts these fields from the
        transcript and stores them as a capture record. Leave empty to skip
        capture for this agent.
      </p>
      <div className="grid gap-1.5">
        <Label htmlFor="capture-name">Schema name</Label>
        <Input
          id="capture-name"
          value={draft.captureSchemaName}
          onChange={(e) => patch({ captureSchemaName: e.target.value })}
          placeholder="Call capture"
        />
      </div>
      <SchemaFieldEditor
        fields={draft.captureFields}
        onChange={(captureFields) => patch({ captureFields })}
      />
    </div>
  );
}
