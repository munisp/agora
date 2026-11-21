"use client";

import * as React from "react";
import { cn } from "@/lib/utils";

export function Table({
  className,
  ...props
}: React.TableHTMLAttributes<HTMLTableElement>) {
  return (
    <div className="relative w-full overflow-auto">
      <table
        className={cn("w-full caption-bottom text-sm", className)}
        {...props}
      />
    </div>
  );
}

export function TableHeader({
  className,
  ...props
}: React.HTMLAttributes<HTMLTableSectionElement>) {
  return (
    <thead className={cn("border-b border-border", className)} {...props} />
  );
}

/** Rows shown per page in every dashboard table (2026-11-20). */
export const TABLE_PAGE_SIZE = 10;
const PAGE_SIZE_CHOICES = [10, 25, 50, 100];

/**
 * Table body with built-in client-side pagination (2026-11-20): when it holds
 * more rows than one page, it shows a page of them plus a pager row
 * underneath (range, rows-per-page, previous/next). Each direct child counts
 * as one row, so a fragment holding a row and its detail row stays together.
 * Pass paginate={false} for tables the server already pages.
 */
export function TableBody({
  className,
  children,
  paginate = true,
  pageSize = TABLE_PAGE_SIZE,
  ...props
}: React.HTMLAttributes<HTMLTableSectionElement> & {
  paginate?: boolean;
  pageSize?: number;
}) {
  const [page, setPage] = React.useState(0);
  const [size, setSize] = React.useState(pageSize);
  const rows = React.Children.toArray(children);
  const total = rows.length;
  if (!paginate || total <= Math.min(size, pageSize)) {
    return (
      <tbody className={cn(className)} {...props}>
        {children}
      </tbody>
    );
  }
  const pages = Math.max(1, Math.ceil(total / size));
  const current = Math.min(page, pages - 1);
  const from = current * size;
  const to = Math.min(total, from + size);
  const btn =
    "inline-flex h-8 items-center rounded-md border border-border bg-card px-3 text-xs text-foreground hover:bg-accent disabled:pointer-events-none disabled:opacity-40";
  return (
    <>
      <tbody className={cn(className)} {...props}>
        {rows.slice(from, to)}
      </tbody>
      <tbody data-table-pager="">
        <tr>
          <td colSpan={1000} className="px-3 py-2.5">
            <div className="flex flex-wrap items-center justify-between gap-3 text-xs text-muted-foreground">
              <span>
                Showing {from + 1}–{to} of {total}
              </span>
              <div className="flex flex-wrap items-center gap-3">
                <label className="flex items-center gap-1.5">
                  Rows
                  <select
                    className="h-8 rounded-md border border-border bg-card px-2 text-xs text-foreground"
                    value={size}
                    onChange={(e) => {
                      setSize(Number(e.target.value));
                      setPage(0);
                    }}
                  >
                    {PAGE_SIZE_CHOICES.map((n) => (
                      <option key={n} value={n}>
                        {n}
                      </option>
                    ))}
                  </select>
                </label>
                <button type="button" className={btn} disabled={current === 0} onClick={() => setPage(current - 1)}>
                  Previous
                </button>
                <span>
                  Page {current + 1} of {pages}
                </span>
                <button type="button" className={btn} disabled={current >= pages - 1} onClick={() => setPage(current + 1)}>
                  Next
                </button>
              </div>
            </div>
          </td>
        </tr>
      </tbody>
    </>
  );
}

export function TableRow({
  className,
  ...props
}: React.HTMLAttributes<HTMLTableRowElement>) {
  return (
    <tr
      className={cn(
        "border-b border-border transition-colors hover:bg-muted/60",
        className,
      )}
      {...props}
    />
  );
}

export function TableHead({
  className,
  ...props
}: React.ThHTMLAttributes<HTMLTableCellElement>) {
  return (
    <th
      className={cn(
        "h-10 px-3 text-left align-middle text-xs font-semibold uppercase tracking-wide text-muted-foreground",
        className,
      )}
      {...props}
    />
  );
}

export function TableCell({
  className,
  ...props
}: React.TdHTMLAttributes<HTMLTableCellElement>) {
  return (
    <td className={cn("px-3 py-2.5 align-middle", className)} {...props} />
  );
}

export function TableEmpty({
  colSpan,
  children,
}: {
  colSpan: number;
  children: React.ReactNode;
}) {
  return (
    <TableRow>
      <TableCell
        colSpan={colSpan}
        className="py-10 text-center text-muted-foreground"
      >
        {children}
      </TableCell>
    </TableRow>
  );
}
