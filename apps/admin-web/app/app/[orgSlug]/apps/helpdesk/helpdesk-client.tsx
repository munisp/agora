"use client";

/**
 * Helpdesk app client (SPEC-W19 Agent A): stats tiles + queue board +
 * ticket detail drawer + SLA policy editor, all through the BFF with the
 * x-tenant-slug header attached:
 *   - GET  /api/bookings/v1/helpdesk/tickets        (filters via query)
 *   - GET  /api/bookings/v1/helpdesk/stats
 *   - GET  /api/bookings/v1/helpdesk/sla-policies
 *   - GET  /api/bookings/v1/helpdesk/team-members   (assignee picker;
 *     soft-fails — the board still works without names)
 */
import * as React from "react";
import { RefreshCw } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Button } from "@/components/ui/button";
import { unwrap } from "@/components/apps/types";
import { StatsTiles } from "@/components/apps/helpdesk/stats-tiles";
import { QueueBoard, type QueueFilters } from "@/components/apps/helpdesk/queue-board";
import { TicketDrawer } from "@/components/apps/helpdesk/ticket-drawer";
import { SLAPolicyEditor } from "@/components/apps/helpdesk/sla-policy-editor";
import type {
  HelpdeskStats,
  SLAPolicy,
  TeamMember,
  Ticket,
} from "@/components/apps/helpdesk/types";

type Tab = "queue" | "policies";

export function HelpdeskClient({
  orgSlug,
  canWork,
  canManage,
}: {
  orgSlug: string;
  /** owner/admin/staff — may assign, change status, add notes */
  canWork: boolean;
  /** owner/admin — may edit SLA policies */
  canManage: boolean;
}) {
  const [tab, setTab] = React.useState<Tab>("queue");
  const [tickets, setTickets] = React.useState<Ticket[]>([]);
  const [stats, setStats] = React.useState<HelpdeskStats | null>(null);
  const [policies, setPolicies] = React.useState<SLAPolicy[]>([]);
  const [members, setMembers] = React.useState<TeamMember[]>([]);
  const [filters, setFilters] = React.useState<QueueFilters>({
    q: "",
    priority: "",
    assigneeId: "",
    channel: "",
  });
  const [loading, setLoading] = React.useState(true);
  const [error, setError] = React.useState<string | null>(null);
  const [selected, setSelected] = React.useState<Ticket | null>(null);
  const [drawerOpen, setDrawerOpen] = React.useState(false);

  const load = React.useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const [ticketData, statsData] = await Promise.all([
          api.get<unknown>("/api/bookings/v1/helpdesk/tickets", {
            tenant: orgSlug,
            q: filters.q || undefined,
            priority: filters.priority || undefined,
            assignee_id: filters.assigneeId || undefined,
            channel: filters.channel || undefined,
          }),
          api.get<{ stats: HelpdeskStats }>("/api/bookings/v1/helpdesk/stats", {
            tenant: orgSlug,
          }),
        ]);
        if (signal?.aborted) return;
        setTickets(unwrap<Ticket>(ticketData));
        setStats(statsData.stats ?? null);
      } catch (e) {
        if (signal?.aborted) return;
        setTickets([]);
        setError(
          e instanceof ApiError && e.status !== 404
            ? e.message
            : "Helpdesk is not available yet — the booking-service helpdesk API may still be rolling out.",
        );
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [orgSlug, filters],
  );

  // Debounce filter changes (search box) into one reload.
  React.useEffect(() => {
    const controller = new AbortController();
    const timer = setTimeout(() => void load(controller.signal), 250);
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [load]);

  // Policies + team members: independent reads; members soft-fail (the board
  // shows "Former member"/raw ids without them, like the CAC page's optional
  // reads).
  const loadPolicies = React.useCallback(
    async (signal?: AbortSignal) => {
      try {
        const data = await api.get<unknown>("/api/bookings/v1/helpdesk/sla-policies", {
          tenant: orgSlug,
        });
        if (!signal?.aborted) setPolicies(unwrap<SLAPolicy>(data));
      } catch {
        if (!signal?.aborted) setPolicies([]);
      }
    },
    [orgSlug],
  );

  React.useEffect(() => {
    const controller = new AbortController();
    void loadPolicies(controller.signal);
    return () => controller.abort();
  }, [loadPolicies]);

  React.useEffect(() => {
    (async () => {
      try {
        const data = await api.get<unknown>("/api/bookings/v1/helpdesk/team-members", {
          tenant: orgSlug,
        });
        setMembers(unwrap<TeamMember>(data));
      } catch {
        setMembers([]);
      }
    })();
  }, [orgSlug]);

  const refresh = () => {
    void load();
    void loadPolicies();
  };

  const openTicket = (t: Ticket) => {
    setSelected(t);
    setDrawerOpen(true);
  };

  return (
    <div>
      <PageHeader
        title="Helpdesk"
        description="SLA ticketing queue — assignment, first-response and resolve clocks, CSAT."
        actions={
          <Button variant="outline" size="sm" onClick={refresh} disabled={loading}>
            <RefreshCw className={`mr-1 h-4 w-4 ${loading ? "animate-spin" : ""}`} />
            Refresh
          </Button>
        }
      />

      {error ? <ErrorNote message={error} /> : null}

      <StatsTiles stats={stats} loading={loading} />

      {/* Tab switch (same segmented-control idiom as the W18 pages). */}
      <div className="mb-4 inline-flex rounded-md border border-border bg-muted/40 p-0.5">
        {(
          [
            ["queue", "Queue"],
            ["policies", "SLA policies"],
          ] as [Tab, string][]
        ).map(([key, label]) => (
          <button
            key={key}
            type="button"
            onClick={() => setTab(key)}
            className={`rounded px-3 py-1.5 text-sm font-medium transition-colors ${
              tab === key
                ? "bg-card text-foreground shadow-sm"
                : "text-muted-foreground hover:text-foreground"
            }`}
          >
            {label}
          </button>
        ))}
      </div>

      {tab === "queue" ? (
        loading && tickets.length === 0 && !error ? (
          <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-4">
            {Array.from({ length: 4 }).map((_, i) => (
              <div
                key={i}
                className="h-48 animate-pulse rounded-lg border border-border bg-muted"
              />
            ))}
          </div>
        ) : !loading && tickets.length === 0 && !error ? (
          <p className="rounded-md border border-border bg-card px-4 py-6 text-sm text-muted-foreground">
            No tickets match — adjust the filters, or create tickets via the
            helpdesk API (POST /v1/helpdesk/tickets).
          </p>
        ) : (
          <QueueBoard
            tickets={tickets}
            members={members}
            filters={filters}
            onFiltersChange={setFilters}
            onOpenTicket={openTicket}
            loading={loading}
          />
        )
      ) : (
        <SLAPolicyEditor
          orgSlug={orgSlug}
          policies={policies}
          canManage={canManage}
          onChanged={() => void loadPolicies()}
        />
      )}

      <TicketDrawer
        orgSlug={orgSlug}
        ticket={selected}
        members={members}
        canWork={canWork}
        open={drawerOpen}
        onOpenChange={setDrawerOpen}
        onChanged={refresh}
      />
    </div>
  );
}
