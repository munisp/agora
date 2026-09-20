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

interface OpsAlert {
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

export function OpsAlertsClient({ orgSlug }: { orgSlug: string }) {
  const [alerts, setAlerts] = React.useState<OpsAlert[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);

  const load = React.useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await api.get<{ alerts?: OpsAlert[] }>(
        "/api/notifications/v1/ops-alerts",
        { tenant: orgSlug, limit: 200 },
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
  }, [orgSlug]);

  React.useEffect(() => {
    void load();
  }, [load]);

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
    </div>
  );
}
