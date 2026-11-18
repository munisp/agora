"use client";

/**
 * CRM-360 contact search client (SPEC-W20 Agent A; SPEC-W82 B1.3/B1.4):
 * debounced prefix search + tag filter over the BFF with the x-tenant-slug
 * header:
 *   - GET /api/bookings/v1/crm/contacts/search?q=&tag=&limit=
 * Row click navigates to the 360 profile page.
 *
 * W82 additions:
 *  - B1.4: q/tag are mirrored into the URL (router.replace) so filtered
 *    searches are bookmarkable and survive refresh/back; initial filters
 *    and matching results arrive from the server component.
 *  - B1.3: every successful search is written to the per-browser
 *    localStorage last-good cache (keyed by tenant + query); on network
 *    failure the cached rows render with a "Cached as of HH:MM —
 *    reconnecting…" warning Badge, and the browser `online` event
 *    triggers a refresh.
 */
import * as React from "react";
import { useRouter } from "next/navigation";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { unwrap } from "@/components/apps/types";
import {
  cacheKey,
  formatCachedAt,
  readCache,
  writeCache,
} from "@/lib/local-cache";
import {
  ContactSearch,
  type SearchFilters,
} from "@/components/apps/crm-360/contact-search";
import type { ContactSearchResult } from "@/components/apps/crm-360/types";

export function Crm360Client({
  orgSlug,
  canWork,
  initialFilters,
  initialResults,
  initialError,
}: {
  orgSlug: string;
  /** owner/admin/staff — may write notes/tags on the profile page */
  canWork: boolean;
  /** SPEC-W82 B1.4: q/tag read from the URL by the server component. */
  initialFilters: SearchFilters;
  /** SPEC-W46 AW-2: search matching initialFilters fetched server-side. */
  initialResults: ContactSearchResult[];
  initialError: string | null;
}) {
  const router = useRouter();
  const [filters, setFilters] = React.useState<SearchFilters>(initialFilters);
  const [results, setResults] =
    React.useState<ContactSearchResult[]>(initialResults);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState<string | null>(initialError);
  /** savedAt of the cached rows being shown, if any (B1.3). */
  const [cachedAt, setCachedAt] = React.useState<number | null>(null);

  const filtersKey = (f: SearchFilters) =>
    `${f.q.trim().toLowerCase()}|${f.tag.trim().toLowerCase()}`;
  const searchCacheKey = (f: SearchFilters) =>
    cacheKey(orgSlug, "crm-search", filtersKey(f));

  // Sync when the server props change (router.refresh() / back-forward).
  React.useEffect(() => {
    setResults(initialResults);
    setError(initialError);
    setCachedAt(null);
  }, [initialResults, initialError]);

  // Keep the filter state in step with the URL (back/forward navigation).
  const initialFiltersJson = JSON.stringify(initialFilters);
  React.useEffect(() => {
    setFilters((current) =>
      JSON.stringify(current) === initialFiltersJson
        ? current
        : (JSON.parse(initialFiltersJson) as SearchFilters),
    );
  }, [initialFiltersJson]);

  const load = React.useCallback(
    async (f: SearchFilters, signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<unknown>("/api/bookings/v1/crm/contacts/search", {
          tenant: orgSlug,
          q: f.q || undefined,
          tag: f.tag || undefined,
          limit: 50,
        });
        if (signal?.aborted) return;
        const rows = unwrap<ContactSearchResult>(data);
        setResults(rows);
        setCachedAt(null);
        // B1.3: remember the last good answer for this tenant + filter set.
        writeCache(searchCacheKey(f), rows);
      } catch (e) {
        if (signal?.aborted) return;
        // B1.3: network failure / offline — fall back to the last-good
        // cached rows for exactly this tenant + query and flag staleness.
        const cached = readCache<ContactSearchResult[]>(searchCacheKey(f));
        if (cached) {
          setResults(cached.data);
          setCachedAt(cached.savedAt);
        } else {
          setResults([]);
          setCachedAt(null);
        }
        setError(
          e instanceof ApiError && e.status !== 404
            ? e.message
            : "CRM-360 is not available yet — the booking-service CRM API may still be rolling out.",
        );
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [orgSlug],
  );

  // Debounce filter changes into one reload + URL sync (same debounce idiom
  // as helpdesk). The initial (server-rendered) results match the initial
  // filters, so the first run is skipped.
  const skipInitialLoad = React.useRef(true);
  React.useEffect(() => {
    if (skipInitialLoad.current) {
      skipInitialLoad.current = false;
      return;
    }
    const controller = new AbortController();
    const timer = setTimeout(() => {
      // B1.4: mirror the filters into the URL (replace, not push — typing
      // should not flood history; back/forward still works across loads).
      const params = new URLSearchParams();
      if (filters.q.trim()) params.set("q", filters.q.trim());
      if (filters.tag.trim()) params.set("tag", filters.tag.trim());
      const qs = params.toString();
      router.replace(
        `/app/${orgSlug}/apps/crm-360${qs ? `?${qs}` : ""}`,
        { scroll: false },
      );
      void load(filters, controller.signal);
    }, 250);
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [filters, load, orgSlug, router]);

  // B1.3: reconnect → refresh the current view automatically.
  React.useEffect(() => {
    const onOnline = () => void load(filters);
    window.addEventListener("online", onOnline);
    return () => window.removeEventListener("online", onOnline);
  }, [load, filters]);

  return (
    <div>
      <PageHeader
        title="CRM 360"
        description="Unified customer profile — search a contact, then see bookings, tickets, work orders, loyalty, notes and tags in one place."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => void load(filters)}
            disabled={loading}
          >
            <RefreshCw className={`mr-1 h-4 w-4 ${loading ? "animate-spin" : ""}`} />
            Refresh
          </Button>
        }
      />

      {error ? <ErrorNote message={error} /> : null}

      {cachedAt !== null ? (
        <div className="mb-3">
          <Badge variant="warning">
            Cached as of {formatCachedAt(cachedAt)} — reconnecting…
          </Badge>
        </div>
      ) : null}

      <ContactSearch
        filters={filters}
        onFiltersChange={setFilters}
        results={results}
        loading={loading}
        onOpen={(c) =>
          canWork
            ? router.push(`/app/${orgSlug}/apps/crm-360/contacts/${c.id}`)
            : undefined
        }
      />
    </div>
  );
}
