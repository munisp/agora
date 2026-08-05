"use client";

/**
 * SPEC-W28 WS-C: Segment Builder form.
 *
 * Form state → declarative segment DSL (see types.ts) → debounced live
 * consent-passing count preview → save via POST /v1/graph/segments. The DSL
 * is shown read-only so the operator can see exactly what is compiled to
 * Cypher (with the mandatory consent filter) server-side.
 *
 * The consent gate is by construction (SPEC-W28 §0): the preview count and
 * every later audience only contain Persons with a purpose-matching
 * CONSENTED edge. include_quarantined is fixed false (§5 gate 4) — the
 * disabled field + note make that rule visible instead of hidden.
 *
 * SPEC-W29 WS-C: optional numeric score-filter rows (propensity_churn /
 * propensity_convert / propensity_turnout / risk_score with >=, <= or
 * between) serialize into the DSL `score_filters` block. Validation mirrors
 * the compiler's 422 rules (validateScoreFilterRows in types.ts) so an
 * invalid row is flagged inline and can neither enter the preview DSL nor
 * be saved.
 */
import * as React from "react";
import { Loader2, Plus, Save, Trash2, Users } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input, Label, Select } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import {
  GRAPH_API,
  SCORE_FILTER_FIELDS,
  buildDSL,
  emptyScoreFilterRow,
  normalizeSegment,
  unwrapCount,
  validateScoreFilterRows,
  type ScoreFilterOp,
  type ScoreFilterRow,
  type Segment,
} from "./types";

/** Consent purposes the builder offers (SPEC-W12 purpose taxonomy is
 * free-text server-side; "marketing" is the outreach default of §4 WS-B). */
const CONSENT_PURPOSES = [
  { value: "marketing", label: "Marketing" },
  { value: "promotions", label: "Promotions" },
  { value: "service", label: "Service updates" },
  { value: "reminders", label: "Reminders" },
  { value: "kyc", label: "KYC" },
];

const PREVIEW_DEBOUNCE_MS = 500;

/** Local row ids for score-filter rows (React keys only). */
let scoreRowSeq = 0;
function nextRowId(): string {
  scoreRowSeq += 1;
  return `sf-${scoreRowSeq}`;
}

/**
 * SPEC-W29 WS-C: prefill handoff from the Person 360 recommendations panel.
 * The panel navigates here with ?similar_to=<personId>&similar_name=&
 * similar_count= after running the similar_persons template; the builder
 * seeds the segment name and shows a provenance note. Read from
 * window.location (not useSearchParams) so the page needs no Suspense
 * boundary — the values are a one-time seed, not reactive state.
 */
interface SimilarSeed {
  personId: string;
  personName?: string;
  count?: number;
}

function readSimilarSeed(): SimilarSeed | null {
  if (typeof window === "undefined") return null;
  const params = new URLSearchParams(window.location.search);
  const personId = params.get("similar_to");
  if (!personId) return null;
  const countRaw = params.get("similar_count");
  const count = countRaw === null ? NaN : Number(countRaw);
  return {
    personId,
    personName: params.get("similar_name") ?? undefined,
    count: Number.isFinite(count) && count >= 0 ? count : undefined,
  };
}

type PreviewState =
  | { status: "idle" }
  | { status: "loading" }
  | { status: "ok"; count: number }
  | { status: "unavailable"; message: string };

export function SegmentBuilder({
  orgSlug,
  onSaved,
}: {
  orgSlug: string;
  onSaved: (segment: Segment) => void;
}) {
  const { toast } = useToast();
  const [name, setName] = React.useState("");
  const [hasConsent, setHasConsent] = React.useState("marketing");
  const [lastBookingBefore, setLastBookingBefore] = React.useState("");
  const [lga, setLga] = React.useState("");
  const [notMessagedSinceDays, setNotMessagedSinceDays] = React.useState("");
  // SPEC-W29 WS-C: numeric score-filter rows (field / op / value), validated
  // client-side with the same rules the compiler enforces with a 422.
  const [scoreRows, setScoreRows] = React.useState<ScoreFilterRow[]>([]);
  const [seed, setSeed] = React.useState<SimilarSeed | null>(null);
  const [saving, setSaving] = React.useState(false);
  const [preview, setPreview] = React.useState<PreviewState>({ status: "idle" });

  // One-time prefill from the Person 360 "Create audience of similar
  // clients" handoff (see readSimilarSeed above).
  React.useEffect(() => {
    const s = readSimilarSeed();
    if (!s) return;
    setSeed(s);
    setName((prev) =>
      prev || `Similar to ${s.personName?.trim() || `client ${s.personId}`}`,
    );
  }, []);

  const scoreValidation = React.useMemo(
    () => validateScoreFilterRows(scoreRows),
    [scoreRows],
  );
  const scoreErrors = scoreValidation.errors;

  const dsl = React.useMemo(
    () =>
      buildDSL({
        hasConsent,
        lastBookingBefore,
        lga,
        notMessagedSinceDays,
        // Invalid rows never enter the DSL — the operator sees the error
        // next to the row, and the preview runs on the valid remainder.
        scoreFilters: scoreValidation.filters,
      }),
    [hasConsent, lastBookingBefore, lga, notMessagedSinceDays, scoreValidation],
  );

  function updateScoreRow(id: string, patch: Partial<ScoreFilterRow>) {
    setScoreRows((rows) =>
      rows.map((r) => (r.id === id ? { ...r, ...patch } : r)),
    );
  }

  // Debounced live preview of the consent-passing count. Primary: the
  // unsaved-DSL count endpoint; fallback: the save endpoint in dry-run mode
  // (graph-service implements at least one — see types.ts header).
  React.useEffect(() => {
    const controller = new AbortController();
    setPreview({ status: "loading" });
    const timer = setTimeout(() => {
      (async () => {
        try {
          const data = await api.post<unknown>(
            `${GRAPH_API}/segments/count`,
            dsl,
            { tenant: orgSlug },
          );
          const count = unwrapCount(data);
          if (controller.signal.aborted) return;
          setPreview(
            count === null
              ? { status: "unavailable", message: "Preview returned no count." }
              : { status: "ok", count },
          );
        } catch (e) {
          if (controller.signal.aborted) return;
          if (e instanceof ApiError && e.status === 404) {
            try {
              const data = await api.post<unknown>(
                `${GRAPH_API}/segments`,
                dsl,
                { tenant: orgSlug, dry_run: "true" },
              );
              const count = unwrapCount(data);
              if (controller.signal.aborted) return;
              setPreview(
                count === null
                  ? { status: "unavailable", message: "Preview returned no count." }
                  : { status: "ok", count },
              );
              return;
            } catch (e2) {
              if (controller.signal.aborted) return;
              setPreview({
                status: "unavailable",
                message:
                  e2 instanceof ApiError && e2.status === 404
                    ? "Live preview is not available yet — save the segment to see its count."
                    : "Preview unavailable — the graph service may be offline.",
              });
              return;
            }
          }
          setPreview({
            status: "unavailable",
            message: "Preview unavailable — the graph service may be offline.",
          });
        }
      })();
    }, PREVIEW_DEBOUNCE_MS);
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [dsl, orgSlug]);

  async function save() {
    // Belt-and-braces: the button is already disabled while any score row
    // is invalid (client mirror of the server 422 rules).
    if (Object.keys(scoreErrors).length > 0) return;
    setSaving(true);
    try {
      const payload = { name: name.trim() || "Untitled segment", ...dsl };
      const data = await api.post<unknown>(`${GRAPH_API}/segments`, payload, {
        tenant: orgSlug,
      });
      const segment = normalizeSegment(
        (typeof data === "object" && data !== null ? data : payload) as Record<
          string,
          unknown
        >,
      );
      if (!segment.id) {
        // graph-service answered 2xx without echoing the row; keep the local
        // copy addressable for the list until the next reload.
        segment.id = `local-${Date.now()}`;
      }
      toast({ title: "Segment saved", variant: "success" });
      onSaved(segment);
      setName("");
    } catch (e) {
      toast({
        title: "Could not save segment",
        description:
          e instanceof ApiError
            ? e.message
            : "The graph service may be offline.",
        variant: "destructive",
      });
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Segment builder</CardTitle>
        <CardDescription>
          Define an audience over your customer graph. Only people who consented
          to the selected purpose are ever included — the count below is the
          consent-passing audience size.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {seed ? (
          <div className="rounded-md border border-border bg-muted px-3 py-2 text-sm">
            Prefilled from Person 360 —{" "}
            {seed.count !== undefined
              ? `${seed.count} ${seed.count === 1 ? "client" : "clients"} similar to `
              : "clients similar to "}
            <span className="font-medium">
              {seed.personName?.trim() || seed.personId}
            </span>
            . Consent, DND and quarantine gates still apply to the audience.
          </div>
        ) : null}
        <div className="grid gap-4 sm:grid-cols-2">
          <div className="space-y-1.5">
            <Label htmlFor="seg-name">Segment name</Label>
            <Input
              id="seg-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Lapsed Ikeja customers"
              maxLength={120}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="seg-consent">Required consent purpose</Label>
            <Select
              id="seg-consent"
              value={hasConsent}
              onChange={(e) => setHasConsent(e.target.value)}
            >
              {CONSENT_PURPOSES.map((p) => (
                <option key={p.value} value={p.value}>
                  {p.label}
                </option>
              ))}
            </Select>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="seg-last-booking">Last booking before</Label>
            <Input
              id="seg-last-booking"
              type="date"
              value={lastBookingBefore}
              onChange={(e) => setLastBookingBefore(e.target.value)}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="seg-lga">LGA</Label>
            <Input
              id="seg-lga"
              value={lga}
              onChange={(e) => setLga(e.target.value)}
              placeholder="Ikeja"
              maxLength={80}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="seg-not-messaged">Not messaged in the last (days)</Label>
            <Input
              id="seg-not-messaged"
              type="number"
              min={0}
              step={1}
              inputMode="numeric"
              value={notMessagedSinceDays}
              onChange={(e) => setNotMessagedSinceDays(e.target.value)}
              placeholder="30"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="seg-quarantined">Include quarantined people</Label>
            <div className="flex h-9 items-center gap-2">
              <input
                id="seg-quarantined"
                type="checkbox"
                checked={false}
                disabled
                aria-describedby="seg-quarantined-note"
                className="h-4 w-4 cursor-not-allowed accent-primary opacity-60"
              />
              <span className="text-sm text-muted-foreground">No — always excluded</span>
            </div>
            <p id="seg-quarantined-note" className="text-xs text-muted-foreground">
              Imported people whose consent is not yet verified stay visible in the
              graph but can never enter an outreach audience (NDPA compliance
              rule, fixed for every segment).
            </p>
          </div>
        </div>

        {/* SPEC-W29 WS-C: numeric score filters over the predictive layer.
            Scores filter *within* the consent-eligible population — they can
            never widen it (SPEC-W29 §0.3). */}
        <div className="space-y-2 rounded-md border border-border px-3 py-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div>
              <p className="text-sm font-medium">Score filters (optional)</p>
              <p className="text-xs text-muted-foreground">
                Filter by this workspace&apos;s predictive scores (0–1):
                churn, convert, turnout or fraud risk.
              </p>
            </div>
            <Button
              variant="outline"
              size="sm"
              onClick={() =>
                setScoreRows((rows) => [...rows, emptyScoreFilterRow(nextRowId())])
              }
            >
              <Plus className="h-3.5 w-3.5" />
              Add score filter
            </Button>
          </div>
          {scoreRows.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              No score filters — the segment uses the consent and attribute
              filters above only.
            </p>
          ) : (
            <ul className="space-y-2">
              {scoreRows.map((row) => {
                const rowError = scoreErrors[row.id];
                const describedBy = `sf-note-${row.id}`;
                return (
                  <li key={row.id} className="space-y-1">
                    <div className="flex flex-wrap items-end gap-2">
                      <div className="space-y-1">
                        <Label htmlFor={`sf-field-${row.id}`} className="text-xs">
                          Score
                        </Label>
                        <Select
                          id={`sf-field-${row.id}`}
                          className="w-44"
                          value={row.field}
                          onChange={(e) =>
                            updateScoreRow(row.id, {
                              field: e.target.value as ScoreFilterRow["field"],
                            })
                          }
                        >
                          {SCORE_FILTER_FIELDS.map((f) => (
                            <option key={f.value} value={f.value}>
                              {f.label}
                            </option>
                          ))}
                        </Select>
                      </div>
                      <div className="space-y-1">
                        <Label htmlFor={`sf-op-${row.id}`} className="text-xs">
                          Operator
                        </Label>
                        <Select
                          id={`sf-op-${row.id}`}
                          className="w-28"
                          value={row.op}
                          onChange={(e) =>
                            updateScoreRow(row.id, {
                              op: e.target.value as ScoreFilterOp,
                            })
                          }
                        >
                          <option value=">=">at least</option>
                          <option value="<=">at most</option>
                          <option value="between">between</option>
                        </Select>
                      </div>
                      {row.op === "between" ? (
                        <>
                          <div className="space-y-1">
                            <Label htmlFor={`sf-lo-${row.id}`} className="text-xs">
                              From
                            </Label>
                            <Input
                              id={`sf-lo-${row.id}`}
                              type="number"
                              min={0}
                              max={1}
                              step={0.05}
                              inputMode="decimal"
                              className="w-24"
                              value={row.valueLo}
                              aria-invalid={rowError ? true : undefined}
                              aria-describedby={rowError ? describedBy : undefined}
                              onChange={(e) =>
                                updateScoreRow(row.id, { valueLo: e.target.value })
                              }
                              placeholder="0.6"
                            />
                          </div>
                          <div className="space-y-1">
                            <Label htmlFor={`sf-hi-${row.id}`} className="text-xs">
                              To
                            </Label>
                            <Input
                              id={`sf-hi-${row.id}`}
                              type="number"
                              min={0}
                              max={1}
                              step={0.05}
                              inputMode="decimal"
                              className="w-24"
                              value={row.valueHi}
                              aria-invalid={rowError ? true : undefined}
                              aria-describedby={rowError ? describedBy : undefined}
                              onChange={(e) =>
                                updateScoreRow(row.id, { valueHi: e.target.value })
                              }
                              placeholder="0.9"
                            />
                          </div>
                        </>
                      ) : (
                        <div className="space-y-1">
                          <Label htmlFor={`sf-val-${row.id}`} className="text-xs">
                            Score (0–1)
                          </Label>
                          <Input
                            id={`sf-val-${row.id}`}
                            type="number"
                            min={0}
                            max={1}
                            step={0.05}
                            inputMode="decimal"
                            className="w-24"
                            value={row.value}
                            aria-invalid={rowError ? true : undefined}
                            aria-describedby={rowError ? describedBy : undefined}
                            onChange={(e) =>
                              updateScoreRow(row.id, { value: e.target.value })
                            }
                            placeholder="0.7"
                          />
                        </div>
                      )}
                      <Button
                        variant="ghost"
                        size="sm"
                        aria-label="Remove score filter"
                        onClick={() =>
                          setScoreRows((rows) => rows.filter((r) => r.id !== row.id))
                        }
                      >
                        <Trash2 className="h-3.5 w-3.5" />
                      </Button>
                    </div>
                    {rowError ? (
                      <p id={describedBy} className="text-xs text-destructive">
                        {rowError}
                      </p>
                    ) : null}
                  </li>
                );
              })}
            </ul>
          )}
        </div>

        <div
          className={cn(
            "flex items-center gap-3 rounded-md border border-border bg-muted px-3 py-2",
          )}
          aria-live="polite"
        >
          <Users className="h-4 w-4 shrink-0 text-muted-foreground" />
          {preview.status === "loading" ? (
            <span className="flex items-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="h-3.5 w-3.5 animate-spin" />
              Counting consent-passing people…
            </span>
          ) : preview.status === "ok" ? (
            <span className="text-sm">
              <span className="text-lg font-semibold tabular-nums">
                {preview.count.toLocaleString()}
              </span>{" "}
              <span className="text-muted-foreground">
                consent-passing {preview.count === 1 ? "person" : "people"} in this segment
              </span>
            </span>
          ) : (
            <span className="text-sm text-muted-foreground">
              {preview.status === "unavailable" ? preview.message : "Adjust the filters to preview the audience."}
            </span>
          )}
        </div>

        <details className="rounded-md border border-border bg-card px-3 py-2 text-xs">
          <summary className="cursor-pointer font-medium text-muted-foreground">
            Segment definition (DSL sent to the graph service)
          </summary>
          <pre className="mt-2 overflow-x-auto rounded bg-muted p-2 font-mono text-[11px] leading-relaxed">
            {JSON.stringify(dsl, null, 2)}
          </pre>
        </details>

        <div className="flex justify-end">
          <Button
            onClick={() => void save()}
            disabled={saving || Object.keys(scoreErrors).length > 0}
            title={
              Object.keys(scoreErrors).length > 0
                ? "Fix the highlighted score filter first."
                : undefined
            }
          >
            <Save className="h-4 w-4" />
            {saving ? "Saving…" : "Save segment"}
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
