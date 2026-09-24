"use client";

/**
 * SPEC-W45 ORPH O15 ops alerts (notification-worker read-back):
 *   GET /api/notifications/v1/ops-alerts[?tenant=<slug>][&limit=<n>]
 *   → { alerts: [{ id, event_id, tenant_id, source, type, severity,
 *                  payload, received_at }] }
 * `payload` is the raw CloudEvent JSON (Go []byte → base64 in JSON); the
 * human-readable message is extracted from it best-effort.
 */
import * as React from "react";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDateTime } from "@/lib/utils";

export interface OpsAlert {
  id: string;
  event_id: string;
  tenant_id: string;
  source: string;
  type: string;
  severity: string;
  /** raw CloudEvent JSON; base64 (Go []byte) or already-parsed object */
  payload?: string | Record<string, unknown>;
  received_at: string;
}

/** Best-effort message extraction from the persisted CloudEvent payload. */
function alertMessage(a: OpsAlert): string {
  let evt: unknown = a.payload;
  if (typeof evt === "string" && evt.length > 0) {
    const raw: string = evt;
    try {
      // Go marshals []byte as base64; decode then parse. If it already is
      // JSON text the first parse succeeds without atob.
      try {
        evt = JSON.parse(raw);
      } catch {
        evt = JSON.parse(atob(raw));
      }
    } catch {
      return "";
    }
  }
  if (typeof evt !== "object" || evt === null) return "";
  const data = (evt as { data?: unknown }).data;
  if (typeof data !== "object" || data === null) return "";
  const d = data as Record<string, unknown>;
  for (const k of ["message", "title", "summary", "reason", "error"]) {
    if (typeof d[k] === "string" && d[k]) return d[k] as string;
  }
  return "";
}

function severityVariant(
  s: string,
): "destructive" | "warning" | "secondary" {
  const v = s.toLowerCase();
  if (v === "critical" || v === "high" || v === "error") return "destructive";
  if (v === "warning" || v === "medium") return "warning";
  return "secondary";
}

/**
 * SPEC-W46 AW-4: page size for the bounded list. Must match the server-side
 * fetch in page.tsx. The endpoint accepts limit (clamped to 500 server-side)
 * but no cursor, so "load more" refetches with a larger limit.
 */
const PAGE_SIZE = 100;
const MAX_LIMIT = 500;

export function OpsAlertsClient({
  orgSlug,
  initialAlerts,
  initialError,
}: {
  orgSlug: string;
  /** SPEC-W46 AW-2: first page fetched server-side in page.tsx. */
  initialAlerts: OpsAlert[];
  initialError: string | null;
}) {
  const [alerts, setAlerts] = React.useState<OpsAlert[]>(initialAlerts);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState<string | null>(initialError);
  const [limit, setLimit] = React.useState(PAGE_SIZE);

  // Keep the table in sync when the server props change (router.refresh()).
  React.useEffect(() => {
    setAlerts(initialAlerts);
    setError(initialError);
  }, [initialAlerts, initialError]);

  const load = React.useCallback(
    async (nextLimit: number = limit) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<{ alerts?: OpsAlert[] }>(
          "/api/notifications/v1/ops-alerts",
          { tenant: orgSlug, limit: nextLimit },
        );
        setAlerts(data.alerts ?? []);
      } catch (e) {
        setError(
          e instanceof ApiError && e.status === 403
            ? "Ops alerts require an admin role."
            : e instanceof ApiError
              ? e.message
              : "Failed to load ops alerts.",
        );
      } finally {
        setLoading(false);
      }
    },
    [orgSlug, limit],
  );

  const loadMore = () => {
    const nextLimit = Math.min(limit + PAGE_SIZE, MAX_LIMIT);
    setLimit(nextLimit);
    void load(nextLimit);
  };

  // A full page means there may be more rows on the server.
  const canLoadMore = alerts.length >= limit && limit < MAX_LIMIT;

  return (
    <div>
      <PageHeader
        title="Ops alerts"
        description="Platform operational alerts persisted from the ops alert stream."
        actions={
          <Button variant="outline" size="sm" onClick={() => void load()}>
            <RefreshCw className="h-4 w-4" /> Refresh
          </Button>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">Severity</TableHead>
              <TableHead>Source</TableHead>
              <TableHead>Message</TableHead>
              <TableHead className="pr-5">Time</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {alerts.length === 0 ? (
              <TableEmpty colSpan={4}>
                {loading ? "Loading…" : "No ops alerts."}
              </TableEmpty>
            ) : (
              alerts.map((a) => (
                <TableRow key={a.id}>
                  <TableCell className="pl-5">
                    <Badge variant={severityVariant(a.severity)}>
                      {a.severity || "—"}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-sm">
                    {a.source || "—"}
                    <span className="block text-xs text-muted-foreground">
                      {a.type}
                    </span>
                  </TableCell>
                  <TableCell className="max-w-md truncate text-sm">
                    {alertMessage(a) || a.event_id}
                  </TableCell>
                  <TableCell className="pr-5 text-sm">
                    {formatDateTime(a.received_at)}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </Card>

      {canLoadMore ? (
        <div className="mt-3 flex justify-center">
          <Button
            variant="outline"
            size="sm"
            onClick={loadMore}
            disabled={loading}
          >
            {loading ? "Loading…" : "Load more"}
          </Button>
        </div>
      ) : null}
    </div>
  );
}
