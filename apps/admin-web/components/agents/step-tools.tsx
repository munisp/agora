"use client";

import { Badge } from "@/components/ui/badge";
import { AGENT_TOOLS, type AgentDraft } from "./agent-draft";

/**
 * Wizard step 4 / edit section — tool allowlist. Selecting nothing leaves
 * every built-in tool available (the runtime only filters when the allowlist
 * is non-empty, SPEC-W38 F2).
 */
export function StepTools({
  draft,
  patch,
}: {
  draft: AgentDraft;
  patch: (p: Partial<AgentDraft>) => void;
}) {
  const toggle = (tool: string) =>
    patch({
      toolAllowlist: draft.toolAllowlist.includes(tool)
        ? draft.toolAllowlist.filter((t) => t !== tool)
        : [...draft.toolAllowlist, tool],
    });

  return (
    <div className="grid gap-3">
      <p className="text-sm text-muted-foreground">
        Restrict which tools this agent may call.{" "}
        <span className="font-medium text-foreground">
          Select none to allow all tools.
        </span>
      </p>
      <div className="grid grid-cols-1 gap-1 sm:grid-cols-2">
        {AGENT_TOOLS.map((tool) => (
          <label key={tool} className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={draft.toolAllowlist.includes(tool)}
              onChange={() => toggle(tool)}
              className="h-4 w-4 accent-primary"
            />
            <span className="font-mono text-[13px]">{tool}</span>
          </label>
        ))}
      </div>
      <div className="flex flex-wrap items-center gap-1.5">
        <span className="text-xs text-muted-foreground">Effective:</span>
        {draft.toolAllowlist.length === 0 ? (
          <Badge variant="secondary">all tools</Badge>
        ) : (
          draft.toolAllowlist.map((t) => (
            <Badge key={t} variant="info" className="font-mono text-[11px]">
              {t}
            </Badge>
          ))
        )}
      </div>
    </div>
  );
}
