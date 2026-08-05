"use client";

/**
 * SPEC-W28 WS-C: Person 360 page client (Graph Explorer v1).
 *
 * GET /v1/graph/persons/{id} through the BFF (tenant header attached;
 * graph-service injects the tenant filter, so a cross-tenant id answers 404
 * exactly like an unknown id — SPEC-W28 §5 gate 1). 404 renders an explicit
 * not-found state with a way back to segments.
 */
import * as React from "react";
import Link from "next/link";
import { ArrowLeft, RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button, buttonVariants } from "@/components/ui/button";
import { Person360View } from "@/components/segments/person-360";
import {
  GRAPH_API,
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
        <Person360View person={person} />
      ) : null}
    </div>
  );
}
