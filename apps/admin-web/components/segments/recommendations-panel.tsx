"use client";

/**
 * SPEC-W29 WS-C: "Recommended next services" panel for Person 360.
 *
 * Reads the tenant-scoped RECOMMENDED_FOR edges through the existing
 * template-allowlisted graph client pattern (raw Cypher is never accepted
 * by the gateway — SPEC-W28 §5 gate 5):
 *
 *   POST /api/graph/v1/graph/cypher
 *     {template: "next_best_services", params: {person_id}}
 *     {template: "similar_persons",    params: {person_id, k}}
 *
 * The panel degrades explicitly: while the W29 scorer has not run (or the
 * template is not deployed yet) the operator sees an empty state, not a
 * crash; a gateway failure shows an error state with retry.
 *
 * "Create audience of similar clients" runs the similar_persons template
 * (cosine over the stored W28 embeddings, tenant-scoped, self excluded) and
 * hands off to the segment builder via URL params, which the builder reads
 * as a prefill (name + seed note).
 */
import * as React from "react";
import { useRouter } from "next/navigation";
import { Loader2, RefreshCw, Sparkles, Users } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { useToast } from "@/components/ui/toast";
import { formatDateTime } from "@/lib/utils";
import { scoreTone, TONE_COLORS } from "./propensity-badge";
import {
  BRAND,
  GRAPH_API,
  humanizeReason,
  normalizeRecommendation,
  unwrapList,
  type ServiceRecommendation,
} from "./types";

/** Top-K of the similar-persons lookup used for the audience handoff. */
const SIMILAR_K = 25;

type RecsState =
  | { status: "loading" }
  | { status: "ok"; recs: ServiceRecommendation[] }
  | { status: "empty"; message: string }
  | { status: "error"; message: string };

export function RecommendationsPanel({
  orgSlug,
  personId,
  personName,
}: {
  orgSlug: string;
  personId: string;
  personName?: string;
}) {
  const router = useRouter();
  const { toast } = useToast();
  const [state, setState] = React.useState<RecsState>({ status: "loading" });
  const [seeding, setSeeding] = React.useState(false);

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setState({ status: "loading" });
      try {
        const data = await api.post<unknown>(
          `${GRAPH_API}/cypher`,
          { template: "next_best_services", params: { person_id: personId } },
          { tenant: orgSlug },
        );
        if (signal?.aborted) return;
        const recs = unwrapList<Record<string, unknown>>(data)
          .map(normalizeRecommendation)
          .filter((r) => r.offering_id !== "")
          .sort((a, b) => (a.rank || Number.MAX_SAFE_INTEGER) - (b.rank || Number.MAX_SAFE_INTEGER));
        setState(
          recs.length === 0
            ? {
                status: "empty",
                message:
                  "No recommendations yet — they appear after the next scoring sweep has run for this workspace.",
              }
            : { status: "ok", recs },
        );
      } catch (e) {
        if (signal?.aborted) return;
        if (e instanceof ApiError && (e.status === 400 || e.status === 404)) {
          // Template not registered yet (pre-W29 backend) — explicit empty
          // state rather than an alarming error.
          setState({
            status: "empty",
            message:
              "Recommendations are not available yet on this workspace's graph service.",
          });
        } else {
          setState({
            status: "error",
            message: "Recommendations unavailable — the graph service may be offline.",
          });
        }
      }
    },
    [orgSlug, personId],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  /**
   * Audience handoff: rank this person's lookalikes via the similar_persons
   * template, then open the segment builder prefilled with the seed. The
   * builder stays consent-gated — the audience still contains only people
   * with a purpose-matching CONSENTED edge (SPEC-W29 §0.3).
   */
  async function createAudienceOfSimilar() {
    setSeeding(true);
    let count: number | null = null;
    try {
      const data = await api.post<unknown>(
        `${GRAPH_API}/cypher`,
        { template: "similar_persons", params: { person_id: personId, k: SIMILAR_K } },
        { tenant: orgSlug },
      );
      count = unwrapList<Record<string, unknown>>(data).length;
    } catch {
      // The similarity lookup is a nice-to-have for the seed note; the
      // builder prefill is still useful without it.
      count = null;
    }
    const params = new URLSearchParams({ similar_to: personId });
    if (personName) params.set("similar_name", personName);
    if (count !== null) params.set("similar_count", String(count));
    setSeeding(false);
    toast({
      title: "Segment builder prefilled",
      description:
        count !== null
          ? `Seeded from ${count} similar ${count === 1 ? "client" : "clients"}.`
          : "Seeded from this person.",
      variant: "success",
    });
    router.push(`/app/${orgSlug}/segments?${params.toString()}`);
  }

  const recs = state.status === "ok" ? state.recs : [];
  const provenance = recs.find((r) => r.model_version || r.scored_at);

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div>
            <CardTitle>Recommended next services</CardTitle>
            <CardDescription>
              Ranked by the workspace&apos;s own scoring sweep — scores rank
              within consent-eligible audiences; they never override consent
              or quarantine gates.
            </CardDescription>
          </div>
          <Button
            variant="outline"
            size="sm"
            onClick={() => void createAudienceOfSimilar()}
            disabled={seeding || state.status === "loading"}
          >
            {seeding ? (
              <Loader2 className="h-3.5 w-3.5 animate-spin" />
            ) : (
              <Users className="h-3.5 w-3.5" />
            )}
            Create audience of similar clients
          </Button>
        </div>
      </CardHeader>
      <CardContent>
        {state.status === "loading" ? (
          <p className="flex items-center gap-2 py-6 text-sm text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" />
            Scoring this person against your offerings…
          </p>
        ) : state.status === "error" ? (
          <div className="flex flex-wrap items-center justify-between gap-2 py-2">
            <p className="text-sm text-muted-foreground">{state.message}</p>
            <Button variant="outline" size="sm" onClick={() => void load()}>
              <RefreshCw className="h-3.5 w-3.5" />
              Retry
            </Button>
          </div>
        ) : state.status === "empty" ? (
          <p className="py-6 text-center text-sm text-muted-foreground">
            {state.message}
          </p>
        ) : (
          <ol className="space-y-3" aria-label="Recommended services, ranked">
            {recs.map((rec, i) => {
              const pct = Math.max(0, Math.min(1, rec.score)) * 100;
              const tone = TONE_COLORS[scoreTone(rec.score)];
              return (
                <li
                  key={`${rec.offering_id}-${i}`}
                  className="rounded-md border border-border px-3 py-2.5"
                  style={{ backgroundColor: BRAND.cream }}
                >
                  <div className="flex flex-wrap items-baseline justify-between gap-2">
                    <span className="text-sm font-medium">
                      <span className="mr-2 text-xs text-muted-foreground tabular-nums">
                        #{rec.rank || i + 1}
                      </span>
                      {rec.offering_name ?? rec.offering_id}
                    </span>
                    <span
                      className="text-xs font-semibold tabular-nums"
                      style={{ color: tone.fg }}
                    >
                      {Math.round(pct)}% match
                    </span>
                  </div>
                  <div
                    className="mt-2 h-2 w-full overflow-hidden rounded-full"
                    style={{ backgroundColor: `${tone.solid}22` }}
                    role="progressbar"
                    aria-valuenow={Math.round(pct)}
                    aria-valuemin={0}
                    aria-valuemax={100}
                    aria-label={`Match score for ${rec.offering_name ?? rec.offering_id}`}
                  >
                    <div
                      className="h-full rounded-full transition-all"
                      style={{ width: `${pct}%`, backgroundColor: tone.solid }}
                    />
                  </div>
                  {rec.reason ? (
                    <p className="mt-1.5 flex items-center gap-1.5 text-xs text-muted-foreground">
                      <Sparkles className="h-3 w-3 shrink-0" style={{ color: BRAND.amber }} />
                      {humanizeReason(rec.reason)}
                    </p>
                  ) : null}
                </li>
              );
            })}
          </ol>
        )}
        {provenance ? (
          <p className="mt-3 text-xs text-muted-foreground">
            Model {provenance.model_version ?? "—"}
            {provenance.scored_at ? ` · scored ${formatDateTime(provenance.scored_at)}` : ""}
          </p>
        ) : null}
      </CardContent>
    </Card>
  );
}
