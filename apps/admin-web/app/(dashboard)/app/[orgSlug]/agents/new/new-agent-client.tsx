"use client";

import * as React from "react";
import { useRouter } from "next/navigation";
import { ArrowLeft, ArrowRight, Check, Loader2 } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { useToast } from "@/components/ui/toast";
import { cn } from "@/lib/utils";
import {
  createBodyFromDraft,
  E164_RE,
  EMPTY_DRAFT,
  type AgentDraft,
} from "@/components/agents/agent-draft";
import { StepBasics } from "@/components/agents/step-basics";
import { StepPhone } from "@/components/agents/step-phone";
import { StepPersona } from "@/components/agents/step-persona";
import { StepTools } from "@/components/agents/step-tools";
import { StepKnowledge } from "@/components/agents/step-knowledge";
import { StepCapture } from "@/components/agents/step-capture";
import { StepReview } from "@/components/agents/step-review";
import type { Agent, CaptureSchema } from "@/lib/types";

/**
 * SPEC-W38 F4 — 7-step agent wizard: Basics → Phone number → Persona & voice
 * → Tools → Knowledge → Capture schema → Review. On finish it POSTs the agent
 * and, when capture fields are defined, the capture schema.
 */

const STEPS = [
  { title: "Basics", description: "Name, slug and what this agent is for." },
  {
    title: "Phone number",
    description: "The E.164 number that routes to this agent (optional).",
  },
  {
    title: "Persona & voice",
    description: "How the agent introduces itself and sounds.",
  },
  { title: "Tools", description: "Which built-in tools the agent may call." },
  {
    title: "Knowledge",
    description: "Knowledge packs the agent grounds its answers on.",
  },
  {
    title: "Capture schema",
    description: "Fields to extract from every call (optional).",
  },
  { title: "Review", description: "Check everything, then create the agent." },
] as const;

export function NewAgentClient({ orgSlug }: { orgSlug: string }) {
  const router = useRouter();
  const { toast } = useToast();
  const [step, setStep] = React.useState(0);
  const [draft, setDraft] = React.useState<AgentDraft>(EMPTY_DRAFT);
  const [error, setError] = React.useState<string | null>(null);
  const [creating, setCreating] = React.useState(false);

  const patch = React.useCallback(
    (p: Partial<AgentDraft>) => setDraft((d) => ({ ...d, ...p })),
    [],
  );

  const stepError = React.useMemo(() => {
    if (step === 0 && !draft.name.trim()) return "Give the agent a name.";
    if (
      step === 1 &&
      draft.phoneNumber.trim() !== "" &&
      !E164_RE.test(draft.phoneNumber.trim())
    )
      return "The phone number must be in E.164 format (or empty).";
    if (step === 5) {
      const seen = new Set<string>();
      for (const f of draft.captureFields) {
        if (!f.key || !f.label.trim())
          return "Every capture field needs a key and a label.";
        if (seen.has(f.key)) return `Duplicate capture field key "${f.key}".`;
        seen.add(f.key);
        if (f.type === "enum" && (f.options ?? []).length === 0)
          return `Enum field "${f.key}" needs at least one option.`;
      }
    }
    return null;
  }, [step, draft]);

  const next = () => {
    if (stepError) return;
    setError(null);
    setStep((s) => Math.min(s + 1, STEPS.length - 1));
  };

  const create = async () => {
    setCreating(true);
    setError(null);
    try {
      const agent = await api.post<Agent>(
        "/api/agents",
        createBodyFromDraft(draft),
        { tenant: orgSlug },
      );
      if (draft.captureFields.length > 0) {
        await api.post<CaptureSchema>(
          "/api/capture-schemas",
          {
            agent_id: agent.id,
            name: draft.captureSchemaName.trim() || "Call capture",
            schema: { fields: draft.captureFields },
          },
          { tenant: orgSlug },
        );
      }
      toast({
        title: "Agent created",
        description: `${agent.name} is live${agent.phone_number ? ` on ${agent.phone_number}` : ""}.`,
        variant: "success",
      });
      router.push(`/app/${orgSlug}/agents/${agent.id}`);
    } catch (e) {
      setError(
        e instanceof ApiError
          ? e.message
          : "Failed to create the agent — the conversation service may be offline.",
      );
    } finally {
      setCreating(false);
    }
  };

  const isLast = step === STEPS.length - 1;

  return (
    <div className="mx-auto max-w-2xl space-y-6 p-6">
      <PageHeader
        title="New agent"
        description="A guided setup — you can change everything later from the agent page."
      />

      {/* Step indicator */}
      <ol className="flex flex-wrap items-center gap-1.5">
        {STEPS.map((s, i) => (
          <li key={s.title} className="flex items-center gap-1.5">
            <button
              type="button"
              onClick={() => i < step && setStep(i)}
              disabled={i >= step}
              className={cn(
                "flex h-6 w-6 items-center justify-center rounded-full text-xs font-semibold",
                i < step
                  ? "bg-primary text-primary-foreground"
                  : i === step
                    ? "border-2 border-primary text-primary"
                    : "border border-border text-muted-foreground",
              )}
              aria-label={`Step ${i + 1}: ${s.title}`}
            >
              {i < step ? <Check className="h-3 w-3" /> : i + 1}
            </button>
            <span
              className={cn(
                "text-xs",
                i === step ? "font-medium text-foreground" : "text-muted-foreground",
              )}
            >
              {s.title}
            </span>
            {i < STEPS.length - 1 ? (
              <span className="mx-1 text-muted-foreground">›</span>
            ) : null}
          </li>
        ))}
      </ol>

      {error ? <ErrorNote message={error} /> : null}

      <Card>
        <CardHeader>
          <CardTitle>
            Step {step + 1} of {STEPS.length} — {STEPS[step].title}
          </CardTitle>
          <CardDescription>{STEPS[step].description}</CardDescription>
        </CardHeader>
        <CardContent>
          {step === 0 ? <StepBasics draft={draft} patch={patch} /> : null}
          {step === 1 ? <StepPhone draft={draft} patch={patch} /> : null}
          {step === 2 ? <StepPersona draft={draft} patch={patch} /> : null}
          {step === 3 ? <StepTools draft={draft} patch={patch} /> : null}
          {step === 4 ? <StepKnowledge draft={draft} patch={patch} /> : null}
          {step === 5 ? <StepCapture draft={draft} patch={patch} /> : null}
          {step === 6 ? <StepReview draft={draft} /> : null}

          {stepError ? (
            <p className="mt-4 text-sm text-destructive">{stepError}</p>
          ) : null}

          <div className="mt-6 flex items-center justify-between">
            <Button
              variant="outline"
              onClick={() => setStep((s) => Math.max(s - 1, 0))}
              disabled={step === 0 || creating}
            >
              <ArrowLeft className="h-4 w-4" /> Back
            </Button>
            {isLast ? (
              <Button onClick={() => void create()} disabled={creating || !!stepError}>
                {creating ? (
                  <Loader2 className="h-4 w-4 animate-spin" />
                ) : (
                  <Check className="h-4 w-4" />
                )}
                {creating ? "Creating…" : "Create agent"}
              </Button>
            ) : (
              <Button onClick={next} disabled={!!stepError}>
                Next <ArrowRight className="h-4 w-4" />
              </Button>
            )}
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
