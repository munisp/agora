"use client";

/**
 * SPEC-W32 WS-C: case detail drawer.
 *
 * Opens from the triage queue, fetches the full case
 * (GET /api/civic/cases/{id} — reporter fields masked server-side unless
 * owner/admin; mirrored client-side for anonymous reporters) and exposes
 * the operator actions from SPEC §3 WS-A:
 *
 *   POST /api/civic/cases/{id}/triage   {category_id?, ward?, mda_queue?}
 *   POST /api/civic/cases/{id}/assign   {assignee}
 *   POST /api/civic/cases/{id}/status   {status, note?}
 *   POST /api/civic/cases/{id}/merge    {canonical_id}  (picker fed by
 *                                        GET …/{id}/duplicates)
 *
 * Location is a map-pin readout from lat/lon (coordinate text + OSM deep
 * link) — no map library. The event timeline renders the detail payload's
 * events/timeline/history array, falling back to the lifecycle timestamps
 * when the backend hasn't grown an event log yet.
 */
import * as React from "react";
import { GitMerge, Loader2, MapPin, X } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input, Label, Select } from "@/components/ui/input";
import { useToast } from "@/components/ui/toast";
import { cn, formatDateTime } from "@/lib/utils";
import {
  ACTIONABLE_STATUSES,
  CIVIC_API,
  SLA_TONE_META,
  channelLabel,
  extractEvents,
  normalizeCase,
  reporterDisplay,
  slaCountdown,
  statusLabel,
  unwrapList,
  type CivicCase,
  type CivicCaseEvent,
  type CivicCategory,
} from "./types";

/* ------------------------------------------------------------- helpers */

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
        {label}
      </div>
      <div className="mt-0.5 text-sm">{children}</div>
    </div>
  );
}

/** Fallback timeline synthesized from lifecycle timestamps. */
function synthesizedEvents(c: CivicCase): CivicCaseEvent[] {
  const ev: CivicCaseEvent[] = [];
  if (c.created_at)
    ev.push({ type: "received", at: c.created_at, note: null, actor: null });
  if (c.acked_at)
    ev.push({ type: "acknowledged", at: c.acked_at, note: null, actor: null });
  if (c.resolved_at)
    ev.push({ type: "resolved", at: c.resolved_at, note: null, actor: null });
  if (c.closed_at)
    ev.push({ type: "closed", at: c.closed_at, note: null, actor: null });
  return ev;
}

/* ----------------------------------------------------------------- drawer */

export function CaseDetail({
  orgSlug,
  caseItem,
  categories,
  canRevealReporter,
  open,
  onOpenChange,
  onChanged,
}: {
  orgSlug: string;
  caseItem: CivicCase | null;
  categories: CivicCategory[];
  canRevealReporter: boolean;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onChanged: () => void;
}) {
  const { toast } = useToast();
  const [detail, setDetail] = React.useState<CivicCase | null>(null);
  const [events, setEvents] = React.useState<CivicCaseEvent[]>([]);
  const [loading, setLoading] = React.useState(false);
  const [loadError, setLoadError] = React.useState<string | null>(null);

  // action state
  const [triageCategory, setTriageCategory] = React.useState("");
  const [triageWard, setTriageWard] = React.useState("");
  const [triageQueue, setTriageQueue] = React.useState("");
  const [assignee, setAssignee] = React.useState("");
  const [nextStatus, setNextStatus] = React.useState("");
  const [statusNote, setStatusNote] = React.useState("");
  const [busy, setBusy] = React.useState<string | null>(null);

  // merge picker
  const [dupes, setDupes] = React.useState<CivicCase[] | null>(null);
  const [dupesLoading, setDupesLoading] = React.useState(false);
  const [mergeTarget, setMergeTarget] = React.useState("");

  const load = React.useCallback(
    async (id: string, signal?: AbortSignal) => {
      setLoading(true);
      setLoadError(null);
      try {
        const data = await api.get<unknown>(
          `${CIVIC_API}/cases/${encodeURIComponent(id)}`,
          { tenant: orgSlug },
          signal,
        );
        if (signal?.aborted) return;
        const c = normalizeCase(data);
        setDetail(c);
        const ev = extractEvents(data);
        setEvents(ev.length > 0 ? ev : synthesizedEvents(c));
      } catch (e) {
        if (signal?.aborted) return;
        // Fall back to the list row so the drawer is still useful when the
        // detail route isn't deployed yet.
        setDetail(caseItem);
        setEvents(caseItem ? synthesizedEvents(caseItem) : []);
        setLoadError(
          e instanceof ApiError && e.status === 404
            ? "Full case detail is not available yet — showing the queue row."
            : "Could not load the full case — showing the queue row.",
        );
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [orgSlug, caseItem],
  );

  React.useEffect(() => {
    if (!open || !caseItem) return;
    const controller = new AbortController();
    void load(caseItem.id, controller.signal);
    return () => controller.abort();
  }, [open, caseItem, load]);

  // Reset action forms whenever a different case opens.
  React.useEffect(() => {
    if (!open || !caseItem) return;
    setTriageCategory(caseItem.category_id ?? "");
    setTriageWard(caseItem.ward ?? "");
    setTriageQueue(caseItem.mda_queue ?? "");
    setAssignee(caseItem.assigned_to ?? "");
    setNextStatus("");
    setStatusNote("");
    setDupes(null);
    setMergeTarget("");
  }, [open, caseItem]);

  if (!open || !caseItem) return null;
  const c = detail ?? caseItem;
  const reporter = reporterDisplay(c, canRevealReporter);
  const ack = slaCountdown(c.ack_due_at, c.sla_breach_ack, c.acked_at);
  const res = slaCountdown(c.resolve_due_at, c.sla_breach_resolve, c.resolved_at);
  const terminal = c.status === "resolved" || c.status === "closed" || c.merged_into;

  const run = async (
    key: string,
    fn: () => Promise<unknown>,
    done: string,
  ) => {
    setBusy(key);
    try {
      await fn();
      toast({ title: done, variant: "success" });
      onChanged();
      await load(c.id);
    } catch (e) {
      toast({
        title: "Action failed",
        description: e instanceof ApiError ? e.message : "Please try again.",
        variant: "destructive",
      });
    } finally {
      setBusy(null);
    }
  };

  const doTriage = () =>
    run(
      "triage",
      () =>
        api.post(
          `${CIVIC_API}/cases/${encodeURIComponent(c.id)}/triage`,
          {
            category_id: triageCategory || undefined,
            ward: triageWard.trim() || undefined,
            mda_queue: triageQueue.trim() || undefined,
          },
          { tenant: orgSlug },
        ),
      `Case ${c.ref} triaged`,
    );

  const doAssign = () =>
    run(
      "assign",
      () =>
        api.post(
          `${CIVIC_API}/cases/${encodeURIComponent(c.id)}/assign`,
          { assignee: assignee.trim() },
          { tenant: orgSlug },
        ),
      `Case ${c.ref} assigned to ${assignee.trim()}`,
    );

  const doStatus = () =>
    run(
      "status",
      () =>
        api.post(
          `${CIVIC_API}/cases/${encodeURIComponent(c.id)}/status`,
          { status: nextStatus, note: statusNote.trim() || undefined },
          { tenant: orgSlug },
        ),
      `Case ${c.ref} → ${statusLabel(nextStatus)}`,
    );

  const loadDupes = async () => {
    setDupesLoading(true);
    try {
      const data = await api.get<unknown>(
        `${CIVIC_API}/cases/${encodeURIComponent(c.id)}/duplicates`,
        { tenant: orgSlug },
      );
      const rows = unwrapList<unknown>(data).map(normalizeCase);
      setDupes(rows.filter((d) => d.id !== c.id));
    } catch {
      setDupes([]);
    } finally {
      setDupesLoading(false);
    }
  };

  const doMerge = () =>
    run(
      "merge",
      () =>
        api.post(
          `${CIVIC_API}/cases/${encodeURIComponent(c.id)}/merge`,
          { canonical_id: mergeTarget },
          { tenant: orgSlug },
        ),
      `Case ${c.ref} merged`,
    );

  return (
    <div className="fixed inset-0 z-50">
      <div
        className="absolute inset-0 bg-foreground/40"
        aria-hidden
        onClick={() => onOpenChange(false)}
      />
      <aside className="absolute inset-y-0 right-0 flex w-full max-w-xl flex-col overflow-y-auto border-l border-border bg-card shadow-xl">
        <div className="flex items-start justify-between gap-3 border-b border-border px-5 py-4">
          <div>
            <div className="flex items-center gap-2">
              <h2 className="font-mono text-lg font-semibold">{c.ref}</h2>
              <Badge
                variant={
                  c.status === "resolved" || c.status === "closed"
                    ? "success"
                    : c.status === "new"
                      ? "warning"
                      : "secondary"
                }
              >
                {statusLabel(c.status)}
              </Badge>
              {c.merged_into ? (
                <Badge variant="outline">merged → {c.merged_into}</Badge>
              ) : null}
            </div>
            <p className="mt-1 text-xs text-muted-foreground">
              {c.created_at ? `Opened ${formatDateTime(c.created_at)}` : ""}
              {c.channel ? ` · via ${channelLabel(c.channel)}` : ""}
            </p>
          </div>
          <Button
            variant="ghost"
            size="icon"
            aria-label="Close case detail"
            onClick={() => onOpenChange(false)}
          >
            <X className="h-4 w-4" />
          </Button>
        </div>

        <div className="flex-1 space-y-6 px-5 py-4">
          {loadError ? (
            <p className="rounded-md border border-warning/40 bg-warning-soft px-3 py-2 text-xs text-warning">
              {loadError}
            </p>
          ) : null}
          {loading ? (
            <p className="flex items-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="h-4 w-4 animate-spin" /> Loading case…
            </p>
          ) : null}

          {/* SLA chips */}
          {ack || res ? (
            <div className="flex flex-wrap gap-2">
              {[
                { label: "Acknowledgement", cd: ack },
                { label: "Resolution", cd: res },
              ].map(({ label, cd }) =>
                cd ? (
                  <span
                    key={label}
                    className="inline-flex items-center rounded-full border px-2.5 py-1 text-xs font-medium"
                    style={{
                      color: SLA_TONE_META[cd.tone].fg,
                      backgroundColor: SLA_TONE_META[cd.tone].bg,
                      borderColor: SLA_TONE_META[cd.tone].border,
                    }}
                  >
                    {label} · {cd.label}
                  </span>
                ) : null,
              )}
            </div>
          ) : null}

          <p className="whitespace-pre-wrap text-sm">{c.description || "—"}</p>

          {/* Location readout — map pin from lat/lon, no map lib. */}
          <section className="space-y-2 rounded-md border border-border bg-muted/40 px-3 py-3">
            <div className="flex items-center gap-1.5 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              <MapPin className="h-3.5 w-3.5" /> Location
            </div>
            <div className="grid grid-cols-2 gap-3">
              <Field label="Ward">{c.ward ?? "—"}</Field>
              <Field label="LGA">{c.lga ?? "—"}</Field>
              <Field label="Coordinates">
                {c.lat !== null && c.lon !== null ? (
                  <a
                    className="font-mono text-xs underline underline-offset-2"
                    href={`https://www.openstreetmap.org/?mlat=${c.lat}&mlon=${c.lon}#map=16/${c.lat}/${c.lon}`}
                    target="_blank"
                    rel="noreferrer noopener"
                  >
                    {c.lat.toFixed(5)}, {c.lon.toFixed(5)}
                  </a>
                ) : (
                  "—"
                )}
              </Field>
              <Field label="Description">{c.location_text ?? "—"}</Field>
            </div>
          </section>

          {/* Reporter (masked per role — SPEC §4 gate 4) */}
          <section className="space-y-2">
            <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              Reporter
            </div>
            <div className="grid grid-cols-2 gap-3">
              <Field label="Name">{reporter.name}</Field>
              <Field label="Phone">
                <span className="font-mono text-xs">{reporter.phone}</span>
              </Field>
              <Field label="Anonymous">{c.anonymous ? "Yes" : "No"}</Field>
              <Field label="Wants updates">{c.wants_updates ? "Yes" : "No"}</Field>
            </div>
            {c.anonymous && !canRevealReporter ? (
              <p className="text-[11px] text-muted-foreground">
                Anonymous report — identity is masked for your role. Owner/admin
                can reveal it.
              </p>
            ) : null}
            {c.photo_url ? (
              <Field label="Photo">
                <a
                  className="text-xs underline underline-offset-2"
                  href={c.photo_url}
                  target="_blank"
                  rel="noreferrer noopener"
                >
                  View attachment
                </a>
              </Field>
            ) : null}
          </section>

          {/* Routing */}
          <section className="space-y-2">
            <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              Routing
            </div>
            <div className="grid grid-cols-2 gap-3">
              <Field label="Category">
                {categories.find(
                  (k) => k.id === c.category_id || k.slug === c.category_slug,
                )?.name ??
                  c.category_name ??
                  c.category_slug ??
                  "—"}
              </Field>
              <Field label="MDA queue">{c.mda_queue ?? "—"}</Field>
              <Field label="Assigned to">{c.assigned_to ?? "Unassigned"}</Field>
            </div>
          </section>

          {/* Actions — hidden on terminal/merged cases. */}
          {!terminal ? (
            <section className="space-y-4 rounded-md border border-border px-3 py-3">
              <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                Actions
              </div>

              <div className="space-y-2">
                <Label className="text-xs">Triage (category / ward / MDA queue)</Label>
                <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
                  <Select
                    aria-label="Triage category"
                    value={triageCategory}
                    onChange={(e) => setTriageCategory(e.target.value)}
                  >
                    <option value="">Keep category</option>
                    {categories.map((k) => (
                      <option key={k.id || k.slug} value={k.id}>
                        {k.name}
                      </option>
                    ))}
                  </Select>
                  <Input
                    aria-label="Triage ward"
                    placeholder="Ward"
                    value={triageWard}
                    onChange={(e) => setTriageWard(e.target.value)}
                  />
                  <Input
                    aria-label="Triage MDA queue"
                    placeholder="MDA queue"
                    value={triageQueue}
                    onChange={(e) => setTriageQueue(e.target.value)}
                  />
                </div>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => void doTriage()}
                  disabled={busy !== null}
                >
                  {busy === "triage" ? "Triaging…" : "Triage"}
                </Button>
              </div>

              <div className="space-y-2">
                <Label htmlFor="case-assignee" className="text-xs">
                  Assign
                </Label>
                <div className="flex gap-2">
                  <Input
                    id="case-assignee"
                    placeholder="Operator / team name…"
                    value={assignee}
                    onChange={(e) => setAssignee(e.target.value)}
                  />
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() => void doAssign()}
                    disabled={busy !== null || !assignee.trim()}
                  >
                    {busy === "assign" ? "Assigning…" : "Assign"}
                  </Button>
                </div>
              </div>

              <div className="space-y-2">
                <Label htmlFor="case-status" className="text-xs">
                  Update status
                </Label>
                <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
                  <Select
                    id="case-status"
                    value={nextStatus}
                    onChange={(e) => setNextStatus(e.target.value)}
                  >
                    <option value="">Choose status…</option>
                    {ACTIONABLE_STATUSES.map((s) => (
                      <option key={s} value={s}>
                        {statusLabel(s)}
                      </option>
                    ))}
                  </Select>
                  <Input
                    aria-label="Status note"
                    placeholder="Note (optional)…"
                    value={statusNote}
                    onChange={(e) => setStatusNote(e.target.value)}
                  />
                </div>
                <Button
                  size="sm"
                  onClick={() => void doStatus()}
                  disabled={busy !== null || !nextStatus}
                >
                  {busy === "status" ? "Updating…" : "Update status"}
                </Button>
              </div>

              <div className="space-y-2 border-t border-border pt-3">
                <div className="flex items-center gap-2">
                  <GitMerge className="h-3.5 w-3.5 text-muted-foreground" />
                  <Label className="text-xs">Merge duplicate</Label>
                  {dupes === null ? (
                    <Button
                      size="sm"
                      variant="ghost"
                      onClick={() => void loadDupes()}
                      disabled={dupesLoading}
                    >
                      {dupesLoading ? "Finding…" : "Find duplicates"}
                    </Button>
                  ) : null}
                </div>
                {dupes !== null ? (
                  dupes.length === 0 ? (
                    <p className="text-xs text-muted-foreground">
                      No duplicate candidates (same category, ≤500 m, ±72 h).
                    </p>
                  ) : (
                    <div className="flex gap-2">
                      <Select
                        aria-label="Canonical case"
                        value={mergeTarget}
                        onChange={(e) => setMergeTarget(e.target.value)}
                      >
                        <option value="">Merge into…</option>
                        {dupes.map((d) => (
                          <option key={d.id} value={d.id}>
                            {d.ref} · {statusLabel(d.status)}
                            {d.ward ? ` · ${d.ward}` : ""}
                          </option>
                        ))}
                      </Select>
                      <Button
                        size="sm"
                        variant="outline"
                        onClick={() => void doMerge()}
                        disabled={busy !== null || !mergeTarget}
                      >
                        {busy === "merge" ? "Merging…" : "Merge"}
                      </Button>
                    </div>
                  )
                ) : null}
                <p className="text-[11px] text-muted-foreground">
                  Merging keeps this case readable and points it at the
                  canonical case; citizen notifications follow the canonical
                  case.
                </p>
              </div>
            </section>
          ) : null}

          {/* Timeline */}
          <section className="space-y-2 pb-6">
            <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              Timeline
            </div>
            {events.length === 0 ? (
              <p className="text-xs text-muted-foreground">No events yet.</p>
            ) : (
              <ol className="relative space-y-3 border-l border-border pl-4">
                {events.map((ev, i) => (
                  <li key={`${ev.type}-${ev.at ?? i}`} className="text-sm">
                    <span
                      className={cn(
                        "absolute -left-[5px] mt-1.5 h-2.5 w-2.5 rounded-full border border-card",
                        i === events.length - 1
                          ? "bg-[#C0562F]"
                          : "bg-[#7A8B6F]",
                      )}
                      aria-hidden
                    />
                    <div className="font-medium capitalize">
                      {statusLabel(ev.type)}
                    </div>
                    <div className="text-xs text-muted-foreground">
                      {ev.at ? formatDateTime(ev.at) : "—"}
                      {ev.actor ? ` · ${ev.actor}` : ""}
                    </div>
                    {ev.note ? (
                      <div className="mt-0.5 text-xs">{ev.note}</div>
                    ) : null}
                  </li>
                ))}
              </ol>
            )}
          </section>
        </div>
      </aside>
    </div>
  );
}
