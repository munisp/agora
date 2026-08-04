"use client";

/**
 * Utilization table (SPEC-W20 Agent D): per agent — scheduled hours,
 * clocked hours and utilization % over a from/to range. Entries still
 * clocked in are counted to now and flagged; agents with no scheduled
 * hours show an honest "—" instead of a fake 0%.
 */
import * as React from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input, Label } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { UtilizationRow } from "./types";

function pctVariant(pct: number): "success" | "warning" | "destructive" {
  if (pct >= 70) return "success";
  if (pct >= 40) return "warning";
  return "destructive";
}

export function UtilizationTable({
  rows,
  loading,
  from,
  to,
  onRangeChange,
}: {
  rows: UtilizationRow[] | null;
  loading: boolean;
  from: string; // YYYY-MM-DD
  to: string; // YYYY-MM-DD (exclusive)
  onRangeChange: (from: string, to: string) => void;
}) {
  const [draftFrom, setDraftFrom] = React.useState(from);
  const [draftTo, setDraftTo] = React.useState(to);
  React.useEffect(() => {
    setDraftFrom(from);
    setDraftTo(to);
  }, [from, to]);

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-end gap-3">
        <div>
          <Label htmlFor="wf-util-from">From</Label>
          <Input
            id="wf-util-from"
            type="date"
            value={draftFrom}
            onChange={(e) => setDraftFrom(e.target.value)}
          />
        </div>
        <div>
          <Label htmlFor="wf-util-to">To (exclusive)</Label>
          <Input
            id="wf-util-to"
            type="date"
            value={draftTo}
            onChange={(e) => setDraftTo(e.target.value)}
          />
        </div>
        <Button
          size="sm"
          variant="secondary"
          onClick={() => onRangeChange(draftFrom, draftTo)}
          disabled={loading || !draftFrom || !draftTo}
        >
          Apply range
        </Button>
      </div>

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Agent</TableHead>
            <TableHead>Scheduled hours</TableHead>
            <TableHead>Clocked hours</TableHead>
            <TableHead>Utilization</TableHead>
            <TableHead>Open entries</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {loading && !rows ? (
            <TableEmpty colSpan={5}>Loading utilization…</TableEmpty>
          ) : !rows || rows.length === 0 ? (
            <TableEmpty colSpan={5}>
              No scheduled shifts or time entries in this range yet.
            </TableEmpty>
          ) : (
            rows.map((r) => (
              <TableRow key={r.agent_id}>
                <TableCell className="font-medium">{r.agent_name}</TableCell>
                <TableCell>{r.scheduled_hours.toFixed(1)}</TableCell>
                <TableCell>{r.clocked_hours.toFixed(1)}</TableCell>
                <TableCell>
                  {r.utilization_pct === null ? (
                    <span className="text-muted-foreground">—</span>
                  ) : (
                    <Badge variant={pctVariant(r.utilization_pct)}>
                      {r.utilization_pct.toFixed(0)}%
                    </Badge>
                  )}
                </TableCell>
                <TableCell>
                  {r.open_entries > 0 ? (
                    <Badge variant="warning">
                      {r.open_entries} open (counting to now)
                    </Badge>
                  ) : (
                    <span className="text-muted-foreground">0</span>
                  )}
                </TableCell>
              </TableRow>
            ))
          )}
        </TableBody>
      </Table>
    </div>
  );
}
