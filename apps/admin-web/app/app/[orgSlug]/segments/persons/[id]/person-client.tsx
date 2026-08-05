"use client";

/**
 * SPEC-W28 WS-C: Person 360 page client (Graph Explorer v1).
 *
 * GET /v1/graph/persons/{id} through the BFF (tenant header attached;
 * graph-service injects the tenant filter, so a cross-tenant id answers 404
 * exactly like an unknown id — SPEC-W28 §5 gate 1). 404 renders an explicit
 * not-found state with a way back to segments.
 *
 * SPEC-W29 WS-C: propensity chips (churn/convert/turnout) + the ranked
 * "Recommended next services" panel mount alongside the W28 view; scores
 * render only when the tenant's scoring sweep has written them.
 *
 * SPEC-W30 WS-D: fraud risk chip (risk_score) + active risk_flags render
 * beside the propensity badges, with a deep link into the alerts queue
 * pre-filtered to this person.
 */
import * as React from "react";
import Link from "next/link";
import { ArrowLeft, RefreshCw, ShieldAlert } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { formatDateTime } from "@/lib/utils";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button, buttonVariants } from "@/components/ui/button";
import { Person360View } from "@/components/segments/person-360";
import { RecommendationsPanel } from "@/components/segments/recommendations-panel";
import {
  PropensityBadge,
  PropensityBadges,
} from "@/components/segments/propensity-badge";
import {
  BRAND,
  GRAPH_API,
  humanizeReason,
  normalizePerson360,
  type Person360,
} from "@/components/segments/types";

export function PersonClient({
  orgSlug,
  personId,
}: {
  orgSlug: string;
  personId: string;
}) {
  const [person, setPerson] = React.useState<Person360 | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [notFound, setNotFound] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setNotFound(false);
      setError(null);
      try {
        const data = await api.get<unknown>(
          `${GRAPH_API}/persons/${encodeURIComponent(personId)}`,
          { tenant: orgSlug },
        );
        if (signal?.aborted) return;
        setPerson(normalizePerson360(data));
      } catch (e) {
        if (signal?.aborted) return;
        setPerson(null);
        if (e instanceof ApiError && e.status === 404) {
          setNotFound(true);
        } else {
          setError("Person unavailable — the graph service may be offline.");
        }
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [orgSlug, personId],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  return (
    <div className="max-w-6xl">
      <PageHeader
        title="Person 360"
        description="Everything your customer graph knows about one person: captures, bookings, consents, referrals and messages."
        actions={
          <div className="flex items-center gap-2">
            <Link
              href={`/app/${orgSlug}/segments`}
              className={buttonVariants({ variant: "outline", size: "sm" })}
            >
              <ArrowLeft className="h-3.5 w-3.5" />
              Segments
            </Link>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void load()}
              disabled={loading}
            >
              <RefreshCw className="h-3.5 w-3.5" />
              {loading ? "Loading…" : "Refresh"}
            </Button>
          </div>
        }
      />

      {error ? <ErrorNote message={error} /> : null}

      {notFound ? (
        <div className="rounded-md border border-border bg-card px-6 py-12 text-center">
          <p className="text-sm font-medium">Person not found</p>
          <p className="mt-1 text-sm text-muted-foreground">
            No person with id <span className="font-mono">{personId}</span>{" "}
            exists in this workspace&apos;s graph (or it was erased).
          </p>
          <Link
            href={`/app/${orgSlug}/segments`}
            className="mt-4 inline-flex items-center gap-2 text-sm text-primary underline-offset-4 hover:underline"
          >
            <ArrowLeft className="h-3.5 w-3.5" />
            Back to segments
          </Link>
        </div>
      ) : loading && !person ? (
        <p className="py-12 text-center text-sm text-muted-foreground">
          Loading person…
        </p>
      ) : person ? (
        <div className="space-y-4">
          {/* SPEC-W29/W30: predictive + fraud-trust header row. Chips render
              only for scores the sweep has actually written. */}
          <div className="flex flex-wrap items-center gap-2">
            <PropensityBadges
              churn={person.propensity_churn}
              convert={person.propensity_convert}
              turnout={person.propensity_turnout}
            />
            {person.risk_score !== undefined ? (
              <PropensityBadge
                label="Risk"
                score={person.risk_score}
                title="Fraud risk score from the tenant scoring sweep (graph-neighborhood outlier detection)."
              />
            ) : null}
            {(person.risk_flags ?? []).map((flag) => (
              <span
                key={flag}
                title={`Active fraud detector flag: ${flag}`}
                className="inline-flex items-center gap-1 rounded-full border px-2.5 py-0.5 text-xs font-medium"
                style={{
                  color: BRAND.terracotta,
                  backgroundColor: `${BRAND.terracotta}1a`,
                  borderColor: `${BRAND.terracotta}59`,
                }}
              >
                <ShieldAlert className="h-3 w-3" />
                {humanizeReason(flag)}
              </span>
            ))}
            {person.risk_score !== undefined ||
            (person.risk_flags ?? []).length > 0 ? (
              <Link
                href={`/app/${orgSlug}/alerts?person_id=${encodeURIComponent(person.person_id || personId)}`}
                className="text-xs text-primary underline-offset-4 hover:underline"
              >
                View this person&apos;s alerts
              </Link>
            ) : null}
            {person.model_version || person.scored_at ? (
              <span className="text-xs text-muted-foreground">
                {person.model_version ? `Model ${person.model_version}` : ""}
                {person.model_version && person.scored_at ? " · " : ""}
                {person.scored_at ? `scored ${formatDateTime(person.scored_at)}` : ""}
              </span>
            ) : null}
          </div>

          <Person360View person={person} />

          <RecommendationsPanel
            orgSlug={orgSlug}
            // Surfaced from the person_by_id projection (envelope-unwrapped
            // in normalizePerson360); the route id is the fallback so the
            // panel never queries with an empty person_id.
            personId={person.person_id || personId}
            personName={person.name}
          />
        </div>
      ) : null}
    </div>
  );
}
