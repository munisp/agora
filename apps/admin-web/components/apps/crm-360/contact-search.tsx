"use client";

/**
 * Contact search + result list (SPEC-W20 Agent A): name/phone/email
 * prefix search with an optional tag filter. Pure presentational — the
 * parent client owns the network call and debounce.
 *
 * SPEC-W82 additions:
 *  - B1.2: below sm: the results table switches to stacked cards.
 *  - B1.5: toolbar with CSV export of the current result set and a
 *    WhatsApp share of a customer summary, both built from the
 *    already-fetched rows (no endpoint involved).
 */
import * as React from "react";
import { Download, Search, Share2 } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input, Label } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { downloadCSV, toCSV, waShareUrl, type CsvColumn } from "@/lib/export";
import type { ContactSearchResult } from "./types";

export interface SearchFilters {
  q: string;
  tag: string;
}

const CSV_COLUMNS: CsvColumn<ContactSearchResult>[] = [
  { header: "Name", value: (c) => c.name },
  { header: "Phone", value: (c) => c.phone },
  { header: "Email", value: (c) => c.email },
  { header: "Tags", value: (c) => c.tags.join(" ") },
  { header: "Source", value: (c) => c.source },
];

export function ContactSearch({
  filters,
  onFiltersChange,
  results,
  loading,
  hasMore = false,
  loadingMore = false,
  onLoadMore,
  onOpen,
}: {
  filters: SearchFilters;
  onFiltersChange: (f: SearchFilters) => void;
  results: ContactSearchResult[];
  loading: boolean;
  /** SPEC-W83: another keyset page exists on the backend. */
  hasMore?: boolean;
  loadingMore?: boolean;
  onLoadMore?: () => void;
  onOpen: (contact: ContactSearchResult) => void;
}) {
  // B1.5: CSV export of exactly what is on screen.
  const exportCsv = () => {
    downloadCSV(
      `crm-360-contacts-${new Date().toISOString().slice(0, 10)}.csv`,
      toCSV(results, CSV_COLUMNS),
    );
  };

  // B1.5: WhatsApp share of a compact customer summary (top rows, so the
  // message stays readable on a phone).
  const shareWhatsApp = () => {
    const lines = results.slice(0, 10).map((c) => {
      const contact = [c.phone, c.email].filter(Boolean).join(" · ");
      const tags = c.tags.length ? ` [${c.tags.join(", ")}]` : "";
      return `• ${c.name}${contact ? ` — ${contact}` : ""}${tags}`;
    });
    const summary =
      `CRM 360 customer summary (${results.length} contact${results.length === 1 ? "" : "s"})` +
      (filters.q.trim() ? ` — search “${filters.q.trim()}”` : "") +
      (filters.tag.trim() ? ` — tag “${filters.tag.trim()}”` : "") +
      `\n${lines.join("\n")}` +
      (results.length > lines.length
        ? `\n…and ${results.length - lines.length} more`
        : "");
    window.open(waShareUrl(summary), "_blank", "noreferrer,noopener");
  };

  return (
    <div className="space-y-3">
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_220px]">
        <div className="space-y-1">
          <Label htmlFor="crm-q">Search contacts</Label>
          <div className="relative">
            <Search className="pointer-events-none absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
            <Input
              id="crm-q"
              value={filters.q}
              onChange={(e) => onFiltersChange({ ...filters, q: e.target.value })}
              placeholder="Name, phone or email prefix…"
              className="pl-8"
            />
          </div>
        </div>
        <div className="space-y-1">
          <Label htmlFor="crm-tag">Tag filter</Label>
          <Input
            id="crm-tag"
            value={filters.tag}
            onChange={(e) => onFiltersChange({ ...filters, tag: e.target.value })}
            placeholder="e.g. vip"
            maxLength={40}
          />
        </div>
      </div>

      {/* B1.5 toolbar: export/share the rows already on screen. */}
      <div className="flex flex-wrap items-center gap-2">
        <Button
          variant="outline"
          size="sm"
          onClick={exportCsv}
          disabled={results.length === 0}
        >
          <Download className="h-3.5 w-3.5" />
          Export CSV
        </Button>
        <Button
          variant="outline"
          size="sm"
          onClick={shareWhatsApp}
          disabled={results.length === 0}
        >
          <Share2 className="h-3.5 w-3.5" />
          Share via WhatsApp
        </Button>
      </div>

      {loading && results.length === 0 ? (
        <div className="space-y-2">
          {Array.from({ length: 3 }).map((_, i) => (
            <div key={i} className="h-12 animate-pulse rounded-md border border-border bg-muted" />
          ))}
        </div>
      ) : results.length === 0 ? (
        <p className="rounded-md border border-border bg-card px-4 py-6 text-sm text-muted-foreground">
          No contacts match — widen the search, or remove the tag filter.
        </p>
      ) : (
        <>
          {/* B1.2: table on ≥sm, stacked cards below. */}
          <div className="hidden rounded-md border border-border bg-card sm:block">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Phone</TableHead>
                  <TableHead>Email</TableHead>
                  <TableHead>Tags</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {results.map((c) => (
                  <TableRow
                    key={c.id}
                    className="cursor-pointer"
                    onClick={() => onOpen(c)}
                  >
                    <TableCell className="font-medium text-foreground">{c.name}</TableCell>
                    <TableCell>{c.phone || "—"}</TableCell>
                    <TableCell>{c.email || "—"}</TableCell>
                    <TableCell>
                      <span className="flex flex-wrap gap-1">
                        {c.tags.map((t) => (
                          <Badge key={t} variant="secondary">
                            {t}
                          </Badge>
                        ))}
                      </span>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>

          <div className="space-y-2 sm:hidden">
            {results.map((c) => (
              <button
                key={c.id}
                type="button"
                onClick={() => onOpen(c)}
                className="w-full rounded-md border border-border bg-card px-4 py-3 text-left cursor-pointer"
              >
                <p className="font-medium text-foreground">{c.name}</p>
                <p className="mt-0.5 text-xs text-muted-foreground">
                  {[c.phone, c.email].filter(Boolean).join(" · ") ||
                    "No phone or email"}
                </p>
                {c.tags.length ? (
                  <span className="mt-2 flex flex-wrap gap-1">
                    {c.tags.map((t) => (
                      <Badge key={t} variant="secondary">
                        {t}
                      </Badge>
                    ))}
                  </span>
                ) : null}
              </button>
            ))}
          </div>

          {/* SPEC-W83: keyset Load more — appended pages dedupe by id in
              the parent; the count is announced for screen readers. */}
          {hasMore ? (
            <div className="flex flex-col items-center gap-1 pt-1" aria-live="polite">
              <Button
                variant="outline"
                size="sm"
                onClick={onLoadMore}
                disabled={loadingMore || !onLoadMore}
              >
                {loadingMore ? "Loading…" : "Load more"}
              </Button>
              <p className="text-xs text-muted-foreground">
                Showing {results.length} contact{results.length === 1 ? "" : "s"}
              </p>
            </div>
          ) : null}
        </>
      )}
    </div>
  );
}
