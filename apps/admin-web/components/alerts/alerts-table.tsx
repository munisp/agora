"use client";

/**
 * SPEC-W30 WS-D: fraud alert queue table. Purely presentational — the
 * owning page client fetches, filters and opens the detail drawer.
 */
import { Badge } from "@/components/ui/badge";
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
import {
  alertTypeLabel,
  severityMeta,
  type FraudAlert,
} from "./types";

/** Severity chip — sage/amber/terracotta ramp (brand: never red/blue). */
export function SeverityChip({ severity }: { severity: string }) {
  const meta = severityMeta(severity);
  return (
    <span
      className="inline-flex items-center rounded-full border px-2.5 py-0.5 text-xs font-medium"
      style={{ color: meta.fg, backgroundColor: meta.bg, borderColor: meta.border }}
    >
      {meta.label}
    </span>
  );
}

function StatusBadge({ status }: { status: string }) {
  const variant =
    status === "confirmed"
      ? "warning"
      : status === "dismissed"
        ? "secondary"
        : "info";
  return <Badge variant={variant}>{status}</Badge>;
}

export function AlertsTable({
  alerts,
  loading,
  selectedId,
  onSelect,
}: {
  alerts: FraudAlert[];
  loading: boolean;
  selectedId?: string | null;
  onSelect: (alert: FraudAlert) => void;
}) {
  return (
    <div className="overflow-x-auto rounded-md border border-border bg-card">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Severity</TableHead>
            <TableHead>Type</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Person</TableHead>
            <TableHead>Agent</TableHead>
            <TableHead>Raised</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {alerts.length === 0 ? (
            <TableEmpty colSpan={6}>
              {loading
                ? "Loading alerts…"
                : "No alerts match the current filters — the queue is clear."}
            </TableEmpty>
          ) : (
            alerts.map((a) => (
              <TableRow
                key={a.alert_id}
                className="cursor-pointer"
                aria-selected={selectedId === a.alert_id}
                onClick={() => onSelect(a)}
              >
                <TableCell>
                  <SeverityChip severity={a.severity} />
                </TableCell>
                <TableCell className="font-medium">{alertTypeLabel(a.type)}</TableCell>
                <TableCell>
                  <StatusBadge status={a.status} />
                </TableCell>
                <TableCell className="font-mono text-xs">
                  {a.person_id ?? "—"}
                </TableCell>
                <TableCell className="font-mono text-xs">
                  {a.agent_id ?? "—"}
                </TableCell>
                <TableCell className="text-sm">
                  {a.created_at ? formatDateTime(a.created_at) : "—"}
                </TableCell>
              </TableRow>
            ))
          )}
        </TableBody>
      </Table>
    </div>
  );
}
