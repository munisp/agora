"use client";

/**
 * Today view (SPEC-W19 Agent B): today's scheduled work orders (tenant-local
 * day, computed server-side in the tenant timezone), optionally filtered to
 * one assignee. Each row opens the detail panel.
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
import { formatDateTime } from "@/lib/utils";
import {
  STATUS_LABEL,
  statusVariant,
  type BoardItem,
} from "./types";

export function TodayView({
  rows,
  loading,
  dayStart,
  onSelect,
}: {
  rows: BoardItem[];
  loading: boolean;
  /** tenant-local day start returned by the API (informational) */
  dayStart: string | null;
  onSelect: (item: BoardItem) => void;
}) {
  return (
    <div className="space-y-2">
      {dayStart ? (
        <p className="text-xs text-muted-foreground">
          Tenant day starts {formatDateTime(dayStart)} — orders scheduled
          between then and midnight tonight.
        </p>
      ) : null}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Scheduled</TableHead>
            <TableHead>Title</TableHead>
            <TableHead>Assignee</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Checklist</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((item) => (
            <TableRow
              key={item.id}
              className="cursor-pointer"
              onClick={() => onSelect(item)}
            >
              <TableCell className="whitespace-nowrap">
                {item.scheduled_start ? formatDateTime(item.scheduled_start) : "—"}
              </TableCell>
              <TableCell className="font-medium">{item.title}</TableCell>
              <TableCell>{item.assignee_name || "Unassigned"}</TableCell>
              <TableCell>
                <Badge variant={statusVariant(item.status)}>
                  {STATUS_LABEL[item.status]}
                </Badge>
              </TableCell>
              <TableCell>
                {item.checklist.length > 0
                  ? `${item.checklist.filter((c) => c.done).length}/${item.checklist.length}`
                  : "—"}
              </TableCell>
            </TableRow>
          ))}
          {!loading && rows.length === 0 ? (
            <TableEmpty colSpan={5}>
              Nothing scheduled for today. Orders with a scheduled start inside
              the tenant-local day appear here.
            </TableEmpty>
          ) : null}
          {loading && rows.length === 0 ? (
            <TableEmpty colSpan={5}>Loading today&apos;s orders…</TableEmpty>
          ) : null}
        </TableBody>
      </Table>
    </div>
  );
}
