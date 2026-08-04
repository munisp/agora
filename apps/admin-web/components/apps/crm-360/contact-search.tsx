"use client";

/**
 * Contact search + result list (SPEC-W20 Agent A): name/phone/email
 * prefix search with an optional tag filter. Pure presentational — the
 * parent client owns the network call and debounce.
 */
import * as React from "react";
import { Search } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Input, Label } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import type { ContactSearchResult } from "./types";

export interface SearchFilters {
  q: string;
  tag: string;
}

export function ContactSearch({
  filters,
  onFiltersChange,
  results,
  loading,
  onOpen,
}: {
  filters: SearchFilters;
  onFiltersChange: (f: SearchFilters) => void;
  results: ContactSearchResult[];
  loading: boolean;
  onOpen: (contact: ContactSearchResult) => void;
}) {
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
        <div className="rounded-md border border-border bg-card">
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
      )}
    </div>
  );
}
