"use client";

/**
 * Journey stats panel (SPEC-W19 Agent D): enrolled/active/completed/
 * exited tiles + the per-step breakdown from GET /journeys/{id}/stats.
 */

import * as React from "react";
import { api } from "@/lib/api";
import { STUDIO_API, type Journey, type JourneyStats } from "./types";

export function StatsPanel({
  orgSlug,
  journey,
  refreshKey,
}: {
  orgSlug: string;
  journey: Journey;
  /** bump to re-fetch (after a step run / enroll) */
  refreshKey: number;
}) {
  const [stats, setStats] = React.useState<JourneyStats | null>(null);
  const [error, setError] = React.useState<string | null>(null);

  React.useEffect(() => {
    const controller = new AbortController();
    (async () => {
      try {
        const res = await api.get<{ stats: JourneyStats }>(
          `${STUDIO_API}/journeys/${journey.id}/stats`,
          { tenant: orgSlug },
          controller.signal,
        );
        if (!controller.signal.aborted) {
          setStats(res.stats);
          setError(null);
        }
      } catch {
        if (!controller.signal.aborted) setError("Stats unavailable.");
      }
    })();
    return () => controller.abort();
  }, [orgSlug, journey.id, refreshKey]);

  if (error) return <p className="text-sm text-muted-foreground">{error}</p>;
  if (!stats) return <p className="text-sm text-muted-foreground">Loading stats…</p>;

  const tiles: { label: string; value: number }[] = [
    { label: "Enrolled", value: stats.enrolled },
    { label: "Active", value: stats.active },
    { label: "Completed", value: stats.completed },
    { label: "Exited", value: stats.exited },
  ];

  return (
    <div className="space-y-3">
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
        {tiles.map((t) => (
          <div key={t.label} className="rounded-md border p-3 text-center">
            <p className="text-xl font-semibold">{t.value.toLocaleString()}</p>
            <p className="text-xs text-muted-foreground">{t.label}</p>
          </div>
        ))}
      </div>
      {stats.per_step.length > 0 ? (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-xs text-muted-foreground">
                <th className="py-1 pr-2 font-medium">Step</th>
                <th className="py-1 pr-2 font-medium">Type</th>
                <th className="py-1 pr-2 font-medium">Waiting</th>
                <th className="py-1 pr-2 font-medium">Passed</th>
                <th className="py-1 pr-2 font-medium">Sent</th>
                <th className="py-1 pr-2 font-medium">Suppressed</th>
                <th className="py-1 pr-2 font-medium">Skipped</th>
                <th className="py-1 pr-2 font-medium">Failed</th>
                <th className="py-1 font-medium">Exited</th>
              </tr>
            </thead>
            <tbody>
              {stats.per_step.map((s) => (
                <tr key={s.step_idx} className="border-b last:border-0">
                  <td className="py-1 pr-2">{s.step_idx + 1}</td>
                  <td className="py-1 pr-2">{s.type}</td>
                  <td className="py-1 pr-2">{s.active}</td>
                  <td className="py-1 pr-2">{s.passed}</td>
                  <td className="py-1 pr-2">{s.sent}</td>
                  <td className="py-1 pr-2">{s.suppressed}</td>
                  <td className="py-1 pr-2">{s.skipped}</td>
                  <td className="py-1 pr-2">{s.failed}</td>
                  <td className="py-1">{s.exited}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </div>
  );
}
