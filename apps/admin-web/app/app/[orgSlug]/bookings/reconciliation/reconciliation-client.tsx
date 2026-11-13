"use client";

/**
 * SPEC-W56 Scope C: receipt ↔ payment reconciliation findings, produced by
 * the booking-service recon sweep (internal/recon) over field-captured
 * receipts (W53 receipt_fields) vs payment state:
 *
 *   GET /api/bookings/v1/recon/receipts[?tenant=<slug>][&status=open|resolved|all][&limit=<n>]
 *   → { findings: [{ id, run_id, kind, confidence, capture_client_id?,
 *                    booking_id?, attempt_tx_ref?, expected_amount_minor?,
 *                    captured_amount_minor?, receipt_amount_major?,
 *                    receipt_currency?, detail, created_at, resolved_at? }] }
 *
 * Honesty notes carried from RECON §5 / SPEC-W56 pinned decisions:
 *  - R1: `exact` matches come ONLY from the checkin contact chain;
 *    `heuristic` from the lead-phone chain or amount+date; an ambiguous
 *    match (>1 candidate at the winning rung) lands as confidence='none'
 *    with the candidate list in `detail`.
 *  - R2: receipts carry NO currency — amounts below are rendered WITHOUT a
 *    currency symbol and comparisons assumed the offering currency
 *    (`detail.currency_unknown` is true on every finding).
 *  - `amount_mismatch` here is paper-vs-rail (receipt vs captured amount),
 *    DISTINCT from the W49 rail-vs-price payment mismatch.
 */
import * as React from "react";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Label, Select } from "@/components/ui/input";
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

export interface ReconFinding {
  id: string;
  run_id?: string;
  kind: string;
  confidence: string;
  capture_client_id?: string | null;
  booking_id?: string | null;
  attempt_tx_ref?: string | null;
  expected_amount_minor?: number | null;
  captured_amount_minor?: number | null;
  receipt_amount_major?: number | null;
  receipt_currency?: string | null;
  /** JSONB; tolerated as object or JSON text. */
  detail?: Record<string, unknown> | string | null;
  created_at: string;
  resolved_at?: string | null;
}

/** SPEC-W56 pinned decision 5 severity mapping, mirrored for display. */
const KIND_META: Record<
  string,
  { label: string; severity: string; variant: "destructive" | "info" | "warning" }
> = {
  receipt_without_payment: {
    label: "Receipt without payment",
    severity: "high",
    variant: "destructive",
  },
  amount_mismatch: {
    label: "Amount mismatch",
    severity: "high",
    variant: "destructive",
  },
  payment_without_receipt: {
    label: "Payment without receipt",
    severity: "info",
    variant: "info",
  },
  currency_unknown: {
    label: "Currency unknown",
    severity: "high",
    variant: "warning",
  },
};

function kindMeta(kind: string) {
  return (
    KIND_META[kind] ?? {
      label: kind.replace(/_/g, " "),
      severity: "info",
      variant: "info" as const,
    }
  );
}

function confidenceVariant(
  c: string,
): "success" | "warning" | "secondary" {
  const v = c.toLowerCase();
  if (v === "exact") return "success";
  if (v === "heuristic") return "warning";
  return "secondary";
}

/**
 * R2 honesty: receipts carry no currency and minor-unit columns assume the
 * offering currency — format as a bare number, never with a currency symbol.
 */
function formatMinor(minor?: number | null): string {
  if (minor === null || minor === undefined) return "—";
  return new Intl.NumberFormat("en-US", {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(minor / 100);
}

function formatMajor(major?: number | null): string {
  if (major === null || major === undefined) return "—";
  return new Intl.NumberFormat("en-US", {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(major);
}

/** detail is JSONB; tolerate a pre-parsed object or JSON text. */
function detailObject(raw: ReconFinding["detail"]): Record<string, unknown> {
  if (typeof raw === "string") {
    try {
      const parsed: unknown = JSON.parse(raw);
      if (typeof parsed === "object" && parsed !== null && !Array.isArray(parsed)) {
        return parsed as Record<string, unknown>;
      }
      return { value: parsed };
    } catch {
      return { raw };
    }
  }
  if (typeof raw === "object" && raw !== null && !Array.isArray(raw)) {
    return raw;
  }
  return {};
}

const STATUSES = [
  { value: "open", label: "Open" },
  { value: "resolved", label: "Resolved" },
  { value: "all", label: "All" },
] as const;

// Keep in sync with the server-side fetch in page.tsx.
const DEFAULT_STATUS = "open";
const PAGE_SIZE = 100;

export function ReconciliationClient({
  orgSlug,
  initialFindings,
  initialError,
}: {
  orgSlug: string;
  /** SPEC-W46 AW-2: first page fetched server-side in page.tsx. */
  initialFindings: ReconFinding[];
  initialError: string | null;
}) {
  const [findings, setFindings] = React.useState<ReconFinding[]>(initialFindings);
  const [status, setStatus] = React.useState<string>(DEFAULT_STATUS);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState<string | null>(initialError);

  // Keep the table in sync when the server props change (router.refresh()).
  React.useEffect(() => {
    setFindings(initialFindings);
    setError(initialError);
  }, [initialFindings, initialError]);

  const load = React.useCallback(
    async (nextStatus: string = status) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<{ findings?: ReconFinding[] }>(
          "/api/bookings/v1/recon/receipts",
          { tenant: orgSlug, status: nextStatus, limit: PAGE_SIZE },
        );
        setFindings(data.findings ?? []);
      } catch (e) {
        setError(
          e instanceof ApiError && e.status === 403
            ? "Receipt reconciliation requires the manage_bookings permission."
            : e instanceof ApiError
              ? e.message
              : "Failed to load reconciliation findings.",
        );
      } finally {
        setLoading(false);
      }
    },
    [orgSlug, status],
  );

  return (
    <div>
      <PageHeader
        title="Receipt reconciliation"
        description="Field-captured receipts matched against payment state. Receipts carry no currency — amounts assume the offering currency and are shown without a currency symbol."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => void load()}
            disabled={loading}
          >
            <RefreshCw className="h-4 w-4" />
            {loading ? "Loading…" : "Refresh"}
          </Button>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <div className="mb-3 flex flex-wrap items-end gap-3 rounded-md border border-border bg-card px-3 py-3">
        <div className="space-y-1">
          <Label htmlFor="recon-status" className="text-xs">
            Status
          </Label>
          <Select
            id="recon-status"
            className="w-36"
            value={status}
            onChange={(e) => {
              setStatus(e.target.value);
              void load(e.target.value);
            }}
            disabled={loading}
          >
            {STATUSES.map((s) => (
              <option key={s.value} value={s.value}>
                {s.label}
              </option>
            ))}
          </Select>
        </div>
      </div>

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">Kind</TableHead>
              <TableHead>Confidence</TableHead>
              <TableHead className="text-right">Expected</TableHead>
              <TableHead className="text-right">Captured</TableHead>
              <TableHead className="text-right">Receipt</TableHead>
              <TableHead>Created</TableHead>
              <TableHead className="pr-5">Detail</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {findings.length === 0 ? (
              <TableEmpty colSpan={7}>
                {loading
                  ? "Loading…"
                  : "No reconciliation findings. Findings appear once staff upload receipts via the capture API and a sweep runs."}
              </TableEmpty>
            ) : (
              findings.map((f) => {
                const km = kindMeta(f.kind);
                return (
                  <TableRow key={f.id}>
                    <TableCell className="pl-5">
                      <Badge variant={km.variant}>{km.label}</Badge>
                      <span className="block text-xs text-muted-foreground">
                        severity: {km.severity}
                      </span>
                    </TableCell>
                    <TableCell>
                      <Badge variant={confidenceVariant(f.confidence)}>
                        {f.confidence || "—"}
                      </Badge>
                    </TableCell>
                    <TableCell className="text-right text-sm tabular-nums">
                      {formatMinor(f.expected_amount_minor)}
                    </TableCell>
                    <TableCell className="text-right text-sm tabular-nums">
                      {formatMinor(f.captured_amount_minor)}
                    </TableCell>
                    <TableCell className="text-right text-sm tabular-nums">
                      {formatMajor(f.receipt_amount_major)}
                    </TableCell>
                    <TableCell className="text-sm">
                      {formatDateTime(f.created_at)}
                      {f.resolved_at ? (
                        <span className="block text-xs text-muted-foreground">
                          resolved {formatDateTime(f.resolved_at)}
                        </span>
                      ) : null}
                    </TableCell>
                    <TableCell className="pr-5">
                      <details className="rounded-md border border-border px-3 py-2 text-xs">
                        <summary className="cursor-pointer font-medium text-muted-foreground">
                          Detail JSON
                        </summary>
                        <pre className="mt-2 max-w-md overflow-x-auto rounded bg-muted p-2 font-mono text-[11px] leading-relaxed">
                          {JSON.stringify(detailObject(f.detail), null, 2)}
                        </pre>
                      </details>
                    </TableCell>
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </Card>
    </div>
  );
}
