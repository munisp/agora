"use client";

/**
 * SPEC-W56 Scope C: receipt ↔ payment reconciliation findings, produced by
 * the booking-service recon sweep (internal/recon) over field-captured
 * receipts (W53 receipt_fields) vs payment state:
 *
 *   GET  /api/bookings/v1/recon/receipts[?tenant=<slug>][&status=open|resolved|all][&limit=<n>]
 *   → { findings: [{ id, run_id, kind, confidence, capture_client_id?,
 *                    booking_id?, attempt_tx_ref?, expected_amount_minor?,
 *                    captured_amount_minor?, receipt_amount_major?,
 *                    receipt_currency?, detail, created_at, resolved_at? }] }
 *   POST /api/bookings/v1/recon/receipts/{id}/resolve   (SPEC-W64: sets
 *     resolved_at + emits ReceiptReconResolved; 404 not_found_or_resolved on
 *     an already-resolved/unknown id — treated as "refresh + toast")
 *
 * Honesty notes carried from RECON §5 / SPEC-W56+W57 pinned decisions:
 *  - R1: `exact` matches come from rung 0 (payment_receipt payload
 *    booking_id → direct booking link, W57) or the checkin contact chain;
 *    `heuristic` from the lead-phone chain or amount+date; an ambiguous
 *    match (>1 candidate at the winning rung) lands as confidence='none'
 *    with the candidate list in `detail`.
 *  - R2 (W57 currency fold-in): the ocr-service always extracted `currency`
 *    and it is stored verbatim in receipt_fields; recon now parses it.
 *    `detail.currency_unknown` is true ONLY when the receipt currency is
 *    absent — when known, the currency code is appended to the rendered
 *    amounts. When receipt and rail currencies both exist and DIFFER, the
 *    pair is not comparable (`detail.currency_mismatch`) and amounts are
 *    NEVER converted (no FX rates exist). Pre-W57 findings keep
 *    `currency_unknown: true` as historical record.
 *  - `amount_mismatch` here is paper-vs-rail (receipt vs captured amount),
 *    DISTINCT from the W49 rail-vs-price payment mismatch.
 */
import * as React from "react";
import Link from "next/link";
import { CheckCircle2, RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
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
import { useToast } from "@/components/ui/toast";
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
 * R2 honesty (post-W57 fold-in): minor-unit columns assume the rail/offering
 * currency and are formatted as a bare number UNLESS the finding's currency
 * is known — then the currency CODE (never a symbol) is appended. When
 * `detail.currency_unknown` is true the bare number is kept, so an unknown
 * currency is never implied.
 */
function formatMinor(minor?: number | null, currency?: string | null): string {
  if (minor === null || minor === undefined) return "—";
  const n = new Intl.NumberFormat("en-US", {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(minor / 100);
  return currency ? `${n} ${currency}` : n;
}

function formatMajor(major?: number | null, currency?: string | null): string {
  if (major === null || major === undefined) return "—";
  const n = new Intl.NumberFormat("en-US", {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(major);
  return currency ? `${n} ${currency}` : n;
}

function asCode(v: unknown): string | null {
  return typeof v === "string" && v.trim() !== "" ? v.trim() : null;
}

/**
 * Per-finding display currencies, derived from detail (P7):
 *  - `currency_unknown: true` (receipt currency absent) => both null, bare
 *    numbers everywhere.
 *  - receipt currency known => receipt column takes `receipt_currency`;
 *    rail columns take the same code UNLESS `currency_mismatch` marks a
 *    divergence, in which case rail columns take `rail_currency` and the two
 *    columns honestly show their own (never converted) currencies.
 */
function findingCurrencies(f: ReconFinding): {
  receipt: string | null;
  rail: string | null;
} {
  const d = detailObject(f.detail);
  if (d.currency_unknown === true) return { receipt: null, rail: null };
  const receipt = asCode(d.receipt_currency) ?? asCode(f.receipt_currency);
  if (!receipt) return { receipt: null, rail: null };
  const rail =
    d.currency_mismatch === true ? (asCode(d.rail_currency) ?? receipt) : receipt;
  return { receipt, rail };
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
  const { toast } = useToast();
  // SPEC-W64: resolve mutation state — confirm dialog target + busy flag.
  const [resolving, setResolving] = React.useState<ReconFinding | null>(null);
  const [busy, setBusy] = React.useState(false);

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

  /**
   * SPEC-W64: resolve a finding (POST /v1/recon/receipts/{id}/resolve).
   * NOT optimistic — the list is refetched on success (media-inbox link /
   * discard idiom). A 404 (not_found_or_resolved) means the row was already
   * resolved (or is gone): surface a toast and refresh to converge.
   */
  const resolve = async () => {
    if (!resolving || busy) return;
    setBusy(true);
    try {
      await api.post(
        `/api/bookings/v1/recon/receipts/${encodeURIComponent(resolving.id)}/resolve`,
        {},
        { tenant: orgSlug },
      );
      toast({
        title: "Finding resolved",
        description:
          "The finding is marked resolved and a ReceiptReconResolved event was emitted.",
        variant: "success",
      });
      setResolving(null);
      void load();
    } catch (e) {
      if (e instanceof ApiError && e.status === 404) {
        toast({
          title: "Already resolved",
          description:
            "This finding no longer exists or was already resolved — refreshing the list.",
          variant: "destructive",
        });
        setResolving(null);
        void load();
      } else {
        toast({
          title: "Resolve failed",
          description:
            e instanceof ApiError
              ? e.status === 403
                ? "Resolving findings requires the manage_bookings permission."
                : e.message
              : "Failed to resolve the finding.",
          variant: "destructive",
        });
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <PageHeader
        title="Receipt reconciliation"
        description="Field-captured receipts matched against payment state — payment_receipt captures link to their booking exactly (rung 0); the rest match heuristically. Receipts now carry an OCR-extracted currency: the code is shown next to amounts when known, and amounts stay bare (never converted) when it is absent or mismatched."
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
              <TableHead>Booking</TableHead>
              <TableHead className="text-right">Expected</TableHead>
              <TableHead className="text-right">Captured</TableHead>
              <TableHead className="text-right">Receipt</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Detail</TableHead>
              <TableHead className="pr-5 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {findings.length === 0 ? (
              <TableEmpty colSpan={9}>
                {loading
                  ? "Loading…"
                  : "No reconciliation findings. Findings appear once staff upload receipts via the capture API and a sweep runs."}
              </TableEmpty>
            ) : (
              findings.map((f) => {
                const km = kindMeta(f.kind);
                const cur = findingCurrencies(f);
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
                    <TableCell className="text-sm">
                      {f.booking_id ? (
                        <Link
                          href={`/app/${orgSlug}/bookings/${f.booking_id}`}
                          className="font-mono text-xs text-primary underline-offset-2 hover:underline"
                          title={f.booking_id}
                        >
                          {f.booking_id.slice(0, 8)}…
                        </Link>
                      ) : (
                        "—"
                      )}
                    </TableCell>
                    <TableCell className="text-right text-sm tabular-nums">
                      {formatMinor(f.expected_amount_minor, cur.rail)}
                    </TableCell>
                    <TableCell className="text-right text-sm tabular-nums">
                      {formatMinor(f.captured_amount_minor, cur.rail)}
                    </TableCell>
                    <TableCell className="text-right text-sm tabular-nums">
                      {formatMajor(f.receipt_amount_major, cur.receipt)}
                    </TableCell>
                    <TableCell className="text-sm">
                      {formatDateTime(f.created_at)}
                      {f.resolved_at ? (
                        <span className="block text-xs text-muted-foreground">
                          resolved {formatDateTime(f.resolved_at)}
                        </span>
                      ) : null}
                    </TableCell>
                    <TableCell>
                      <details className="rounded-md border border-border px-3 py-2 text-xs">
                        <summary className="cursor-pointer font-medium text-muted-foreground">
                          Detail JSON
                        </summary>
                        <pre className="mt-2 max-w-md overflow-x-auto rounded bg-muted p-2 font-mono text-[11px] leading-relaxed">
                          {JSON.stringify(detailObject(f.detail), null, 2)}
                        </pre>
                      </details>
                    </TableCell>
                    <TableCell className="pr-5 text-right">
                      {f.resolved_at ? (
                        // Read-only: resolved_at is rendered in the Created
                        // column; resolved findings have no action.
                        <span className="text-xs text-muted-foreground">—</span>
                      ) : (
                        <Button
                          variant="ghost"
                          size="icon"
                          title="Resolve finding"
                          disabled={busy}
                          onClick={() => setResolving(f)}
                        >
                          <CheckCircle2 className="h-4 w-4" />
                        </Button>
                      )}
                    </TableCell>
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </Card>

      <ResolveDialog
        finding={resolving}
        busy={busy}
        onClose={() => setResolving(null)}
        onConfirm={() => void resolve()}
      />
    </div>
  );
}

/**
 * SPEC-W64: resolve confirmation (media-inbox DiscardDialog idiom) — sets
 * resolved_at server-side and emits ReceiptReconResolved; irreversible.
 */
function ResolveDialog({
  finding,
  busy,
  onClose,
  onConfirm,
}: {
  finding: ReconFinding | null;
  busy: boolean;
  onClose: () => void;
  onConfirm: () => void;
}) {
  return (
    <Dialog open={finding !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogContent onClose={onClose} className="max-w-xl">
        <DialogHeader>
          <DialogTitle>Resolve finding</DialogTitle>
        </DialogHeader>
        <p className="mb-4 text-sm text-muted-foreground">
          Mark this{" "}
          {finding ? kindMeta(finding.kind).label.toLowerCase() : ""} finding
          as resolved? It will no longer appear in the open queue, the
          resolution timestamp and your identity are recorded, and a
          ReceiptReconResolved event is emitted. This cannot be undone.
        </p>
        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button onClick={onConfirm} disabled={busy}>
            {busy ? "Resolving…" : "Resolve finding"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
