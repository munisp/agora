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
 */
import * as React from "react";
import { Loader2, Save, Users } from "lucide-react";
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
  buildDSL,
  normalizeSegment,
  unwrapCount,
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
  const [saving, setSaving] = React.useState(false);
  const [preview, setPreview] = React.useState<PreviewState>({ status: "idle" });

  const dsl = React.useMemo(
    () => buildDSL({ hasConsent, lastBookingBefore, lga, notMessagedSinceDays }),
    [hasConsent, lastBookingBefore, lga, notMessagedSinceDays],
  );

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
          <Button onClick={() => void save()} disabled={saving}>
            <Save className="h-4 w-4" />
            {saving ? "Saving…" : "Save segment"}
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
