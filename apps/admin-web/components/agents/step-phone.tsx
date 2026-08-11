"use client";

import { Input, Label } from "@/components/ui/input";
import { E164_RE, type AgentDraft } from "./agent-draft";

/** Wizard step 2 / edit section — optional E.164 inbound number. */
export function StepPhone({
  draft,
  patch,
}: {
  draft: AgentDraft;
  patch: (p: Partial<AgentDraft>) => void;
}) {
  const value = draft.phoneNumber.trim();
  const invalid = value !== "" && !E164_RE.test(value);

  return (
    <div className="grid gap-4">
      <div className="grid gap-1.5">
        <Label htmlFor="agent-phone">Phone number (optional)</Label>
        <Input
          id="agent-phone"
          value={draft.phoneNumber}
          onChange={(e) => patch({ phoneNumber: e.target.value })}
          placeholder="+2348012345678"
          className="font-mono"
          aria-invalid={invalid}
        />
        {invalid ? (
          <p className="text-xs text-destructive">
            Use E.164 format — a leading + followed by 7–15 digits, e.g.
            +2348012345678.
          </p>
        ) : (
          <p className="text-xs text-muted-foreground">
            Calls to this number route to this agent (SPEC-W38 F1). Leave empty
            to assign a number later — it must be in E.164 format and unique
            per organisation.
          </p>
        )}
      </div>
    </div>
  );
}
