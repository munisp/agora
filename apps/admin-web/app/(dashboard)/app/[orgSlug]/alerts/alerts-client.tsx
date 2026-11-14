"use client";

/**
 * SPEC-W30 WS-D: fraud & trust alert queue.
 *
 * Tenant-scoped list from the graph-service alerts router through the
 * existing /api/graph/* BFF mount (JWT attached by the BFF; the service
 * binds every query to the tenant — SPEC-W30 §5 gate 6):
 *
 *   GET /api/graph/v1/graph/alerts?status=&type=&severity=
 *
 * Filters (status / type / severity) are applied server-side via query
 * params; the person prefilter (deep link from Person 360) is additionally
 * enforced client-side so it works even before the router grows a
 * person_id param.
 */
// NAV: orchestrator adds Alerts item (Shield icon)
import * as React from "react";
import Link from "next/link";
import { RefreshCw, Shield, X } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { Label, Select } from "@/components/ui/input";
import { AlertsTable } from "@/components/alerts/alerts-table";
import { AlertDetail } from "@/components/alerts/alert-detail";
import {
  ALERTS_API,
  ALERT_STATUSES,
  ALERT_TYPES,
  normalizeAlert,
  type FraudAlert,
} from "@/components/alerts/types";
import { unwrapList } from "@/components/segments/types";

export function AlertsClient({
  orgSlug,
  initialPersonId,
}: {
  orgSlug: string;
  /** Deep link from Person 360 pre-filters the queue to one person. */
  initialPersonId?: string;
}) {
  const [alerts, setAlerts] = React.useState<FraudAlert[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [status, setStatus] = React.useState("open");
  const [type, setType] = React.useState("");
  const [severity, setSeverity] = React.useState("");
  const [personFilter, setPersonFilter] = React.useState(initialPersonId ?? "");
  const [selected, setSelected] = React.useState<FraudAlert | null>(null);
  const [drawerOpen, setDrawerOpen] = React.useState(false);

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<unknown>(
          ALERTS_API,
          {
            tenant: orgSlug,
            status: status || undefined,
            type: type || undefined,
            severity: severity || undefined,
            person_id: personFilter || undefined,
          },
          signal,
        );
        if (signal?.aborted) return;
        let rows = unwrapList<Record<string, unknown>>(data).map(normalizeAlert);
        // Client-side enforcement of the person prefilter (the router may
        // not support the param yet — the deep link must still work).
        if (personFilter) {
          rows = rows.filter((a) => a.person_id === personFilter);
        }
        setAlerts(rows);
      } catch (e) {
        if (signal?.aborted) return;
        setAlerts([]);
        setError(
          e instanceof ApiError && e.status === 404
            ? "The alert queue is not available yet on this workspace."
            : "Alerts unavailable — the graph service may be offline.",
        );
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [orgSlug, status, type, severity, personFilter],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  return (
    <div className="max-w-6xl space-y-4">
      <PageHeader
        title="Fraud & trust alerts"
        description="Graph-detected fraud signals — referral rings, duplicate identities, impossible travel, consent backdating and more. Detection never punishes by itself: humans adjudicate here."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => void load()}
            disabled={loading}
          >
            <RefreshCw className="h-3.5 w-3.5" />
            {loading ? "Loading…" : "Refresh"}
          </Button>
        }
      />

      {error ? <ErrorNote message={error} /> : null}

      <div className="flex flex-wrap items-end gap-3 rounded-md border border-border bg-card px-3 py-3">
        <div className="space-y-1">
          <Label htmlFor="alerts-status" className="text-xs">
            Status
          </Label>
          <Select
            id="alerts-status"
            className="w-36"
            value={status}
            onChange={(e) => setStatus(e.target.value)}
          >
            <option value="">All statuses</option>
            {ALERT_STATUSES.map((s) => (
              <option key={s.value} value={s.value}>
                {s.label}
              </option>
            ))}
          </Select>
        </div>
        <div className="space-y-1">
          <Label htmlFor="alerts-type" className="text-xs">
            Type
          </Label>
          <Select
            id="alerts-type"
            className="w-44"
            value={type}
            onChange={(e) => setType(e.target.value)}
          >
            <option value="">All types</option>
            {ALERT_TYPES.map((t) => (
              <option key={t.value} value={t.value}>
                {t.label}
              </option>
            ))}
          </Select>
        </div>
        <div className="space-y-1">
          <Label htmlFor="alerts-severity" className="text-xs">
            Severity
          </Label>
          <Select
            id="alerts-severity"
            className="w-36"
            value={severity}
            onChange={(e) => setSeverity(e.target.value)}
          >
            <option value="">All severities</option>
            <option value="low">Low</option>
            <option value="medium">Medium</option>
            <option value="high">High</option>
          </Select>
        </div>
        {personFilter ? (
          <span className="inline-flex items-center gap-2 rounded-full border border-border bg-muted px-3 py-1 text-xs">
            <Shield className="h-3 w-3" />
            Person <span className="font-mono">{personFilter}</span>
            <button
              type="button"
              aria-label="Clear person filter"
              className="cursor-pointer text-muted-foreground hover:text-foreground"
              onClick={() => setPersonFilter("")}
            >
              <X className="h-3 w-3" />
            </button>
          </span>
        ) : null}
      </div>

      <AlertsTable
        alerts={alerts}
        loading={loading}
        selectedId={selected?.alert_id ?? null}
        onSelect={(a) => {
          setSelected(a);
          setDrawerOpen(true);
        }}
      />

      {personFilter && !loading && alerts.length === 0 && !error ? (
        <p className="text-sm text-muted-foreground">
          No alerts for this person under the current filters —{" "}
          <button
            type="button"
            className="text-primary underline-offset-4 hover:underline cursor-pointer"
            onClick={() => setStatus("")}
          >
            show all statuses
          </button>
          {" · "}
          <Link
            href={`/app/${orgSlug}/segments/persons/${encodeURIComponent(personFilter)}`}
            className="text-primary underline-offset-4 hover:underline"
          >
            back to Person 360
          </Link>
          .
        </p>
      ) : null}

      <AlertDetail
        orgSlug={orgSlug}
        alert={selected}
        open={drawerOpen}
        onOpenChange={setDrawerOpen}
        onResolved={() => void load()}
      />
    </div>
  );
}
