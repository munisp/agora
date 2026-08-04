"use client";

/**
 * CRM-360 contact search client (SPEC-W20 Agent A): debounced prefix
 * search + tag filter over the BFF with the x-tenant-slug header:
 *   - GET /api/bookings/v1/crm/contacts/search?q=&tag=&limit=
 * Row click navigates to the 360 profile page.
 */
import * as React from "react";
import { useRouter } from "next/navigation";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { unwrap } from "@/components/apps/types";
import {
  ContactSearch,
  type SearchFilters,
} from "@/components/apps/crm-360/contact-search";
import type { ContactSearchResult } from "@/components/apps/crm-360/types";

export function Crm360Client({
  orgSlug,
  canWork,
}: {
  orgSlug: string;
  /** owner/admin/staff — may write notes/tags on the profile page */
  canWork: boolean;
}) {
  const router = useRouter();
  const [filters, setFilters] = React.useState<SearchFilters>({ q: "", tag: "" });
  const [results, setResults] = React.useState<ContactSearchResult[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<unknown>("/api/bookings/v1/crm/contacts/search", {
          tenant: orgSlug,
          q: filters.q || undefined,
          tag: filters.tag || undefined,
          limit: 50,
        });
        if (signal?.aborted) return;
        setResults(unwrap<ContactSearchResult>(data));
      } catch (e) {
        if (signal?.aborted) return;
        setResults([]);
        setError(
          e instanceof ApiError && e.status !== 404
            ? e.message
            : "CRM-360 is not available yet — the booking-service CRM API may still be rolling out.",
        );
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [orgSlug, filters],
  );

  // Debounce filter changes into one reload (same idiom as helpdesk).
  React.useEffect(() => {
    const controller = new AbortController();
    const timer = setTimeout(() => void load(controller.signal), 250);
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [load]);

  return (
    <div>
      <PageHeader
        title="CRM 360"
        description="Unified customer profile — search a contact, then see bookings, tickets, work orders, loyalty, notes and tags in one place."
        actions={
          <Button variant="outline" size="sm" onClick={() => void load()} disabled={loading}>
            <RefreshCw className={`mr-1 h-4 w-4 ${loading ? "animate-spin" : ""}`} />
            Refresh
          </Button>
        }
      />

      {error ? <ErrorNote message={error} /> : null}

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
