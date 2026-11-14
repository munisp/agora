"use client";

/**
 * Campaign Studio client (SPEC-W19 Agent D): journeys tab (list + editor +
 * stats) and segments tab (builder + count). All traffic goes through the
 * BFF with the /api/bookings/v1/studio/... path style (consistent with the
 * backend handlers in booking-service internal/campaignstudio):
 *
 *   GET/POST        /api/bookings/v1/studio/segments
 *   PATCH           /api/bookings/v1/studio/segments/{id}
 *   POST            /api/bookings/v1/studio/segments/{id}/count
 *   GET/POST        /api/bookings/v1/studio/journeys
 *   GET/PATCH       /api/bookings/v1/studio/journeys/{id}
 *   POST            /api/bookings/v1/studio/journeys/{id}/enroll
 *   POST            /api/bookings/v1/studio/journeys/{id}/step
 *   GET             /api/bookings/v1/studio/journeys/{id}/stats
 *
 * Lists are read through the tolerant unwrap<T>() convention
 * (components/apps/types.ts, READ-ONLY import). Honest loading/empty/error
 * states throughout; mutations are owner/admin only (`canManage`, computed
 * server-side in page.tsx) — the backend enforces manage_bookings again.
 */

import * as React from "react";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { unwrap } from "@/components/apps/types";
import {
  STUDIO_API,
  type Journey,
  type Segment,
} from "@/components/apps/campaign-studio/types";
import { JourneyList } from "@/components/apps/campaign-studio/journey-list";
import { JourneyEditor } from "@/components/apps/campaign-studio/journey-editor";
import { SegmentBuilder } from "@/components/apps/campaign-studio/segment-builder";

export function StudioClient({
  orgSlug,
  canManage,
}: {
  orgSlug: string;
  /** owner/admin only (server-computed in page.tsx) */
  canManage: boolean;
}) {
  const [journeys, setJourneys] = React.useState<Journey[]>([]);
  const [segments, setSegments] = React.useState<Segment[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [editing, setEditing] = React.useState<Journey | null>(null);
  const [editorOpen, setEditorOpen] = React.useState(false);

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const [jData, sData] = await Promise.all([
          api.get<unknown>(`${STUDIO_API}/journeys`, { tenant: orgSlug }, signal),
          api.get<unknown>(`${STUDIO_API}/segments`, { tenant: orgSlug }, signal),
        ]);
        if (signal?.aborted) return;
        setJourneys(unwrap<Journey>(jData));
        setSegments(unwrap<Segment>(sData));
      } catch (e) {
        if (signal?.aborted) return;
        setJourneys([]);
        setSegments([]);
        setError(
          e instanceof ApiError && e.status !== 404
            ? e.message
            : "Campaign Studio is not available yet — the booking-service studio API may still be rolling out, or the campaign-studio app is not enabled for this tenant.",
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

  const refresh = () => void load();

  return (
    <div>
      <PageHeader
        title="Campaign Studio"
        description="Segments, journeys and enrollment — lifecycle messaging on the paced notification plane (DND + quiet-hours enforced automatically)."
        actions={
          <div className="flex items-center gap-2">
            <Button variant="outline" size="sm" onClick={refresh} disabled={loading}>
              <RefreshCw className={`mr-1 h-3.5 w-3.5 ${loading ? "animate-spin" : ""}`} />
              Refresh
            </Button>
            {canManage ? (
              <Button
                size="sm"
                onClick={() => {
                  setEditing(null);
                  setEditorOpen(true);
                }}
              >
                New journey
              </Button>
            ) : null}
          </div>
        }
      />
      {error ? <ErrorNote message={error} /> : null}
      {loading && journeys.length === 0 && segments.length === 0 ? (
        <p className="text-sm text-muted-foreground">Loading campaign studio…</p>
      ) : (
        <Tabs defaultValue="journeys">
          <TabsList>
            <TabsTrigger value="journeys">Journeys</TabsTrigger>
            <TabsTrigger value="segments">Segments</TabsTrigger>
          </TabsList>
          <TabsContent value="journeys" className="mt-4 space-y-4">
            {editorOpen && canManage ? (
              <JourneyEditor
                orgSlug={orgSlug}
                journey={editing}
                segments={segments}
                onSaved={() => {
                  setEditorOpen(false);
                  setEditing(null);
                  refresh();
                }}
                onCancel={() => {
                  setEditorOpen(false);
                  setEditing(null);
                }}
              />
            ) : null}
            <JourneyList
              orgSlug={orgSlug}
              canManage={canManage}
              journeys={journeys}
              onEdit={(j) => {
                setEditing(j);
                setEditorOpen(true);
              }}
              onChanged={refresh}
            />
          </TabsContent>
          <TabsContent value="segments" className="mt-4">
            <SegmentBuilder
              orgSlug={orgSlug}
              canManage={canManage}
              segments={segments}
              onChanged={refresh}
            />
          </TabsContent>
        </Tabs>
      )}
    </div>
  );
}
