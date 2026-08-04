"use client";

/**
 * Journey editor (SPEC-W19 Agent D): structured step forms (type picker,
 * kind, template textarea, wait hours, branch condition) with a raw JSON
 * fallback for power users. Structural edits are accepted by the backend
 * only while the journey is draft|paused (409 otherwise — surfaced
 * honestly).
 */

import * as React from "react";
import { ArrowDown, Plus, Trash2 } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Input, Label, Select, Textarea } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import {
  STUDIO_API,
  type Journey,
  type JourneyStep,
  type Segment,
} from "./types";

const emptyStep = (): JourneyStep => ({ type: "wait", wait_hours: 1 });

/**
 * Branch condition editor: keeps its own text state so typing through
 * temporarily-invalid JSON doesn't fight the controlled input; only valid
 * parses propagate to the step.
 */
function ConditionEditor({
  condition,
  onChange,
}: {
  condition: JourneyStep["condition"];
  onChange: (c: NonNullable<JourneyStep["condition"]>) => void;
}) {
  const [text, setText] = React.useState(() =>
    JSON.stringify(condition ?? { filters: [{ field: "lead_status", op: "eq", value: "qualified" }] }, null, 2),
  );
  const [invalid, setInvalid] = React.useState(false);
  return (
    <div>
      <Label>Condition (JSON — same filter shape as segments)</Label>
      <Textarea
        rows={4}
        className="font-mono text-xs"
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          try {
            onChange(JSON.parse(e.target.value));
            setInvalid(false);
          } catch {
            setInvalid(true);
          }
        }}
      />
      <p className="mt-1 text-xs text-muted-foreground">
        {invalid ? (
          <span className="text-destructive">Invalid JSON — the last valid condition is kept.</span>
        ) : (
          "False → the enrollment exits with reason branch_condition_false."
        )}
      </p>
    </div>
  );
}

export function JourneyEditor({
  orgSlug,
  journey,
  segments,
  onSaved,
  onCancel,
}: {
  orgSlug: string;
  /** null → create new journey */
  journey: Journey | null;
  segments: Segment[];
  onSaved: () => void;
  onCancel: () => void;
}) {
  const { toast } = useToast();
  const [name, setName] = React.useState(journey?.name ?? "");
  const [triggerKind, setTriggerKind] = React.useState(journey?.trigger_kind ?? "manual");
  const [segmentId, setSegmentId] = React.useState(journey?.segment_id ?? "");
  const [steps, setSteps] = React.useState<JourneyStep[]>(
    journey?.steps?.length ? journey.steps : [emptyStep()],
  );
  const [rawMode, setRawMode] = React.useState(false);
  const [rawJson, setRawJson] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const [formError, setFormError] = React.useState<string | null>(null);

  const setStep = (idx: number, patch: Partial<JourneyStep>) =>
    setSteps((ss) => ss.map((s, i) => (i === idx ? { ...s, ...patch } : s)));

  const changeType = (idx: number, type: JourneyStep["type"]) => {
    if (type === "wait") setStep(idx, { type, kind: undefined, template: undefined, condition: undefined, wait_hours: 1 });
    else if (type === "send") setStep(idx, { type, kind: "sms", template: "", condition: undefined, wait_hours: undefined });
    else setStep(idx, { type, kind: undefined, template: undefined, wait_hours: undefined, condition: { filters: [{ field: "lead_status", op: "eq", value: "qualified" }] } });
  };

  const save = async () => {
    setFormError(null);
    if (!name.trim()) {
      setFormError("Name is required.");
      return;
    }
    let finalSteps = steps;
    if (rawMode) {
      try {
        const parsed = JSON.parse(rawJson) as unknown;
        if (!Array.isArray(parsed)) throw new Error("steps must be a JSON array");
        finalSteps = parsed as JourneyStep[];
      } catch (e) {
        setFormError(`Raw steps JSON is invalid: ${e instanceof Error ? e.message : String(e)}`);
        return;
      }
    }
    setSaving(true);
    try {
      const body: Record<string, unknown> = {
        name: name.trim(),
        trigger_kind: triggerKind,
        segment_id: triggerKind === "segment" && segmentId ? segmentId : null,
        steps: finalSteps,
      };
      if (journey) {
        await api.patch(`${STUDIO_API}/journeys/${journey.id}`, body, { tenant: orgSlug });
        toast({ title: "Journey updated" });
      } else {
        await api.post(`${STUDIO_API}/journeys`, body, { tenant: orgSlug });
        toast({ title: "Journey created (draft)" });
      }
      onSaved();
    } catch (e) {
      setFormError(e instanceof ApiError ? e.message : "Save failed.");
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="rounded-md border p-4">
      <h2 className="mb-3 text-sm font-medium">{journey ? "Edit journey" : "New journey"}</h2>
      <div className="space-y-3">
        <div className="grid gap-3 sm:grid-cols-2">
          <div>
            <Label htmlFor="j-name">Name</Label>
            <Input id="j-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. Winback 30d" />
          </div>
          <div>
            <Label htmlFor="j-trigger">Trigger</Label>
            <Select id="j-trigger" value={triggerKind} onChange={(e) => setTriggerKind(e.target.value as Journey["trigger_kind"])}>
              <option value="manual">manual (API enroll)</option>
              <option value="segment">segment (CRON follow-up)</option>
              <option value="event">event (CRON follow-up)</option>
            </Select>
          </div>
        </div>
        {triggerKind === "segment" ? (
          <div>
            <Label htmlFor="j-segment">Segment</Label>
            <Select id="j-segment" value={segmentId} onChange={(e) => setSegmentId(e.target.value)}>
              <option value="">— pick a segment —</option>
              {segments.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name}
                </option>
              ))}
            </Select>
          </div>
        ) : null}

        <div className="flex items-center justify-between">
          <Label>Steps ({steps.length})</Label>
          <Button
            size="sm"
            variant="ghost"
            onClick={() => {
              if (!rawMode) setRawJson(JSON.stringify(steps, null, 2));
              setRawMode(!rawMode);
            }}
          >
            {rawMode ? "Use form editor" : "Raw JSON"}
          </Button>
        </div>

        {rawMode ? (
          <Textarea
            aria-label="Raw steps JSON"
            rows={12}
            className="font-mono text-xs"
            value={rawJson}
            onChange={(e) => setRawJson(e.target.value)}
          />
        ) : (
          <div className="space-y-2">
            {steps.map((step, idx) => (
              <div key={idx} className="rounded-md border p-3">
                <div className="mb-2 flex items-center justify-between gap-2">
                  <span className="flex items-center gap-1 text-xs text-muted-foreground">
                    <ArrowDown className="h-3 w-3" /> Step {idx + 1}
                  </span>
                  <div className="flex items-center gap-2">
                    <Select
                      aria-label="Step type"
                      value={step.type}
                      onChange={(e) => changeType(idx, e.target.value as JourneyStep["type"])}
                      className="w-28"
                    >
                      <option value="wait">wait</option>
                      <option value="send">send</option>
                      <option value="branch">branch</option>
                    </Select>
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label="Remove step"
                      disabled={steps.length <= 1}
                      onClick={() => setSteps((ss) => ss.filter((_, i) => i !== idx))}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </Button>
                  </div>
                </div>

                {step.type === "wait" ? (
                  <div>
                    <Label>Wait hours</Label>
                    <Input
                      type="number"
                      min={0}
                      value={step.wait_hours ?? 0}
                      onChange={(e) => setStep(idx, { wait_hours: Math.max(0, Number(e.target.value) || 0) })}
                    />
                  </div>
                ) : null}

                {step.type === "send" ? (
                  <div className="space-y-2">
                    <div className="grid gap-2 sm:grid-cols-2">
                      <div>
                        <Label>Kind</Label>
                        <Select
                          value={step.kind ?? "sms"}
                          onChange={(e) => setStep(idx, { kind: e.target.value as JourneyStep["kind"] })}
                        >
                          <option value="sms">sms</option>
                          <option value="push_marketing">push_marketing</option>
                          <option value="ussd">ussd (advanced as skipped)</option>
                        </Select>
                      </div>
                      <div>
                        <Label>A/B variant (optional)</Label>
                        <Select
                          value={step.ab_variant ?? ""}
                          onChange={(e) => setStep(idx, { ab_variant: e.target.value || undefined })}
                        >
                          <option value="">—</option>
                          <option value="A">A</option>
                          <option value="B">B</option>
                        </Select>
                      </div>
                    </div>
                    <div>
                      <Label>Template ({"{name}"} token supported)</Label>
                      <Textarea
                        rows={3}
                        value={step.template ?? ""}
                        onChange={(e) => setStep(idx, { template: e.target.value })}
                        placeholder="Hi {name}, …"
                      />
                    </div>
                  </div>
                ) : null}

                {step.type === "branch" ? (
                  <ConditionEditor
                    condition={step.condition}
                    onChange={(c) => setStep(idx, { condition: c })}
                  />
                ) : null}
              </div>
            ))}
            <Button size="sm" variant="outline" onClick={() => setSteps((ss) => [...ss, emptyStep()])}>
              <Plus className="mr-1 h-3.5 w-3.5" /> Add step
            </Button>
          </div>
        )}

        {formError ? <p className="text-sm text-destructive">{formError}</p> : null}
        <div className="flex gap-2">
          <Button disabled={saving} onClick={() => void save()}>
            {saving ? "Saving…" : journey ? "Save changes" : "Create journey"}
          </Button>
          <Button variant="outline" onClick={onCancel}>
            Cancel
          </Button>
        </div>
      </div>
    </div>
  );
}
