"use client";

/**
 * Survey editor (SPEC-W20 Agent B): name, kind + channel pickers and the
 * question rows (type picker, label, options editor for single/multi,
 * required toggle, remove). A raw-JSON fallback lets operators paste a
 * questions array directly — the server is the authoritative validator.
 * Used for both create and edit (edit pre-fills from the survey).
 */
import * as React from "react";
import { Plus, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input, Label, Select, Textarea } from "@/components/ui/input";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  QUESTION_TYPES,
  SURVEY_CHANNELS,
  SURVEY_KINDS,
  type Question,
  type QuestionType,
  type Survey,
  type SurveyChannel,
  type SurveyKind,
} from "./types";

export interface SurveyInput {
  name: string;
  kind: SurveyKind;
  channel: SurveyChannel;
  questions: Question[];
}

let rowSeq = 0;
function nextRowId(): string {
  rowSeq += 1;
  return `q${rowSeq}_${Date.now().toString(36)}`;
}

function emptyQuestion(): Question {
  return { id: "", type: "rating", label: "", required: false };
}

export function SurveyEditor({
  survey,
  busy,
  onSave,
  onCancel,
}: {
  /** present → edit mode (pre-filled); absent → create mode */
  survey?: Survey;
  busy: boolean;
  onSave: (input: SurveyInput) => Promise<boolean>;
  onCancel: () => void;
}) {
  const [name, setName] = React.useState(survey?.name ?? "");
  const [kind, setKind] = React.useState<SurveyKind>(survey?.kind ?? "nps");
  const [channel, setChannel] = React.useState<SurveyChannel>(
    survey?.channel ?? "sms",
  );
  const [questions, setQuestions] = React.useState<Question[]>(
    survey?.questions?.length ? survey.questions : [emptyQuestion()],
  );
  const [rawMode, setRawMode] = React.useState(false);
  const [rawJson, setRawJson] = React.useState("");
  const [rawError, setRawError] = React.useState<string | null>(null);
  const [formError, setFormError] = React.useState<string | null>(null);

  const patchQuestion = (idx: number, patch: Partial<Question>) => {
    setQuestions((qs) =>
      qs.map((q, i) => {
        if (i !== idx) return q;
        const next = { ...q, ...patch };
        // Type switches keep field discipline: rating/text take no options;
        // single/multi seed two options when none exist.
        if (patch.type) {
          if (patch.type === "single" || patch.type === "multi") {
            if (!next.options || next.options.length < 2) {
              next.options = ["Option 1", "Option 2"];
            }
          } else {
            delete next.options;
          }
        }
        return next;
      }),
    );
  };

  const applyRaw = () => {
    setRawError(null);
    try {
      const parsed = JSON.parse(rawJson) as Question[];
      if (!Array.isArray(parsed)) throw new Error("want a JSON array");
      setQuestions(
        parsed.map((q) => ({
          id: String(q.id ?? ""),
          type: q.type,
          label: String(q.label ?? ""),
          options: q.options,
          required: Boolean(q.required),
        })),
      );
      setRawMode(false);
    } catch (e) {
      setRawError(e instanceof Error ? e.message : "invalid JSON");
    }
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setFormError(null);
    if (!name.trim()) {
      setFormError("Name is required.");
      return;
    }
    const cleaned = questions
      .map((q, i) => ({
        ...q,
        id: q.id.trim() || `q${i + 1}`,
        label: q.label.trim(),
        options: q.options?.map((o) => o.trim()).filter(Boolean),
      }))
      .filter((q) => q.label !== "");
    if (cleaned.length === 0) {
      setFormError("Add at least one question with a label.");
      return;
    }
    const ok = await onSave({
      name: name.trim(),
      kind,
      channel,
      questions: cleaned,
    });
    if (ok && !survey) {
      setName("");
      setQuestions([emptyQuestion()]);
    }
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">
          {survey ? "Edit survey" : "New survey"}
        </CardTitle>
        <CardDescription>
          {survey
            ? "Changes apply to future invites; responses already collected keep their answers."
            : "Surveys are created as drafts — activate them to send invites."}
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form onSubmit={submit} className="space-y-4">
          <div className="grid gap-3 sm:grid-cols-3">
            <div className="space-y-1.5 sm:col-span-1">
              <Label htmlFor="sv-name">Name</Label>
              <Input
                id="sv-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="NPS — Q3"
                maxLength={200}
                required
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="sv-kind">Kind</Label>
              <Select
                id="sv-kind"
                value={kind}
                onChange={(e) => setKind(e.target.value as SurveyKind)}
              >
                {SURVEY_KINDS.map((k) => (
                  <option key={k} value={k}>
                    {k.toUpperCase()}
                  </option>
                ))}
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="sv-channel">Invite channel</Label>
              <Select
                id="sv-channel"
                value={channel}
                onChange={(e) => setChannel(e.target.value as SurveyChannel)}
              >
                {SURVEY_CHANNELS.map((c) => (
                  <option key={c} value={c}>
                    {c === "sms" ? "SMS" : "Push (marketing)"}
                  </option>
                ))}
              </Select>
            </div>
          </div>

          <div className="flex items-center justify-between">
            <Label>Questions</Label>
            <div className="flex items-center gap-2">
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => {
                  setRawJson(JSON.stringify(questions, null, 2));
                  setRawError(null);
                  setRawMode((m) => !m);
                }}
              >
                {rawMode ? "Back to rows" : "Raw JSON"}
              </Button>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => setQuestions((qs) => [...qs, emptyQuestion()])}
              >
                <Plus className="mr-1 h-3.5 w-3.5" /> Add question
              </Button>
            </div>
          </div>

          {rawMode ? (
            <div className="space-y-2">
              <Textarea
                value={rawJson}
                onChange={(e) => setRawJson(e.target.value)}
                rows={10}
                className="font-mono text-xs"
                placeholder='[{"id":"nps","type":"rating","label":"How likely?","required":true}]'
              />
              {rawError ? (
                <p className="text-sm text-destructive">{rawError}</p>
              ) : null}
              <Button type="button" variant="secondary" size="sm" onClick={applyRaw}>
                Apply JSON
              </Button>
            </div>
          ) : (
            <div className="space-y-3">
              {questions.map((q, idx) => (
                <div
                  key={`${idx}-${q.id || nextRowId()}`}
                  className="rounded-md border border-border p-3 space-y-2"
                >
                  <div className="grid gap-2 sm:grid-cols-[1fr_9rem_auto] sm:items-end">
                    <div className="space-y-1">
                      <Label className="text-xs text-muted-foreground">
                        Label
                      </Label>
                      <Input
                        value={q.label}
                        onChange={(e) =>
                          patchQuestion(idx, { label: e.target.value })
                        }
                        placeholder="How likely are you to recommend us?"
                        maxLength={300}
                      />
                    </div>
                    <div className="space-y-1">
                      <Label className="text-xs text-muted-foreground">
                        Type
                      </Label>
                      <Select
                        value={q.type}
                        onChange={(e) =>
                          patchQuestion(idx, {
                            type: e.target.value as QuestionType,
                          })
                        }
                      >
                        {QUESTION_TYPES.map((t) => (
                          <option key={t} value={t}>
                            {t}
                          </option>
                        ))}
                      </Select>
                    </div>
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      aria-label="Remove question"
                      onClick={() =>
                        setQuestions((qs) => qs.filter((_, i) => i !== idx))
                      }
                    >
                      <Trash2 className="h-4 w-4" />
                    </Button>
                  </div>
                  <div className="grid gap-2 sm:grid-cols-[1fr_10rem_auto] sm:items-center">
                    <Input
                      value={q.id}
                      onChange={(e) => patchQuestion(idx, { id: e.target.value })}
                      placeholder={`id (blank → q${idx + 1})`}
                      maxLength={64}
                      className="font-mono text-xs"
                    />
                    <label className="flex items-center gap-2 text-sm">
                      <input
                        type="checkbox"
                        checked={q.required}
                        onChange={(e) =>
                          patchQuestion(idx, { required: e.target.checked })
                        }
                        className="h-4 w-4 accent-primary"
                      />
                      Required
                    </label>
                    <span className="text-xs text-muted-foreground">
                      {q.type === "rating" ? "0–10" : null}
                    </span>
                  </div>
                  {q.type === "single" || q.type === "multi" ? (
                    <div className="space-y-1">
                      <Label className="text-xs text-muted-foreground">
                        Options (one per line, at least 2)
                      </Label>
                      <Textarea
                        rows={3}
                        value={(q.options ?? []).join("\n")}
                        onChange={(e) =>
                          patchQuestion(idx, {
                            options: e.target.value.split("\n"),
                          })
                        }
                      />
                    </div>
                  ) : null}
                </div>
              ))}
            </div>
          )}

          {formError ? (
            <p className="text-sm text-destructive">{formError}</p>
          ) : null}
          <div className="flex items-center gap-2">
            <Button type="submit" disabled={busy}>
              {busy ? "Saving…" : survey ? "Save changes" : "Create survey"}
            </Button>
            <Button type="button" variant="outline" onClick={onCancel}>
              Cancel
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  );
}
