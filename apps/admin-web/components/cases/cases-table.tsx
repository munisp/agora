"use client";

/**
 * SPEC-W32 WS-C: civic cases triage table.
 *
 * Columns: select (bulk assign), ref, category, ward, reporter (masked per
 * role — SPEC §4 gate 4), status, SLA countdown chips (ack + resolve, sage
 * → amber → terracotta ramp, never red/blue), opened timestamp.
 */
import * as React from "react";
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
import { cn, formatDateTime } from "@/lib/utils";
import {
  SLA_TONE_META,
  reporterDisplay,
  slaCountdown,
  statusLabel,
  type CivicCase,
  type CivicCategory,
  type SlaTone,
} from "./types";

function SlaChip({
  label,
  countdown,
}: {
  label: string;
  countdown: { tone: SlaTone; label: string } | null;
}) {
  if (!countdown) return null;
  const meta = SLA_TONE_META[countdown.tone];
  return (
    <span
      className="inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] font-medium"
      style={{ color: meta.fg, backgroundColor: meta.bg, borderColor: meta.border }}
      title={`${label}: ${countdown.label}`}
    >
      {label} · {countdown.label}
    </span>
  );
}

export function CasesTable({
  cases,
  categories,
  loading,
  unavailable,
  canRevealReporter,
  selectedIds,
  onToggleSelect,
  onToggleSelectAll,
  onOpen,
}: {
  cases: CivicCase[];
  categories: CivicCategory[];
  loading: boolean;
  /** true when the civic module 404s — the parent renders the empty state. */
  unavailable: boolean;
  canRevealReporter: boolean;
  selectedIds: Set<string>;
  onToggleSelect: (id: string, on: boolean) => void;
  onToggleSelectAll: (on: boolean) => void;
  onOpen: (c: CivicCase) => void;
}) {
  // Tick once a minute so countdown chips stay fresh without a refetch.
  const [, setTick] = React.useState(0);
  React.useEffect(() => {
    const t = setInterval(() => setTick((n) => n + 1), 60_000);
    return () => clearInterval(t);
  }, []);

  const categoryName = (c: CivicCase): string => {
    if (c.category_name) return c.category_name;
    const found = categories.find(
      (k) => k.id === c.category_id || k.slug === c.category_slug,
    );
    return found?.name ?? c.category_slug ?? "—";
  };

  const allSelected = cases.length > 0 && cases.every((c) => selectedIds.has(c.id));

  return (
    <div className="rounded-md border border-border bg-card">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="w-8">
              <input
                type="checkbox"
                aria-label="Select all cases"
                className="cursor-pointer accent-[#7c5b3e]"
                checked={allSelected}
                onChange={(e) => onToggleSelectAll(e.target.checked)}
              />
            </TableHead>
            <TableHead>Ref</TableHead>
            <TableHead>Category</TableHead>
            <TableHead>Ward</TableHead>
            <TableHead>Reporter</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>SLA</TableHead>
            <TableHead>Opened</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {cases.map((c) => {
            const reporter = reporterDisplay(c, canRevealReporter);
            const ack = slaCountdown(c.ack_due_at, c.sla_breach_ack, c.acked_at);
            const res = slaCountdown(
              c.resolve_due_at,
              c.sla_breach_resolve,
              c.resolved_at,
            );
            const merged = c.merged_into !== null;
            return (
              <TableRow
                key={c.id || c.ref}
                className="cursor-pointer"
                onClick={() => onOpen(c)}
              >
                <TableCell onClick={(e) => e.stopPropagation()}>
                  <input
                    type="checkbox"
                    aria-label={`Select case ${c.ref}`}
                    className="cursor-pointer accent-[#7c5b3e]"
                    checked={selectedIds.has(c.id)}
                    onChange={(e) => onToggleSelect(c.id, e.target.checked)}
                  />
                </TableCell>
                <TableCell>
                  <span className="font-mono text-xs font-medium">{c.ref}</span>
                  {merged ? (
                    <span className="ml-1 text-[11px] text-muted-foreground">
                      → {c.merged_into}
                    </span>
                  ) : null}
                </TableCell>
                <TableCell>{categoryName(c)}</TableCell>
                <TableCell>{c.ward ?? "—"}</TableCell>
                <TableCell>
                  <div className="text-xs">
                    <div>{reporter.name}</div>
                    <div className="font-mono text-muted-foreground">
                      {reporter.phone}
                    </div>
                  </div>
                </TableCell>
                <TableCell>
                  <Badge
                    variant={
                      c.status === "resolved" || c.status === "closed"
                        ? "success"
                        : c.status === "new"
                          ? "warning"
                          : "secondary"
                    }
                  >
                    {statusLabel(c.status)}
                  </Badge>
                </TableCell>
                <TableCell>
                  <div className={cn("flex flex-wrap gap-1")}>
                    <SlaChip label="Ack" countdown={ack} />
                    <SlaChip label="Fix" countdown={res} />
                    {!ack && !res ? (
                      <span className="text-xs text-muted-foreground">—</span>
                    ) : null}
                  </div>
                </TableCell>
                <TableCell className="text-xs text-muted-foreground">
                  {c.created_at ? formatDateTime(c.created_at) : "—"}
                </TableCell>
              </TableRow>
            );
          })}
          {cases.length === 0 && !loading && !unavailable ? (
            <TableEmpty colSpan={8}>
              No cases match these filters. Citizen reports from the public
              portal will appear here.
            </TableEmpty>
          ) : null}
          {cases.length === 0 && !loading && unavailable ? (
            <TableEmpty colSpan={8}>
              Civic reporting is not enabled on this workspace yet.
            </TableEmpty>
          ) : null}
          {loading ? (
            <TableEmpty colSpan={8}>Loading cases…</TableEmpty>
          ) : null}
        </TableBody>
      </Table>
    </div>
  );
}
