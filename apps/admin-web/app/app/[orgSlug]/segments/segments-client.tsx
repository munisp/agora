"use client";

/**
 * SPEC-W28 WS-C: Segments page — Segment Builder + saved segment list +
 * Ask box over the tenant knowledge graph.
 *
 * All data flows through the BFF with the tenant header attached:
 *   - graph-service via the /api/graph/* gateway mount (segments, counts,
 *     ask, person 360 — see components/segments/types.ts);
 *   - notification-worker audience intake via the existing
 *     /api/notifications/* mount (campaign handoff).
 */
import * as React from "react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { AskBox } from "@/components/segments/ask-box";
import { SegmentBuilder } from "@/components/segments/segment-builder";
import { SegmentList } from "@/components/segments/segment-list";
import {
  GRAPH_API,
  normalizeSegment,
  unwrapList,
  type Segment,
} from "@/components/segments/types";

export function SegmentsClient({ orgSlug }: { orgSlug: string }) {
  const [segments, setSegments] = React.useState<Segment[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<unknown>(`${GRAPH_API}/segments`, {
          tenant: orgSlug,
        });
        if (signal?.aborted) return;
        setSegments(
          unwrapList<Record<string, unknown>>(data).map(normalizeSegment),
        );
      } catch (e) {
        if (signal?.aborted) return;
        setSegments([]);
        setError(
          e instanceof ApiError && e.status === 404
            ? "Segment listing is not available yet."
            : "Segments unavailable — the graph service may be offline.",
        );
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  return (
    <div className="max-w-6xl space-y-4">
      <PageHeader
        title="Segments"
        description="Build consent-gated audiences over your customer graph, preview their size, and hand them to a campaign. Consent, DND and quiet-hours rules are enforced on every send."
      />
      {error ? <ErrorNote message={error} /> : null}
      <SegmentBuilder
        orgSlug={orgSlug}
        onSaved={(segment) => setSegments((prev) => [segment, ...prev])}
      />
      <SegmentList
        orgSlug={orgSlug}
        segments={segments}
        loading={loading}
        error={null}
        onRefresh={() => void load()}
      />
      <AskBox orgSlug={orgSlug} />
    </div>
  );
}
