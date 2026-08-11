"use client";

import { Badge } from "@/components/ui/badge";
import type { AgentDraft } from "./agent-draft";

function Row({ term, children }: { term: string; children: React.ReactNode }) {
  return (
    <div>
      <dt className="text-muted-foreground">{term}</dt>
      <dd className="font-medium">{children}</dd>
    </div>
  );
}

/** Wizard step 7 — read-only summary before creation. */
export function StepReview({ draft }: { draft: AgentDraft }) {
  return (
    <dl className="grid grid-cols-1 gap-x-6 gap-y-3 text-sm sm:grid-cols-2">
      <Row term="Name">{draft.name.trim() || "—"}</Row>
      <Row term="Slug">
        <span className="font-mono">{draft.slug || "—"}</span>
      </Row>
      <Row term="Phone number">
        <span className="font-mono">{draft.phoneNumber.trim() || "—"}</span>
      </Row>
      <Row term="Voice">
        <span className="font-mono">
          {draft.voiceId} · {draft.language}
        </span>
      </Row>
      <div className="sm:col-span-2">
        <Row term="Purpose">{draft.purpose.trim() || "—"}</Row>
      </div>
      <div className="sm:col-span-2">
        <Row term="Persona">
          {draft.persona.trim() ? (
            <span className="whitespace-pre-wrap">{draft.persona.trim()}</span>
          ) : (
            "—"
          )}
        </Row>
      </div>
      <Row term="Tools">
        {draft.toolAllowlist.length === 0 ? (
          <Badge variant="secondary">all tools</Badge>
        ) : (
          <span className="flex flex-wrap gap-1">
            {draft.toolAllowlist.map((t) => (
              <Badge key={t} variant="info" className="font-mono text-[11px]">
                {t}
              </Badge>
            ))}
          </span>
        )}
      </Row>
      <Row term="Knowledge packs">
        {draft.knowledgePacks.length === 0
          ? "—"
          : draft.knowledgePacks.join(", ")}
      </Row>
      <div className="sm:col-span-2">
        <Row term={`Capture schema (${draft.captureFields.length} field${draft.captureFields.length === 1 ? "" : "s"})`}>
          {draft.captureFields.length === 0 ? (
            "No capture schema — the agent will not extract call data."
          ) : (
            <span className="flex flex-wrap gap-1">
              {draft.captureFields.map((f) => (
                <Badge
                  key={f.key}
                  variant={f.required ? "default" : "outline"}
                  className="font-mono text-[11px]"
                >
                  {f.key}: {f.type}
                </Badge>
              ))}
            </span>
          )}
        </Row>
      </div>
    </dl>
  );
}
