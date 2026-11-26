"use client";

import * as React from "react";
import { ArrowDown, ArrowUp, ArrowUpDown } from "lucide-react";
import { cn } from "@/lib/utils";
import {
  isWhenHeader,
  mostlyDates,
  sortKeyOf,
  sortedOrder,
  type SortDir,
} from "@/lib/table-sort";

/**
 * Sortable tables (2026-11-26): every column header sorts the rows on click (again to reverse). A table starts with
 * the latest first on its first "when" column (When, Date, Created, Last seen...) when that column holds dates;
 * otherwise rows keep the order they came in. Sort keys come from each cell's text as shown (dates, numbers, text);
 * pass sortValue on a TableCell when the shown text does not sort (e.g. "2 hours ago"), sortable={false} on a
 * TableHead (or on the Table) to opt out. Rows must be TableRow elements (a fragment holding a row and its detail row
 * counts as one row, sorted by that first row) or pass sortValues (see rowValues). Server-paged bodies (paginate={false}) sort the page shown.
 */

type SortState = { col: number; dir: SortDir };

interface SortCtx {
  sort: SortState | null;
  setSort: (s: SortState) => void;
  /** the sort applied when nobody has clicked: the "when" column, latest first */
  auto: SortState | null;
  setAuto: (s: SortState | null) => void;
  whenCol: number | null;
  setWhenCol: (c: number | null) => void;
  rowsSortable: boolean;
  setRowsSortable: (b: boolean) => void;
}

const TableSortContext = React.createContext<SortCtx | null>(null);

export function Table({
  className,
  sortable = true,
  children,
  ...props
}: React.TableHTMLAttributes<HTMLTableElement> & { sortable?: boolean }) {
  const [sort, setSort] = React.useState<SortState | null>(null);
  const [auto, setAutoState] = React.useState<SortState | null>(null);
  const [whenCol, setWhenColState] = React.useState<number | null>(null);
  const [rowsSortable, setRowsSortableState] = React.useState(false);
  const ctx = React.useMemo<SortCtx>(
    () => ({
      sort,
      setSort,
      auto,
      setAuto: (s) =>
        setAutoState((prev) => (prev?.col === s?.col && prev?.dir === s?.dir ? prev : s)),
      whenCol,
      setWhenCol: (c) => setWhenColState((prev) => (prev === c ? prev : c)),
      rowsSortable,
      setRowsSortable: (b) => setRowsSortableState((prev) => (prev === b ? prev : b)),
    }),
    [sort, auto, whenCol, rowsSortable],
  );
  const table = (
    <div className="relative w-full overflow-auto">
      <table className={cn("w-full caption-bottom text-sm", className)} {...props}>
        {children}
      </table>
    </div>
  );
  return sortable ? <TableSortContext.Provider value={ctx}>{table}</TableSortContext.Provider> : table;
}

/** Plain text of a React node, for sort keys and header names. Components without text children fall back to a
 * label/status/value/date-like prop (e.g. <StatusBadge status="confirmed" />). */
function nodeText(node: React.ReactNode): string {
  if (node === null || node === undefined || typeof node === "boolean") return "";
  if (typeof node === "string" || typeof node === "number") return String(node);
  if (Array.isArray(node)) return node.map(nodeText).filter(Boolean).join(" ");
  if (React.isValidElement(node)) {
    const p = node.props as Record<string, unknown>;
    const inner = nodeText(p.children as React.ReactNode);
    if (inner) return inner;
    for (const k of ["label", "status", "value", "title", "iso", "date", "datetime", "dateTime", "timestamp"]) {
      const v = p[k];
      if (typeof v === "string" || typeof v === "number") return String(v);
    }
  }
  return "";
}

/** A row's sort value by column index, or null when the row cannot be read. A TableRow is read from its cells; a
 * custom row component (e.g. <IncidentRow />) passes sortValues={[...]}, one per column. */
function rowValues(row: React.ReactNode): ((col: number) => unknown) | null {
  if (!React.isValidElement(row)) return null;
  const props = row.props as { children?: React.ReactNode; sortValues?: unknown[] };
  if (Array.isArray(props.sortValues)) {
    const values = props.sortValues;
    return (col) => values[col] ?? null;
  }
  if (row.type === React.Fragment) {
    const first = React.Children.toArray(props.children).find(React.isValidElement);
    return first ? rowValues(first) : null;
  }
  if (row.type !== TableRow && row.type !== "tr") return null;
  const cells = new Map<number, React.ReactElement>();
  let i = 0;
  React.Children.forEach(props.children, (cell) => {
    if (!React.isValidElement(cell)) return;
    cells.set(i, cell);
    i += Number((cell.props as { colSpan?: number }).colSpan) || 1;
  });
  return (col) => cellSortValue(cells.get(col));
}

function cellSortValue(cell: React.ReactElement | undefined): unknown {
  if (!cell) return null;
  const p = cell.props as { sortValue?: unknown; children?: React.ReactNode };
  return p.sortValue !== undefined ? p.sortValue : nodeText(p.children);
}

export function TableHeader({
  className,
  children,
  ...props
}: React.HTMLAttributes<HTMLTableSectionElement>) {
  const ctx = React.useContext(TableSortContext);
  let whenCol: number | null = null;
  const indexed = ctx
    ? React.Children.map(children, (row) => {
        if (!React.isValidElement(row)) return row;
        let i = 0;
        const heads = React.Children.map((row.props as { children?: React.ReactNode }).children, (th) => {
          if (!React.isValidElement(th)) return th;
          const idx = i;
          const hp = th.props as { colSpan?: number; sortable?: boolean; children?: React.ReactNode };
          i += Number(hp.colSpan) || 1;
          if (whenCol === null && hp.sortable !== false && isWhenHeader(nodeText(hp.children))) whenCol = idx;
          return React.cloneElement(th as React.ReactElement<{ colIndex?: number }>, { colIndex: idx });
        });
        return React.cloneElement(row, {}, heads);
      })
    : children;
  React.useEffect(() => {
    ctx?.setWhenCol(whenCol);
  });
  return (
    <thead className={cn("border-b border-border", className)} {...props}>
      {indexed}
    </thead>
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
 * Rows are sorted first (2026-11-26, see Table).
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
  const ctx = React.useContext(TableSortContext);
  const [page, setPage] = React.useState(0);
  const [size, setSize] = React.useState(pageSize);
  let rows = React.Children.toArray(children);

  // Sorting: every row must be a TableRow we can read.
  const valuesByRow = ctx ? rows.map(rowValues) : [];
  const sortable = !!ctx && rows.length > 1 && valuesByRow.every((v) => v !== null);
  let auto: SortState | null = null;
  if (sortable && ctx && ctx.whenCol !== null && !ctx.sort) {
    const col = ctx.whenCol;
    const texts = valuesByRow.map((rv) => {
      const v = rv!(col);
      return v instanceof Date ? v.toISOString() : String(v ?? "");
    });
    if (mostlyDates(texts)) auto = { col, dir: "desc" };
  }
  const active = sortable && ctx ? ctx.sort ?? auto : null;
  if (active) {
    const keys = valuesByRow.map((rv) => sortKeyOf(rv!(active.col)));
    const order = sortedOrder(keys, active.dir);
    rows = order.map((i) => rows[i]);
  }
  React.useEffect(() => {
    if (!ctx) return;
    ctx.setRowsSortable(sortable);
    ctx.setAuto(auto);
  });
  const sortSig = active ? `${active.col}:${active.dir}` : "";
  React.useEffect(() => {
    setPage(0);
  }, [sortSig]);

  const total = rows.length;
  if (!paginate || total <= Math.min(size, pageSize)) {
    return (
      <tbody className={cn(className)} {...props}>
        {rows}
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
  children,
  colIndex,
  sortable,
  ...props
}: React.ThHTMLAttributes<HTMLTableCellElement> & {
  /** set by TableHeader */
  colIndex?: number;
  /** false: this column does not sort */
  sortable?: boolean;
}) {
  const ctx = React.useContext(TableSortContext);
  const label = nodeText(children).trim();
  const canSort =
    !!ctx && ctx.rowsSortable && colIndex !== undefined && sortable !== false && label !== "" &&
    !/^actions?$/i.test(label);
  const cls = cn(
    "h-10 px-3 text-left align-middle text-xs font-semibold uppercase tracking-wide text-muted-foreground",
    className,
  );
  if (!canSort || !ctx || colIndex === undefined) {
    return (
      <th className={cls} {...props}>
        {children}
      </th>
    );
  }
  const active = ctx.sort ?? ctx.auto;
  const dir = active && active.col === colIndex ? active.dir : null;
  const Icon = dir === "asc" ? ArrowUp : dir === "desc" ? ArrowDown : ArrowUpDown;
  const right = /\btext-right\b/.test(className ?? "");
  return (
    <th
      className={cls}
      aria-sort={dir === "asc" ? "ascending" : dir === "desc" ? "descending" : "none"}
      {...props}
    >
      <button
        type="button"
        onClick={() => ctx.setSort({ col: colIndex, dir: dir === "asc" ? "desc" : dir === "desc" ? "asc" : "asc" })}
        className={cn(
          "inline-flex items-center gap-1 uppercase tracking-wide hover:text-foreground cursor-pointer",
          right && "flex-row-reverse",
          dir && "text-foreground",
        )}
        title={dir === "desc" ? "Sorted, latest / highest first — click to reverse" : dir === "asc" ? "Sorted, earliest / lowest first — click to reverse" : `Sort by ${label.toLowerCase()}`}
      >
        {children}
        <Icon className={cn("h-3 w-3 shrink-0", !dir && "opacity-40")} aria-hidden />
      </button>
    </th>
  );
}

export function TableCell({
  className,
  sortValue: _sortValue,
  ...props
}: React.TdHTMLAttributes<HTMLTableCellElement> & {
  /** what this cell sorts by when its shown text does not (a date behind "2 hours ago", a raw amount...) */
  sortValue?: string | number | Date | null;
}) {
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
