"use client";

/**
 * SPEC-W117: conversations advanced search (OpenSearch `conversations`
 * turn index) via the EXISTING /api/conversations/* APISIX OIDC wildcard
 * (no new gateway route; X-Tenant-Slugs injection is server-side).
 *
 *   GET /api/conversations/v1/conversations/search
 *     ?q=&channel=&role=&from_ts=&to_ts=&limit=&offset=
 *
 * Honest failure posture (SPEC §7): a 503 (search_unavailable) is shown
 * as a loud note — the screen NEVER silently substitutes a degraded
 * list with different semantics.
 */
import * as React from "react";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
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
import { AdvancedSearchBar } from "@/components/advanced-search-bar";
import {
  EMPTY_SEARCH_STATE,
  nextOffset,
  prevOffset,
  type SearchState,
} from "@/lib/advanced-search";
import { buildSearchParams } from "@/lib/advanced-search";
import { formatDateTime } from "@/lib/utils";

interface SearchHit {
  conversation_id: string;
  site_slug: string | null;
  channel: string | null;
  role: string | null;
  text: string | null;
  ts: string | null;
}

interface SearchResponse {
  hits: SearchHit[];
  total: number;
  limit: number;
  offset: number;
}

const CHANNEL_TONES: Record<string, string> = {
  whatsapp: "bg-emerald-100 text-emerald-800",
  telegram: "bg-sky-100 text-sky-800",
  web: "bg-muted text-muted-foreground",
  voice: "bg-violet-100 text-violet-800",
  ussd: "bg-amber-100 text-amber-800",
};

export function ConversationsClient({ orgSlug }: { orgSlug: string }) {
  const [state, setState] = React.useState<SearchState>(EMPTY_SEARCH_STATE);
  const [result, setResult] = React.useState<SearchResponse | null>(null);
  const [searching, setSearching] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [unavailable, setUnavailable] = React.useState(false);

  const runSearch = React.useCallback(
    async (next: SearchState) => {
      setSearching(true);
      setError(null);
      setUnavailable(false);
      try {
        const params = buildSearchParams(next);
        const query: Record<string, string> = {};
        params.forEach((value, key) => {
          query[key] = value;
        });
        const resp = await api.get<SearchResponse>(
          "/api/conversations/v1/conversations/search",
          query,
        );
        setResult(resp);
      } catch (err) {
        if (err instanceof ApiError && err.status === 503) {
          // SPEC §7: loud, honest — no silent degraded substitute list.
          setUnavailable(true);
          setResult(null);
        } else {
          setError(
            err instanceof ApiError
              ? err.message
              : "Search failed — please try again.",
          );
        }
      } finally {
        setSearching(false);
      }
    },
    [],
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

  const next = result ? nextOffset(state, result.total) : null;
  const prev = prevOffset(state);

  return (
    <div className="space-y-4">
      <PageHeader
        title="Conversations"
        description="Advanced search across every channel's transcripts — full text plus channel, role and date filters."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => void runSearch(state)}
            disabled={searching}
          >
            <RefreshCw className="mr-1 h-4 w-4" />
            Refresh
          </Button>
        }
      />
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Advanced search</CardTitle>
          <CardDescription>
            Searches the OpenSearch transcripts index (turn level). Results
            are newest first.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <AdvancedSearchBar
            state={state}
            onChange={setState}
            onSearch={onSearch}
            searching={searching}
            placeholder="Search message text…"
          />
          {unavailable ? (
            <p className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-sm text-amber-900">
              Search is temporarily unavailable (the search index did not
              answer). No substitute list is shown — results here only ever
              come from the search index.
            </p>
          ) : null}
          {error ? <ErrorNote message={error} /> : null}
          {result ? (
            <>
              <p className="text-xs text-muted-foreground">
                {result.total} matching turn{result.total === 1 ? "" : "s"}
                {result.total > 0
                  ? ` — showing ${result.offset + 1}–${Math.min(
                      result.offset + result.hits.length,
                      result.total,
                    )}`
                  : ""}
              </p>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>When</TableHead>
                    <TableHead>Channel</TableHead>
                    <TableHead>Role</TableHead>
                    <TableHead className="w-[45%]">Message</TableHead>
                    <TableHead>Conversation</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {result.hits.length === 0 ? (
                    <TableEmpty colSpan={5}>
                      No turns match these filters.
                    </TableEmpty>
                  ) : (
                    result.hits.map((hit, i) => (
                      <TableRow key={`${hit.conversation_id}-${i}`}>
                        <TableCell className="whitespace-nowrap text-xs">
                          {hit.ts ? formatDateTime(hit.ts) : "—"}
                        </TableCell>
                        <TableCell>
                          {hit.channel ? (
                            <span
                              className={`inline-block rounded-full px-2 py-0.5 text-[10px] font-semibold ${
                                CHANNEL_TONES[hit.channel] ??
                                "bg-muted text-muted-foreground"
                              }`}
                            >
                              {hit.channel}
                            </span>
                          ) : (
                            "—"
                          )}
                        </TableCell>
                        <TableCell className="text-xs">{hit.role ?? "—"}</TableCell>
                        <TableCell className="text-sm">
                          <span className="line-clamp-2">{hit.text ?? ""}</span>
                        </TableCell>
                        <TableCell>
                          <Badge variant="outline" className="font-mono text-[10px]">
                            {hit.conversation_id.slice(0, 8)}…
                          </Badge>
                        </TableCell>
                      </TableRow>
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
    </div>
  );
}
