"use client";

/**
 * Helpdesk queue board (SPEC-W19 Agent A): status columns (open / pending /
 * resolved / closed), ticket cards with priority pills and SLA-breach
 * badges, plus the filter bar (search, priority, assignee, channel).
 * Clicking a card opens the ticket detail drawer.
 *
 * Data: GET /api/bookings/v1/helpdesk/tickets?status=&priority=&assignee_id=&channel=&q=
 */
import * as React from "react";
import { AlertTriangle, Search, User } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Input, Select } from "@/components/ui/input";
import { formatDateTime } from "@/lib/utils";
import {
  PRIORITIES,
  PRIORITY_META,
  STATUSES,
  STATUS_META,
  ticketBreaches,
  type TeamMember,
  type Ticket,
  type TicketPriority,
  type TicketStatus,
} from "@/components/apps/helpdesk/types";

export interface QueueFilters {
  q: string;
  priority: "" | TicketPriority;
  assigneeId: string;
  channel: string;
}

export function QueueBoard({
  tickets,
  members,
  filters,
  onFiltersChange,
  onOpenTicket,
  loading,
}: {
  tickets: Ticket[];
  members: TeamMember[];
  filters: QueueFilters;
  onFiltersChange: (f: QueueFilters) => void;
  onOpenTicket: (t: Ticket) => void;
  loading: boolean;
}) {
  const memberName = React.useMemo(() => {
    const map = new Map(members.map((m) => [m.id, m.name]));
    return (id: string | null) => (id ? map.get(id) ?? "Former member" : null);
  }, [members]);

  const columns = React.useMemo(() => {
    const byStatus = new Map<TicketStatus, Ticket[]>();
    for (const s of STATUSES) byStatus.set(s, []);
    for (const t of tickets) byStatus.get(t.status)?.push(t);
    return byStatus;
  }, [tickets]);

  return (
    <div>
      {/* Filter bar */}
      <div className="mb-4 flex flex-wrap items-center gap-2">
        <div className="relative">
          <Search className="pointer-events-none absolute left-2.5 top-2.5 h-4 w-4 text-muted-foreground" />
          <Input
            className="w-56 pl-8"
            placeholder="Search subject…"
            value={filters.q}
            onChange={(e) => onFiltersChange({ ...filters, q: e.target.value })}
          />
        </div>
        <Select
          value={filters.priority}
          onChange={(e) =>
            onFiltersChange({
              ...filters,
              priority: e.target.value as QueueFilters["priority"],
            })
          }
        >
          <option value="">All priorities</option>
          {PRIORITIES.map((p) => (
            <option key={p} value={p}>
              {PRIORITY_META[p].label}
            </option>
          ))}
        </Select>
        <Select
          value={filters.assigneeId}
          onChange={(e) =>
            onFiltersChange({ ...filters, assigneeId: e.target.value })
          }
        >
          <option value="">All assignees</option>
          {members.map((m) => (
            <option key={m.id} value={m.id}>
              {m.name}
            </option>
          ))}
        </Select>
        <Input
          className="w-36"
          placeholder="Channel"
          value={filters.channel}
          onChange={(e) => onFiltersChange({ ...filters, channel: e.target.value })}
        />
      </div>

      {/* Status columns */}
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-4">
        {STATUSES.map((status) => {
          const list = columns.get(status) ?? [];
          return (
            <div
              key={status}
              className="rounded-lg border border-border bg-muted/40 p-3"
            >
              <div className="mb-3 flex items-center justify-between">
                <h3 className="text-sm font-semibold">{STATUS_META[status].label}</h3>
                <Badge variant="secondary">{list.length}</Badge>
              </div>
              <div className="space-y-2">
                {loading && tickets.length === 0 ? (
                  <div className="h-16 animate-pulse rounded-md border border-border bg-muted" />
                ) : list.length === 0 ? (
                  <p className="rounded-md border border-dashed border-border px-3 py-4 text-center text-xs text-muted-foreground">
                    No tickets
                  </p>
                ) : (
                  list.map((t) => (
                    <TicketCard
                      key={t.id}
                      ticket={t}
                      assigneeName={memberName(t.assignee_id)}
                      onClick={() => onOpenTicket(t)}
                    />
                  ))
                )}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function TicketCard({
  ticket,
  assigneeName,
  onClick,
}: {
  ticket: Ticket;
  assigneeName: string | null;
  onClick: () => void;
}) {
  const breach = ticketBreaches(ticket);
  const prio = PRIORITY_META[ticket.priority] ?? PRIORITY_META.normal;
  return (
    <button
      type="button"
      onClick={onClick}
      className="w-full rounded-md border border-border bg-card p-3 text-left shadow-sm transition-colors hover:border-primary/50"
    >
      <div className="mb-1.5 flex items-start justify-between gap-2">
        <p className="line-clamp-2 text-sm font-medium">{ticket.subject}</p>
        <Badge variant={prio.variant}>{prio.label}</Badge>
      </div>
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
        <span className="rounded bg-muted px-1.5 py-0.5">{ticket.channel}</span>
        {assigneeName ? (
          <span className="inline-flex items-center gap-1">
            <User className="h-3 w-3" />
            {assigneeName}
          </span>
        ) : (
          <span className="italic">Unassigned</span>
        )}
        <span>{formatDateTime(ticket.created_at)}</span>
      </div>
      {(breach.firstResponse || breach.resolve) && (
        <div className="mt-1.5 flex flex-wrap gap-1">
          {breach.firstResponse && (
            <Badge variant="destructive">
              <AlertTriangle className="mr-1 h-3 w-3" />
              First-response breach
            </Badge>
          )}
          {breach.resolve && (
            <Badge variant="destructive">
              <AlertTriangle className="mr-1 h-3 w-3" />
              Resolve breach
            </Badge>
          )}
        </div>
      )}
      {ticket.csat_rating !== null && (
        <p className="mt-1.5 text-xs text-muted-foreground">
          CSAT {ticket.csat_rating}/5
        </p>
      )}
    </button>
  );
}
