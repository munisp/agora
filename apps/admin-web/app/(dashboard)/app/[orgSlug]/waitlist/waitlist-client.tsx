"use client";

import * as React from "react";
import { Bell, Link2, Plus, RefreshCw, TimerOff } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { ConfirmDialog } from "@/components/ui/dialog";
import { Input, Label, Select } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useToast } from "@/components/ui/toast";
import { formatDateTime } from "@/lib/utils";
import type { Offering, WaitlistEntry } from "@/lib/types";

type StatusFilter = "" | "waiting" | "notified" | "claimed" | "expired";

/**
 * Row-action guards. The W48 backend spells the pre-claim state "waiting"
 * (pending in the W48-A contract); both spellings are accepted so the UI
 * keeps working across either naming.
 */
const NOTIFYABLE: ReadonlySet<string> = new Set(["waiting", "pending"]);
const EXPIRABLE: ReadonlySet<string> = new Set([
  "waiting",
  "pending",
  "notified",
]);
/** Entries whose claim link is still usable (not yet claimed/expired). */
const LINKABLE: ReadonlySet<string> = new Set([
  "waiting",
  "pending",
  "notified",
]);

function statusVariant(
  status: string,
): "success" | "warning" | "destructive" | "secondary" {
  switch (status) {
    case "claimed":
      return "success";
    case "waiting":
    case "pending":
    case "notified":
      return "warning";
    case "expired":
      return "secondary";
    default:
      return "secondary";
  }
}

/** Local datetime-local value → RFC3339 (the API expects timestamps). */
function localToIso(v: string): string | null {
  if (!v) return null;
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? null : d.toISOString();
}

export function WaitlistClient({
  orgSlug,
  siteSlug,
  initialEntries,
  initialOfferings,
  initialError,
}: {
  orgSlug: string;
  siteSlug: string;
  initialEntries: WaitlistEntry[];
  initialOfferings: Offering[];
  initialError: string | null;
}) {
  const { toast } = useToast();
  const [entries, setEntries] = React.useState<WaitlistEntry[]>(initialEntries);
  const [error, setError] = React.useState<string | null>(initialError);
  const [loading, setLoading] = React.useState(false);
  const [status, setStatus] = React.useState<StatusFilter>("");
  const [creating, setCreating] = React.useState(false);
  const [showCreate, setShowCreate] = React.useState(false);
  const [busyId, setBusyId] = React.useState<string | null>(null);
  const [expireTarget, setExpireTarget] = React.useState<WaitlistEntry | null>(
    null,
  );
  const [form, setForm] = React.useState({
    offering_id: "",
    contact_name: "",
    contact_phone: "",
    window_start: "",
    window_end: "",
  });

  const offeringName = React.useMemo(() => {
    const map = new Map(initialOfferings.map((o) => [o.id, o.name]));
    return (id: string) => map.get(id) ?? id;
  }, [initialOfferings]);

  const load = React.useCallback(
    async (f: StatusFilter) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<{ entries?: WaitlistEntry[] }>(
          "/api/bookings/v1/waitlist",
          { tenant: orgSlug, status: f || undefined },
        );
        setEntries(data.entries ?? []);
      } catch (e) {
        setError(
          e instanceof ApiError ? e.message : "Failed to load the waitlist.",
        );
      } finally {
        setLoading(false);
      }
    },
    [orgSlug],
  );

  // The default (unfiltered) list arrives server-rendered; refetch only on
  // filter change.
  const skipInitialLoad = React.useRef(true);
  React.useEffect(() => {
    if (skipInitialLoad.current) {
      skipInitialLoad.current = false;
      return;
    }
    void load(status);
  }, [status, load]);

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    const windowStart = localToIso(form.window_start);
    const windowEnd = localToIso(form.window_end);
    if (!form.offering_id || !form.contact_phone.trim() || !windowStart || !windowEnd)
      return;
    setCreating(true);
    try {
      await api.post<WaitlistEntry>(
        "/api/bookings/v1/waitlist",
        {
          offering_id: form.offering_id,
          contact_name: form.contact_name.trim(),
          contact_phone: form.contact_phone.trim(),
          window_start: windowStart,
          window_end: windowEnd,
        },
        { tenant: orgSlug },
      );
      toast({
        title: "Waitlist entry added",
        description:
          "The customer is notified automatically (top 3, FIFO) when a matching booking is cancelled.",
        variant: "success",
      });
      setShowCreate(false);
      setForm({
        offering_id: "",
        contact_name: "",
        contact_phone: "",
        window_start: "",
        window_end: "",
      });
      await load(status);
    } catch (err) {
      toast({
        title: "Could not add the entry",
        description: err instanceof ApiError ? err.message : undefined,
        variant: "destructive",
      });
    } finally {
      setCreating(false);
    }
  };

  /** POST /v1/waitlist/{id}/notify — sends the claim link to the customer. */
  const notify = async (entry: WaitlistEntry) => {
    setBusyId(entry.id);
    try {
      await api.post(
        `/api/bookings/v1/waitlist/${entry.id}/notify`,
        {},
        { tenant: orgSlug },
      );
      toast({
        title: "Customer notified",
        description: `${
          entry.contact_name || entry.contact_phone
        } was sent their single-use claim link.`,
        variant: "success",
      });
      await load(status);
    } catch (e) {
      toast({
        title: "Notify failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusyId(null);
    }
  };

  /** POST /v1/waitlist/{id}/expire — manual expiry (confirmed first). */
  const expire = async () => {
    if (!expireTarget) return;
    setBusyId(expireTarget.id);
    try {
      await api.post(
        `/api/bookings/v1/waitlist/${expireTarget.id}/expire`,
        {},
        { tenant: orgSlug },
      );
      toast({
        title: "Entry expired",
        description: "The claim link for this entry no longer works.",
        variant: "success",
      });
      setExpireTarget(null);
      await load(status);
    } catch (e) {
      toast({
        title: "Expire failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setBusyId(null);
    }
  };

  const copyClaimLink = async (entry: WaitlistEntry) => {
    const link = `${window.location.origin}/p/${siteSlug}/claim?token=${entry.claim_token}`;
    try {
      await navigator.clipboard.writeText(link);
      toast({
        title: "Claim link copied",
        description:
          "Send it to the customer — the single-use token is the only credential needed.",
        variant: "success",
      });
    } catch {
      toast({
        title: "Claim link",
        description: link,
      });
    }
  };

  return (
    <div>
      <PageHeader
        title="Waitlist"
        description="Customers waiting for an opening. When a booking is cancelled the top 3 waiting entries for the offering are notified automatically with a single-use claim link; you can also notify an entry manually, copy its link for manual outreach, or expire it."
        actions={
          <>
            <Button
              variant="outline"
              size="sm"
              onClick={() => void load(status)}
              disabled={loading}
            >
              <RefreshCw className={loading ? "h-3.5 w-3.5 animate-spin" : "h-3.5 w-3.5"} />
              Refresh
            </Button>
            <Button size="sm" onClick={() => setShowCreate((v) => !v)}>
              <Plus className="h-4 w-4" />
              Add entry
            </Button>
          </>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      {showCreate ? (
        <Card className="mb-4">
          <CardHeader>
            <CardTitle>Add to waitlist</CardTitle>
          </CardHeader>
          <CardContent>
            {initialOfferings.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                No bookable offerings found — create an offering first.
              </p>
            ) : (
              <form onSubmit={create} className="flex flex-wrap items-end gap-3">
                <div className="grid min-w-48 gap-1.5">
                  <Label htmlFor="wl-offering">Offering</Label>
                  <Select
                    id="wl-offering"
                    value={form.offering_id}
                    onChange={(e) =>
                      setForm((f) => ({ ...f, offering_id: e.target.value }))
                    }
                  >
                    <option value="">Select…</option>
                    {initialOfferings.map((o) => (
                      <option key={o.id} value={o.id}>
                        {o.name}
                      </option>
                    ))}
                  </Select>
                </div>
                <div className="grid min-w-40 gap-1.5">
                  <Label htmlFor="wl-name">Customer name</Label>
                  <Input
                    id="wl-name"
                    value={form.contact_name}
                    onChange={(e) =>
                      setForm((f) => ({ ...f, contact_name: e.target.value }))
                    }
                  />
                </div>
                <div className="grid min-w-40 gap-1.5">
                  <Label htmlFor="wl-phone">Phone (required)</Label>
                  <Input
                    id="wl-phone"
                    type="tel"
                    placeholder="+234…"
                    value={form.contact_phone}
                    onChange={(e) =>
                      setForm((f) => ({ ...f, contact_phone: e.target.value }))
                    }
                  />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor="wl-from">Window from</Label>
                  <Input
                    id="wl-from"
                    type="datetime-local"
                    value={form.window_start}
                    onChange={(e) =>
                      setForm((f) => ({ ...f, window_start: e.target.value }))
                    }
                  />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor="wl-to">Window to</Label>
                  <Input
                    id="wl-to"
                    type="datetime-local"
                    value={form.window_end}
                    onChange={(e) =>
                      setForm((f) => ({ ...f, window_end: e.target.value }))
                    }
                  />
                </div>
                <Button
                  type="submit"
                  disabled={
                    creating ||
                    !form.offering_id ||
                    !form.contact_phone.trim() ||
                    !form.window_start ||
                    !form.window_end
                  }
                >
                  {creating ? "Adding…" : "Add entry"}
                </Button>
              </form>
            )}
          </CardContent>
        </Card>
      ) : null}

      <div className="mb-3 flex items-center gap-2">
        <Label htmlFor="wl-status" className="text-xs">
          Status
        </Label>
        <Select
          id="wl-status"
          value={status}
          onChange={(e) => setStatus(e.target.value as StatusFilter)}
          className="w-40"
        >
          <option value="">All</option>
          <option value="waiting">Waiting</option>
          <option value="notified">Notified</option>
          <option value="claimed">Claimed</option>
          <option value="expired">Expired</option>
        </Select>
      </div>

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">Customer</TableHead>
              <TableHead>Offering</TableHead>
              <TableHead>Window</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Added</TableHead>
              <TableHead className="pr-5 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {entries.length === 0 ? (
              <TableEmpty colSpan={6}>
                {loading
                  ? "Loading…"
                  : "Nobody is waiting — entries appear here when customers join the waitlist."}
              </TableEmpty>
            ) : (
              entries.map((w) => (
                <TableRow key={w.id}>
                  <TableCell className="pl-5">
                    <div className="font-medium">
                      {w.contact_name || "—"}
                    </div>
                    <div className="text-xs text-muted-foreground">
                      {w.contact_phone}
                    </div>
                  </TableCell>
                  <TableCell>{offeringName(w.offering_id)}</TableCell>
                  <TableCell className="text-xs">
                    {formatDateTime(w.window_start)}
                    <br />
                    <span className="text-muted-foreground">
                      to {formatDateTime(w.window_end)}
                    </span>
                  </TableCell>
                  <TableCell>
                    <Badge variant={statusVariant(w.status)}>{w.status}</Badge>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {formatDateTime(w.created_at)}
                  </TableCell>
                  <TableCell className="pr-5 text-right">
                    <div className="flex items-center justify-end gap-2">
                      {LINKABLE.has(w.status) ? (
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={() => void copyClaimLink(w)}
                        >
                          <Link2 className="h-3.5 w-3.5" />
                          Copy claim link
                        </Button>
                      ) : null}
                      {NOTIFYABLE.has(w.status) ? (
                        <Button
                          variant="outline"
                          size="sm"
                          disabled={busyId === w.id}
                          onClick={() => void notify(w)}
                        >
                          <Bell className="h-3.5 w-3.5" />
                          Notify
                        </Button>
                      ) : null}
                      {EXPIRABLE.has(w.status) ? (
                        <Button
                          variant="outline"
                          size="sm"
                          className="text-destructive"
                          disabled={busyId === w.id}
                          onClick={() => setExpireTarget(w)}
                        >
                          <TimerOff className="h-3.5 w-3.5" />
                          Expire
                        </Button>
                      ) : null}
                      {!LINKABLE.has(w.status) ? (
                        <span className="text-xs text-muted-foreground">—</span>
                      ) : null}
                    </div>
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </Card>
      <p className="mt-3 text-xs text-muted-foreground">
        A background sweep marks entries expired about every 5 minutes once
        their window passes; claims on an expired window are refused (410).
        Claimed entries are linked to the resulting booking.
      </p>

      <ConfirmDialog
        open={expireTarget !== null}
        onOpenChange={(open) => {
          if (!open && busyId === null) setExpireTarget(null);
        }}
        title="Expire waitlist entry?"
        description={
          expireTarget
            ? `${
                expireTarget.contact_name || expireTarget.contact_phone
              } will no longer be able to claim this window — the entry is marked expired and its claim link stops working.`
            : undefined
        }
        confirmLabel="Expire entry"
        destructive
        busy={expireTarget !== null && busyId === expireTarget.id}
        onConfirm={() => void expire()}
      />
    </div>
  );
}
