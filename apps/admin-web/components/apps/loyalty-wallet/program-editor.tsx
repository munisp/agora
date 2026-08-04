"use client";

/**
 * Loyalty program editor (SPEC-W19 Agent C): a friendly form over the
 * earn_rules / tiers jsonb (event select + points rows; tier name /
 * threshold / benefits rows) with a raw-JSON fallback for advanced edits.
 * Pure presentational — the parent client owns the POST/PATCH calls so the
 * program list can refresh after a save.
 */
import * as React from "react";
import { Plus, Save, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input, Label, Select, Textarea } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import {
  EARN_EVENTS,
  EVENT_LABELS,
  type EarnRule,
  type Program,
  type Tier,
} from "./types";

export interface ProgramDraft {
  program_id?: string; // present when editing an existing program
  name: string;
  active: boolean;
  earn_rules: EarnRule[];
  tiers: Tier[];
  cap_per_day: number;
}

export function draftFromProgram(p: Program): ProgramDraft {
  return {
    program_id: p.program_id,
    name: p.name,
    active: p.active,
    earn_rules: p.earn_rules ?? [],
    tiers: p.tiers ?? [],
    cap_per_day: p.cap_per_day ?? 0,
  };
}

export function emptyDraft(): ProgramDraft {
  return {
    name: "",
    active: true,
    earn_rules: [{ event: "booking_completed", points: 50 }],
    tiers: [],
    cap_per_day: 0,
  };
}

/** Client-side mirror of the backend validation so save failures are rare. */
export function validateDraft(d: ProgramDraft): string | null {
  if (d.name.trim() === "") return "Program name is required.";
  if (d.cap_per_day < 0) return "Daily cap must be ≥ 0 (0 = uncapped).";
  const seen = new Set<string>();
  for (const r of d.earn_rules) {
    if (!(r.points > 0)) return "Every earn rule needs points > 0.";
    if (seen.has(r.event))
      return `Duplicate earn rule for ${EVENT_LABELS[r.event] ?? r.event}.`;
    seen.add(r.event);
  }
  for (const t of d.tiers) {
    if (t.name.trim() === "") return "Every tier needs a name.";
    if (t.min_points < 0) return "Tier thresholds must be ≥ 0.";
  }
  return null;
}

export function ProgramEditor({
  initial,
  busy,
  onSave,
  onCancel,
}: {
  initial: ProgramDraft;
  busy: boolean;
  onSave: (draft: ProgramDraft) => Promise<boolean>;
  onCancel: () => void;
}) {
  const [draft, setDraft] = React.useState<ProgramDraft>(initial);
  const [rawMode, setRawMode] = React.useState(false);
  const [rawRules, setRawRules] = React.useState(() =>
    JSON.stringify(initial.earn_rules, null, 2),
  );
  const [rawTiers, setRawTiers] = React.useState(() =>
    JSON.stringify(initial.tiers, null, 2),
  );
  const [formError, setFormError] = React.useState<string | null>(null);

  const setRule = (i: number, patch: Partial<EarnRule>) =>
    setDraft((d) => ({
      ...d,
      earn_rules: d.earn_rules.map((r, j) => (j === i ? { ...r, ...patch } : r)),
    }));
  const setTier = (i: number, patch: Partial<Tier>) =>
    setDraft((d) => ({
      ...d,
      tiers: d.tiers.map((t, j) => (j === i ? { ...t, ...patch } : t)),
    }));

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy) return;
    let next = draft;
    if (rawMode) {
      // Raw JSON fallback: parse both blobs before validating.
      try {
        next = { ...draft, earn_rules: JSON.parse(rawRules) as EarnRule[] };
      } catch {
        setFormError("earn_rules is not valid JSON.");
        return;
      }
      try {
        next = { ...next, tiers: JSON.parse(rawTiers) as Tier[] };
      } catch {
        setFormError("tiers is not valid JSON.");
        return;
      }
    }
    const err = validateDraft(next);
    if (err) {
      setFormError(err);
      return;
    }
    setFormError(null);
    const ok = await onSave(next);
    if (ok && !next.program_id) setDraft(emptyDraft());
  };

  const usedEvents = new Set(draft.earn_rules.map((r) => r.event));

  return (
    <Card>
      <CardHeader>
        <CardTitle>
          {draft.program_id ? "Edit program" : "New program"}
        </CardTitle>
        <CardDescription>
          Earn rules award points per event; tiers are recomputed from
          lifetime earned on every accrual. Daily cap clamps over-cap
          accruals (0 = uncapped).
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form onSubmit={submit} className="space-y-4">
          <div className="grid gap-3 sm:grid-cols-3">
            <div className="space-y-1.5 sm:col-span-2">
              <Label htmlFor="prog-name">Name</Label>
              <Input
                id="prog-name"
                value={draft.name}
                onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                placeholder="e.g. Club Rewards"
                required
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="prog-cap">Daily earn cap (0 = none)</Label>
              <Input
                id="prog-cap"
                type="number"
                min={0}
                value={draft.cap_per_day}
                onChange={(e) =>
                  setDraft({ ...draft, cap_per_day: Number(e.target.value) || 0 })
                }
              />
            </div>
          </div>

          <label className="flex items-center gap-2 text-sm text-foreground">
            <input
              type="checkbox"
              checked={draft.active}
              onChange={(e) => setDraft({ ...draft, active: e.target.checked })}
              className="h-4 w-4 rounded border-border"
            />
            Active — accruals resolve earn rules from the most recently
            updated active program
          </label>

          <div className="flex items-center justify-between">
            <span className="text-sm font-medium text-foreground">
              Earn rules & tiers
            </span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => {
                if (rawMode) {
                  // Leaving raw mode: refresh the textareas from the form.
                  setRawRules(JSON.stringify(draft.earn_rules, null, 2));
                  setRawTiers(JSON.stringify(draft.tiers, null, 2));
                }
                setRawMode(!rawMode);
              }}
            >
              {rawMode ? "Use form editor" : "Edit raw JSON"}
            </Button>
          </div>

          {rawMode ? (
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-1.5">
                <Label htmlFor="raw-rules">earn_rules (JSON)</Label>
                <Textarea
                  id="raw-rules"
                  rows={8}
                  value={rawRules}
                  onChange={(e) => setRawRules(e.target.value)}
                  className="font-mono text-xs"
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="raw-tiers">tiers (JSON)</Label>
                <Textarea
                  id="raw-tiers"
                  rows={8}
                  value={rawTiers}
                  onChange={(e) => setRawTiers(e.target.value)}
                  className="font-mono text-xs"
                />
              </div>
            </div>
          ) : (
            <>
              <div className="space-y-2">
                {draft.earn_rules.map((r, i) => (
                  <div key={i} className="flex items-end gap-2">
                    <div className="flex-1 space-y-1.5">
                      <Label>Event</Label>
                      <Select
                        value={r.event}
                        onChange={(e) => setRule(i, { event: e.target.value })}
                      >
                        {EARN_EVENTS.map((ev) => (
                          <option
                            key={ev}
                            value={ev}
                            disabled={usedEvents.has(ev) && r.event !== ev}
                          >
                            {EVENT_LABELS[ev]}
                          </option>
                        ))}
                      </Select>
                    </div>
                    <div className="w-28 space-y-1.5">
                      <Label>Points</Label>
                      <Input
                        type="number"
                        min={1}
                        value={r.points}
                        onChange={(e) =>
                          setRule(i, { points: Number(e.target.value) || 0 })
                        }
                      />
                    </div>
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      aria-label="Remove rule"
                      onClick={() =>
                        setDraft((d) => ({
                          ...d,
                          earn_rules: d.earn_rules.filter((_, j) => j !== i),
                        }))
                      }
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </Button>
                  </div>
                ))}
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={draft.earn_rules.length >= EARN_EVENTS.length}
                  onClick={() => {
                    const free = EARN_EVENTS.find((ev) => !usedEvents.has(ev));
                    setDraft((d) => ({
                      ...d,
                      earn_rules: [
                        ...d.earn_rules,
                        { event: free ?? EARN_EVENTS[0], points: 10 },
                      ],
                    }));
                  }}
                >
                  <Plus className="h-3.5 w-3.5" /> Add earn rule
                </Button>
              </div>

              <div className="space-y-2">
                {draft.tiers.map((t, i) => (
                  <div key={i} className="flex items-end gap-2">
                    <div className="w-32 space-y-1.5">
                      <Label>Tier</Label>
                      <Input
                        value={t.name}
                        placeholder="silver"
                        onChange={(e) => setTier(i, { name: e.target.value })}
                      />
                    </div>
                    <div className="w-32 space-y-1.5">
                      <Label>Min lifetime points</Label>
                      <Input
                        type="number"
                        min={0}
                        value={t.min_points}
                        onChange={(e) =>
                          setTier(i, { min_points: Number(e.target.value) || 0 })
                        }
                      />
                    </div>
                    <div className="flex-1 space-y-1.5">
                      <Label>Benefits</Label>
                      <Input
                        value={t.benefits ?? ""}
                        placeholder="e.g. priority support"
                        onChange={(e) => setTier(i, { benefits: e.target.value })}
                      />
                    </div>
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      aria-label="Remove tier"
                      onClick={() =>
                        setDraft((d) => ({
                          ...d,
                          tiers: d.tiers.filter((_, j) => j !== i),
                        }))
                      }
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </Button>
                  </div>
                ))}
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  onClick={() =>
                    setDraft((d) => ({
                      ...d,
                      tiers: [
                        ...d.tiers,
                        {
                          name: "",
                          min_points:
                            d.tiers.length === 0
                              ? 0
                              : Math.max(...d.tiers.map((t) => t.min_points)) +
                                100,
                        },
                      ],
                    }))
                  }
                >
                  <Plus className="h-3.5 w-3.5" /> Add tier
                </Button>
              </div>
            </>
          )}

          {formError ? (
            <p className="text-sm text-destructive">{formError}</p>
          ) : null}
          <div className="flex gap-2">
            <Button type="submit" disabled={busy}>
              <Save className="h-3.5 w-3.5" />
              {busy ? "Saving…" : draft.program_id ? "Save changes" : "Create program"}
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

/** Read-only program summary card (the list view). */
export function ProgramCard({
  program,
  canManage,
  onEdit,
  onToggle,
}: {
  program: Program;
  canManage: boolean;
  onEdit: () => void;
  onToggle: (active: boolean) => void;
}) {
  return (
    <Card>
      <CardHeader className="flex flex-row items-start justify-between space-y-0">
        <div>
          <CardTitle className="flex items-center gap-2">
            {program.name}
            <Badge variant={program.active ? "success" : "secondary"}>
              {program.active ? "active" : "inactive"}
            </Badge>
          </CardTitle>
          <CardDescription>
            {program.cap_per_day > 0
              ? `Daily earn cap: ${program.cap_per_day} pts`
              : "No daily earn cap"}
          </CardDescription>
        </div>
        {canManage ? (
          <div className="flex gap-2">
            <Button variant="outline" size="sm" onClick={onEdit}>
              Edit
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => onToggle(!program.active)}
            >
              {program.active ? "Deactivate" : "Activate"}
            </Button>
          </div>
        ) : null}
      </CardHeader>
      <CardContent className="space-y-2 text-sm">
        <div className="flex flex-wrap gap-2">
          {(program.earn_rules ?? []).map((r) => (
            <Badge key={r.event} variant="info">
              {EVENT_LABELS[r.event] ?? r.event}: +{r.points}
            </Badge>
          ))}
          {(program.earn_rules ?? []).length === 0 ? (
            <span className="text-muted-foreground">No earn rules yet.</span>
          ) : null}
        </div>
        <div className="flex flex-wrap gap-2">
          {(program.tiers ?? []).map((t) => (
            <Badge key={t.name} variant="outline">
              {t.name} ≥ {t.min_points}
              {t.benefits ? ` — ${t.benefits}` : ""}
            </Badge>
          ))}
        </div>
      </CardContent>
    </Card>
  );
}
