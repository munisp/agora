"use client";

/**
 * Ticket detail drawer (SPEC-W19 Agent A): slides in from the right (same
 * overlay/Escape idiom as apps/config-drawer.tsx — there is no drawer
 * primitive) and shows the ticket with its event timeline, assignment
 * control (member / "Auto-assign" / unassign), status flow, note composer
 * and CSAT capture (resolved/closed only).
 *
 * Data:
 *   - GET   /api/bookings/v1/helpdesk/tickets/{id}      (ticket + events)
 *   - PATCH /api/bookings/v1/helpdesk/tickets/{id}      (assign/status/note)
 *   - PATCH /api/bookings/v1/helpdesk/tickets/{id}/csat (rating 1-5)
 */
import * as React from "react";
import { Star, X } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Label, Select, Textarea } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import { formatDateTime } from "@/lib/utils";
import {
  eventLabel,
  PRIORITY_META,
  STATUSES,
  STATUS_META,
  ticketBreaches,
  type TeamMember,
  type Ticket,
  type TicketEvent,
} from "@/components/apps/helpdesk/types";

export function TicketDrawer({
  orgSlug,
  ticket,
  members,
  canWork,
  open,
  onOpenChange,
  onChanged,
}: {
  orgSlug: string;
  /** the board card that was clicked; null while closed */
  ticket: Ticket | null;
  members: TeamMember[];
  /** owner/admin/staff — viewers get a read-only drawer */
  canWork: boolean;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** called after any successful mutation so the board/stats refresh */
  onChanged: () => void;
}) {
  const { toast } = useToast();
  const [detail, setDetail] = React.useState<Ticket | null>(null);
  const [events, setEvents] = React.useState<TicketEvent[]>([]);
  const [loading, setLoading] = React.useState(false);
  const [loadError, setLoadError] = React.useState<string | null>(null);
  const [note, setNote] = React.useState("");
  const [assigneeSel, setAssigneeSel] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [csatRating, setCsatRating] = React.useState(0);
  const [csatComment, setCsatComment] = React.useState("");

  const load = React.useCallback(
    async (id: string, signal?: AbortSignal) => {
      setLoading(true);
      setLoadError(null);
      try {
        const data = await api.get<{ ticket: Ticket; events: TicketEvent[] }>(
          `/api/bookings/v1/helpdesk/tickets/${id}`,
          { tenant: orgSlug },
          signal,
        );
        if (signal?.aborted) return;
        setDetail(data.ticket);
        setEvents(Array.isArray(data.events) ? data.events : []);
      } catch (e) {
        if (signal?.aborted) return;
        setLoadError(
          e instanceof ApiError ? e.message : "Failed to load the ticket.",
        );
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [orgSlug],
  );

  // Load the full detail (timeline) whenever a ticket is opened.
  React.useEffect(() => {
    if (open && ticket) {
      setDetail(ticket);
      setEvents([]);
      setNote("");
      setAssigneeSel(ticket.assignee_id ?? "");
      setCsatRating(ticket.csat_rating ?? 0);
      setCsatComment(ticket.csat_comment ?? "");
      const controller = new AbortController();
      void load(ticket.id, controller.signal);
      return () => controller.abort();
    }
  }, [open, ticket, load]);

  // Escape closes — same idiom as components/apps/config-drawer.tsx.
  React.useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onOpenChange(false);
    };
    document.addEventListener("keydown", onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = prev;
    };
  }, [open, onOpenChange]);

  if (!open || !ticket) return null;

  const t = detail ?? ticket;
  const breach = ticketBreaches(t);
  const prio = PRIORITY_META[t.priority] ?? PRIORITY_META.normal;
  const memberName = (id: string | null) =>
    id ? members.find((m) => m.id === id)?.name ?? "Former member" : null;

  const patch = async (body: Record<string, unknown>, okTitle: string) => {
    setBusy(true);
    try {
      const data = await api.patch<{ ticket: Ticket }>(
        `/api/bookings/v1/helpdesk/tickets/${t.id}`,
        body,
        { tenant: orgSlug },
      );
      setDetail(data.ticket);
      toast({ title: okTitle, variant: "success" });
      await load(t.id); // refresh the timeline
      onChanged();
    } catch (e) {
      toast({
        title: "Update failed",
        description: e instanceof ApiError ? e.message : "The helpdesk service may be offline.",
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  const saveNote = async () => {
    const body = note.trim();
    if (!body) return;
    await patch({ note: body }, "Note added");
    setNote("");
  };

  const saveAssignee = async (value: string) => {
    setAssigneeSel(value);
    if (value === "__auto__") {
      await patch({ assignee_id: "auto" }, "Auto-assigned");
    } else if (value === "") {
      await patch({ assignee_id: null }, "Unassigned");
    } else {
      await patch({ assignee_id: value }, "Assignee updated");
    }
  };

  const saveCsat = async () => {
    if (csatRating < 1 || csatRating > 5) return;
    setBusy(true);
    try {
      const data = await api.patch<{ ticket: Ticket }>(
        `/api/bookings/v1/helpdesk/tickets/${t.id}/csat`,
        { rating: csatRating, comment: csatComment.trim() || undefined },
        { tenant: orgSlug },
      );
      setDetail(data.ticket);
      toast({ title: "CSAT recorded", variant: "success" });
      onChanged();
    } catch (e) {
      toast({
        title: "CSAT failed",
        description: e instanceof ApiError ? e.message : "The helpdesk service may be offline.",
        variant: "destructive",
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="fixed inset-0 z-50">
      <div
        className="absolute inset-0 bg-black/40"
        onClick={() => onOpenChange(false)}
        aria-hidden
      />
      <div className="absolute inset-y-0 right-0 flex w-full max-w-lg flex-col border-l border-border bg-background shadow-xl">
        {/* Header */}
        <div className="flex items-start justify-between gap-3 border-b border-border p-4">
          <div className="min-w-0">
            <div className="mb-1 flex flex-wrap items-center gap-2">
              <Badge variant={prio.variant}>{prio.label}</Badge>
              <Badge variant="outline">{STATUS_META[t.status].label}</Badge>
              <span className="rounded bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">
                {t.channel}
              </span>
              {breach.firstResponse && (
                <Badge variant="destructive">First-response breach</Badge>
              )}
              {breach.resolve && <Badge variant="destructive">Resolve breach</Badge>}
            </div>
            <h2 className="text-lg font-semibold leading-snug">{t.subject}</h2>
            <p className="mt-0.5 text-xs text-muted-foreground">
              Opened {formatDateTime(t.created_at)}
            </p>
          </div>
          <button
            type="button"
            onClick={() => onOpenChange(false)}
            className="rounded-md p-1 text-muted-foreground hover:bg-muted"
            aria-label="Close"
          >
            <X className="h-5 w-5" />
          </button>
        </div>

        {/* Body */}
        <div className="flex-1 space-y-5 overflow-y-auto p-4">
          {loadError ? (
            <p className="rounded-md border border-warning/40 bg-warning-soft px-3 py-2 text-sm text-warning">
              {loadError}
            </p>
          ) : null}

          {/* SLA clocks */}
          <div className="grid grid-cols-2 gap-2 text-xs">
            <div className="rounded-md border border-border p-2">
              <p className="text-muted-foreground">First response</p>
              <p className="mt-0.5 font-medium">
                {t.first_response_at
                  ? `Done ${formatDateTime(t.first_response_at)}`
                  : t.due_first_response_at
                    ? `Due ${formatDateTime(t.due_first_response_at)}`
                    : "No policy"}
              </p>
            </div>
            <div className="rounded-md border border-border p-2">
              <p className="text-muted-foreground">Resolution</p>
              <p className="mt-0.5 font-medium">
                {t.resolved_at
                  ? `Done ${formatDateTime(t.resolved_at)}`
                  : t.due_resolve_at
                    ? `Due ${formatDateTime(t.due_resolve_at)}`
                    : "No policy"}
              </p>
            </div>
          </div>

          {/* Assignment + status (staff only) */}
          {canWork ? (
            <div className="space-y-3 rounded-md border border-border p-3">
              <div>
                <Label htmlFor="hd-assignee">Assignee</Label>
                <Select
                  id="hd-assignee"
                  value={assigneeSel}
                  disabled={busy}
                  onChange={(e) => void saveAssignee(e.target.value)}
                >
                  <option value="">Unassigned</option>
                  <option value="__auto__">Auto-assign (least open tickets)</option>
                  {members.map((m) => (
                    <option key={m.id} value={m.id}>
                      {m.name}
                    </option>
                  ))}
                </Select>
              </div>
              <div>
                <Label>Status</Label>
                <div className="flex flex-wrap gap-1.5">
                  {STATUSES.map((s) => (
                    <Button
                      key={s}
                      size="sm"
                      variant={t.status === s ? "default" : "outline"}
                      disabled={busy || t.status === s}
                      onClick={() => void patch({ status: s }, `Status → ${STATUS_META[s].label}`)}
                    >
                      {STATUS_META[s].label}
                    </Button>
                  ))}
                </div>
              </div>
            </div>
          ) : (
            <p className="text-xs text-muted-foreground">
              Assignee: {memberName(t.assignee_id) ?? "Unassigned"} · read-only for your role
            </p>
          )}

          {/* Timeline */}
          <div>
            <h3 className="mb-2 text-sm font-semibold">Timeline</h3>
            {loading && events.length === 0 ? (
              <div className="space-y-2">
                {[0, 1].map((i) => (
                  <div key={i} className="h-10 animate-pulse rounded-md border border-border bg-muted" />
                ))}
              </div>
            ) : events.length === 0 ? (
              <p className="rounded-md border border-dashed border-border px-3 py-4 text-center text-xs text-muted-foreground">
                No timeline events yet
              </p>
            ) : (
              <ol className="space-y-2">
                {events.map((e) => (
                  <li key={e.id} className="rounded-md border border-border p-2.5 text-sm">
                    <div className="flex items-center justify-between gap-2">
                      <span className="font-medium">{eventLabel(e)}</span>
                      <span className="text-xs text-muted-foreground">
                        {formatDateTime(e.ts)}
                      </span>
                    </div>
                    {e.actor ? (
                      <p className="mt-0.5 text-xs text-muted-foreground">by {e.actor}</p>
                    ) : null}
                    {e.kind === "note" && typeof e.payload.body === "string" ? (
                      <p className="mt-1 whitespace-pre-wrap text-sm">{e.payload.body}</p>
                    ) : null}
                    {e.kind === "assigned" && typeof e.payload.assignee_name === "string" ? (
                      <p className="mt-0.5 text-xs text-muted-foreground">
                        → {e.payload.assignee_name}
                      </p>
                    ) : null}
                  </li>
                ))}
              </ol>
            )}
          </div>

          {/* Note composer (staff only) */}
          {canWork ? (
            <div className="space-y-2">
              <Label htmlFor="hd-note">Add note</Label>
              <Textarea
                id="hd-note"
                rows={3}
                placeholder="Internal note or customer reply…"
                value={note}
                onChange={(e) => setNote(e.target.value)}
              />
              <Button
                size="sm"
                disabled={busy || !note.trim()}
                onClick={() => void saveNote()}
              >
                Add note
              </Button>
            </div>
          ) : null}

          {/* CSAT */}
          <div className="rounded-md border border-border p-3">
            <h3 className="mb-2 text-sm font-semibold">Customer satisfaction</h3>
            {t.csat_rating !== null ? (
              <div>
                <p className="flex items-center gap-1 text-sm">
                  {Array.from({ length: 5 }).map((_, i) => (
                    <Star
                      key={i}
                      className={`h-4 w-4 ${
                        i < (t.csat_rating ?? 0)
                          ? "fill-warning text-warning"
                          : "text-muted-foreground"
                      }`}
                    />
                  ))}
                  <span className="ml-1 text-xs text-muted-foreground">
                    {t.csat_at ? formatDateTime(t.csat_at) : ""}
                  </span>
                </p>
                {t.csat_comment ? (
                  <p className="mt-1 text-sm text-muted-foreground">{t.csat_comment}</p>
                ) : null}
              </div>
            ) : t.status === "resolved" || t.status === "closed" ? (
              <div className="space-y-2">
                <div className="flex items-center gap-1">
                  {Array.from({ length: 5 }).map((_, i) => (
                    <button
                      key={i}
                      type="button"
                      onClick={() => setCsatRating(i + 1)}
                      aria-label={`Rate ${i + 1}`}
                    >
                      <Star
                        className={`h-5 w-5 ${
                          i < csatRating ? "fill-warning text-warning" : "text-muted-foreground"
                        }`}
                      />
                    </button>
                  ))}
                </div>
                <Textarea
                  rows={2}
                  placeholder="Optional comment"
                  value={csatComment}
                  onChange={(e) => setCsatComment(e.target.value)}
                />
                <Button size="sm" disabled={busy || csatRating === 0} onClick={() => void saveCsat()}>
                  Record CSAT
                </Button>
              </div>
            ) : (
              <p className="text-xs text-muted-foreground">
                Available once the ticket is resolved.
              </p>
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
