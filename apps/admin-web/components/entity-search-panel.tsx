"use client";

/**
 * SPEC-W119 §4: shared entity-search panel mounted by the bookings,
 * contacts and leads screens. Fetches booking-service
 * /api/bookings/v1/{entity}/search through the EXISTING bookings BFF
 * wildcard (no new gateway route; X-Tenant-Slug via api.get's tenant).
 *
 * Honest failure posture (SPEC §7, mirrors W117): a 503 renders a loud
 * "temporarily unavailable" banner — the regular list below is unaffected
 * and no degraded substitute with different semantics is ever shown.
 * Results are eventual-consistent (sync interval, default 30s) — the panel
 * is labelled "indexed search" and the empty state says so.
 */

import * as React from "react";
import { api, ApiError } from "@/lib/api";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { EntitySearchBar } from "@/components/entity-search-bar";
import {
  buildEntitySearchParams,
  emptyEntitySearchState,
  nextEntityOffset,
  prevEntityOffset,
  type EntityDescriptor,
  type EntitySearchState,
} from "@/lib/entity-search";

export type EntityHit = Record<string, unknown>;

interface EntitySearchResponse {
  hits: EntityHit[];
  total: number;
}

export function EntitySearchPanel({
  orgSlug,
  descriptor,
  title,
  description,
  columns,
  renderRow,
  hitKey,
}: {
  orgSlug: string;
  descriptor: EntityDescriptor;
  title: string;
  description: string;
  columns: string[];
  /** Renders the TableCell set for one _source doc. */
  renderRow: (hit: EntityHit) => React.ReactNode;
  hitKey: (hit: EntityHit) => string;
}) {
  const [state, setState] = React.useState<EntitySearchState>(emptyEntitySearchState);
  const [result, setResult] = React.useState<EntitySearchResponse | null>(null);
  const [searching, setSearching] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [unavailable, setUnavailable] = React.useState(false);

  const runSearch = React.useCallback(
    async (next: EntitySearchState) => {
      setSearching(true);
      setError(null);
      setUnavailable(false);
      try {
        const params = buildEntitySearchParams(next, descriptor);
        const query: Record<string, string | number> = { tenant: orgSlug };
        params.forEach((value, key) => {
          query[key] = value;
        });
        const resp = await api.get<EntitySearchResponse>(
          `/api/bookings/v1/${descriptor.entity}/search`,
          query,
        );
        setResult(resp);
      } catch (err) {
        if (err instanceof ApiError && err.status === 503) {
          // Fail-closed backend: loud banner, the list below is unaffected.
          setUnavailable(true);
          setResult(null);
        } else {
          setError(err instanceof ApiError ? err.message : "Search failed — please try again.");
        }
      } finally {
        setSearching(false);
      }
    },
    [orgSlug, descriptor],
  );

  const onSearch = React.useCallback(() => {
    const reset = { ...state, offset: 0 };
    setState(reset);
    void runSearch(reset);
  }, [state, runSearch]);

  const gotoOffset = React.useCallback(
    (offset: number) => {
      const next = { ...state, offset };
      setState(next);
      void runSearch(next);
    },
    [state, runSearch],
  );

  const next = result ? nextEntityOffset(state, result.total) : null;
  const prev = prevEntityOffset(state);

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">{title}</CardTitle>
        <CardDescription>{description}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <EntitySearchBar
          descriptor={descriptor}
          state={state}
          onChange={setState}
          onSearch={onSearch}
          searching={searching}
        />
        {unavailable ? (
          <p className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-sm text-amber-900">
            Advanced search is temporarily unavailable. The list below is
            unaffected.
          </p>
        ) : null}
        {error ? (
          <p className="rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive">
            {error}
          </p>
        ) : null}
        {result ? (
          <>
            <p className="text-xs text-muted-foreground">
              {result.total} indexed result{result.total === 1 ? "" : "s"}
              {result.total > 0
                ? ` — showing ${state.offset + 1}–${Math.min(
                    state.offset + result.hits.length,
                    result.total,
                  )}`
                : ""}
            </p>
            <Table>
              <TableHeader>
                <TableRow>
                  {columns.map((col, i) => (
                    <TableHead key={col} className={i === 0 ? "pl-5" : undefined}>
                      {col}
                    </TableHead>
                  ))}
                </TableRow>
              </TableHeader>
              <TableBody>
                {result.hits.length === 0 ? (
                  <TableEmpty colSpan={columns.length}>
                    No indexed results match these filters. Recently created
                    records appear within about 30 seconds.
                  </TableEmpty>
                ) : (
                  result.hits.map((hit) => (
                    <TableRow key={hitKey(hit)}>{renderRow(hit)}</TableRow>
                  ))
                )}
              </TableBody>
            </Table>
            <div className="flex items-center justify-end gap-2">
              <Button
                variant="outline"
                size="sm"
                disabled={prev === null || searching}
                onClick={() => prev !== null && gotoOffset(prev)}
              >
                Previous
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={next === null || searching}
                onClick={() => next !== null && gotoOffset(next)}
              >
                Next
              </Button>
            </div>
          </>
        ) : null}
      </CardContent>
    </Card>
  );
}
