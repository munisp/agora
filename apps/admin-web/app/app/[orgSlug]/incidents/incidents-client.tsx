"use client";

import * as React from "react";
import { ChevronDown, ChevronRight, CircleCheck, Plus, RefreshCw, Send, Trash2 } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { PageHeader } from "@/components/page-header";
import { ErrorNote } from "@/components/error-note";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  ConfirmDialog,
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input, Label, Select, Textarea } from "@/components/ui/input";
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
import { formatDateTime, titleCase } from "@/lib/utils";
import type {
  Incident,
  IncidentDelivery,
  IncidentDispatchEndpoint,
} from "@/lib/types";

type StatusFilter = "" | "new" | "dispatched" | "acknowledged" | "resolved" | "closed";

/** Statuses from which POST /v1/incidents/{id}/resolve is accepted (409 otherwise). */
const RESOLVABLE: ReadonlySet<string> = new Set([
  "new",
  "dispatched",
  "acknowledged",
]);

function statusVariant(
  status: string,
): "success" | "warning" | "destructive" | "secondary" {
  switch (status) {
    case "new":
      return "destructive";
    case "dispatched":
      return "warning";
    case "acknowledged":
      return "secondary";
    case "resolved":
    case "closed":
      return "success";
    default:
      return "secondary";
  }
}

function severityVariant(
  severity: string,
): "success" | "warning" | "destructive" | "secondary" {
  switch (severity) {
    case "critical":
    case "high":
      return "destructive";
    case "medium":
      return "warning";
    case "low":
      return "success";
    default:
      return "secondary";
  }
}

export function IncidentsClient({
  orgSlug,
  initialIncidents,
  initialEndpoints,
  initialError,
}: {
  orgSlug: string;
  initialIncidents: Incident[];
  initialEndpoints: IncidentDispatchEndpoint[];
  initialError: string | null;
}) {
  const { toast } = useToast();
  const [incidents, setIncidents] = React.useState<Incident[]>(initialIncidents);
  const [endpoints, setEndpoints] =
    React.useState<IncidentDispatchEndpoint[]>(initialEndpoints);
  const [error, setError] = React.useState<string | null>(initialError);
  const [loading, setLoading] = React.useState(false);
  const [status, setStatus] = React.useState<StatusFilter>("");
  const [openId, setOpenId] = React.useState<string | null>(null);
  const [epUrl, setEpUrl] = React.useState("");
  const [epSecret, setEpSecret] = React.useState("");
  const [epBusy, setEpBusy] = React.useState(false);
  const [deleteEp, setDeleteEp] =
    React.useState<IncidentDispatchEndpoint | null>(null);
  const [resolveTarget, setResolveTarget] = React.useState<Incident | null>(
    null,
  );
  const [resolveNote, setResolveNote] = React.useState("");
  const [resolving, setResolving] = React.useState(false);

  const load = React.useCallback(
    async (f: StatusFilter) => {
      setLoading(true);
      setError(null);
      try {
        const data = await api.get<{ incidents?: Incident[] }>(
          "/api/bookings/v1/incidents",
          { tenant: orgSlug, status: f || undefined },
        );
        setIncidents(data.incidents ?? []);
      } catch (e) {
        setError(
          e instanceof ApiError ? e.message : "Failed to load incidents.",
        );
      } finally {
        setLoading(false);
      }
    },
    [orgSlug],
  );

  const loadEndpoints = React.useCallback(async () => {
    try {
      const data = await api.get<{ endpoints?: IncidentDispatchEndpoint[] }>(
        "/api/bookings/v1/incidents/dispatch-endpoints",
        { tenant: orgSlug },
      );
      setEndpoints(data.endpoints ?? []);
    } catch {
      /* endpoints section is supplementary — the list error stays primary */
    }
  }, [orgSlug]);

  const skipInitialLoad = React.useRef(true);
  React.useEffect(() => {
    if (skipInitialLoad.current) {
      skipInitialLoad.current = false;
      return;
    }
    void load(status);
  }, [status, load]);

  const dispatch = async (incident: Incident) => {
    try {
      const res = await api.post<{ deliveries?: IncidentDelivery[] }>(
        `/api/bookings/v1/incidents/${incident.id}/dispatch`,
        {},
        { tenant: orgSlug },
      );
      toast({
        title: "Incident dispatched",
        description: `Delivered to ${res.deliveries?.length ?? 0} endpoint(s).`,
        variant: "success",
      });
      await load(status);
    } catch (e) {
      toast({
        title: "Dispatch failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    }
  };

  const resolveIncident = async () => {
    if (!resolveTarget) return;
    setResolving(true);
    try {
      const note = resolveNote.trim();
      await api.post<{ incident?: Incident }>(
        `/api/bookings/v1/incidents/${resolveTarget.id}/resolve`,
        { note: note || undefined },
        { tenant: orgSlug },
      );
      toast({
        title: "Incident resolved",
        description: `${
          resolveTarget.reference_number || resolveTarget.id
        } marked resolved.`,
        variant: "success",
      });
      setResolveTarget(null);
      setResolveNote("");
      await load(status);
    } catch (e) {
      toast({
        title: "Resolve failed",
        description: e instanceof ApiError ? e.message : undefined,
        variant: "destructive",
      });
    } finally {
      setResolving(false);
    }
  };

  const addEndpoint = async (e: React.FormEvent) => {
    e.preventDefault();
    const url = epUrl.trim();
    if (!url) return;
    setEpBusy(true);
    try {
      await api.post(
        "/api/bookings/v1/incidents/dispatch-endpoints",
        { url, secret: epSecret.trim() || undefined },
        { tenant: orgSlug },
      );
      toast({ title: "Dispatch endpoint saved", variant: "success" });
      setEpUrl("");
      setEpSecret("");
      await loadEndpoints();
    } catch (err) {
      toast({
        title: "Could not save the endpoint",
        description: err instanceof ApiError ? err.message : undefined,
        variant: "destructive",
      });
    } finally {
      setEpBusy(false);
    }
  };

  const removeEndpoint = async () => {
    if (!deleteEp) return;
    setEpBusy(true);
    try {
      await api.delete("/api/bookings/v1/incidents/dispatch-endpoints", {
        tenant: orgSlug,
        url: deleteEp.url,
      });
      toast({ title: "Dispatch endpoint removed", variant: "success" });
      setDeleteEp(null);
      await loadEndpoints();
    } catch (err) {
      toast({
        title: "Could not remove the endpoint",
        description: err instanceof ApiError ? err.message : undefined,
        variant: "destructive",
      });
    } finally {
      setEpBusy(false);
    }
  };

  return (
    <div>
      <PageHeader
        title="Incidents"
        description="Device and civic incidents ingested from IoT/webhook sources. Dispatch signs and delivers the incident payload to every active endpoint; acknowledgement and closure come back from the endpoints' callbacks. Staff can also resolve an incident manually once the underlying issue is handled."
        actions={
          <Button
            variant="outline"
            size="sm"
            onClick={() => void load(status)}
            disabled={loading}
          >
            <RefreshCw className={loading ? "h-3.5 w-3.5 animate-spin" : "h-3.5 w-3.5"} />
            Refresh
          </Button>
        }
      />
      {error ? <ErrorNote message={error} /> : null}

      <div className="mb-3 flex items-center gap-2">
        <Label htmlFor="inc-status" className="text-xs">
          Status
        </Label>
        <Select
          id="inc-status"
          value={status}
          onChange={(e) => setStatus(e.target.value as StatusFilter)}
          className="w-44"
        >
          <option value="">All</option>
          <option value="new">New</option>
          <option value="dispatched">Dispatched</option>
          <option value="acknowledged">Acknowledged</option>
          <option value="resolved">Resolved</option>
          <option value="closed">Closed</option>
        </Select>
      </div>

      <Card>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-8" />
              <TableHead>Reference</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Severity</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Reported</TableHead>
              <TableHead className="pr-5 text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {incidents.length === 0 ? (
              <TableEmpty colSpan={7}>
                {loading
                  ? "Loading…"
                  : "No incidents — ingested device and civic reports appear here."}
              </TableEmpty>
            ) : (
              incidents.map((inc) => (
                <IncidentRow
                  key={inc.id}
                  orgSlug={orgSlug}
                  incident={inc}
                  open={openId === inc.id}
                  onToggle={() =>
                    setOpenId((id) => (id === inc.id ? null : inc.id))
                  }
                  onDispatch={() => void dispatch(inc)}
                  onResolve={() => {
                    setResolveNote("");
                    setResolveTarget(inc);
                  }}
                />
              ))
            )}
          </TableBody>
        </Table>
      </Card>

      <Card className="mt-6">
        <CardHeader>
          <CardTitle>Dispatch endpoints</CardTitle>
        </CardHeader>
        <CardContent>
          {endpoints.length > 0 ? (
            <ul className="mb-4 space-y-2">
              {endpoints.map((ep) => (
                <li
                  key={ep.url}
                  className="flex items-center justify-between gap-3 rounded-md border border-border px-3 py-2 text-sm"
                >
                  <span className="min-w-0 truncate font-mono text-xs">
                    {ep.url}
                  </span>
                  <span className="flex items-center gap-2">
                    <Badge variant={ep.active ? "success" : "secondary"}>
                      {ep.active ? "active" : "inactive"}
                    </Badge>
                    <Button
                      variant="ghost"
                      size="sm"
                      className="text-destructive"
                      onClick={() => setDeleteEp(ep)}
                    >
                      <Trash2 className="h-3.5 w-3.5" />
                    </Button>
                  </span>
                </li>
              ))}
            </ul>
          ) : (
            <p className="mb-4 text-sm text-muted-foreground">
              No dispatch endpoints configured — dispatched incidents go
              nowhere until one is added.
            </p>
          )}
          <form onSubmit={addEndpoint} className="flex flex-wrap items-end gap-3">
            <div className="grid min-w-64 flex-1 gap-1.5">
              <Label htmlFor="ep-url">Endpoint URL</Label>
              <Input
                id="ep-url"
                type="url"
                placeholder="https://ops.example.com/incidents"
                value={epUrl}
                onChange={(e) => setEpUrl(e.target.value)}
              />
            </div>
            <div className="grid min-w-48 flex-1 gap-1.5">
              <Label htmlFor="ep-secret">Signing secret</Label>
              <Input
                id="ep-secret"
                type="password"
                value={epSecret}
                onChange={(e) => setEpSecret(e.target.value)}
                placeholder="Used to sign deliveries"
              />
            </div>
            <Button type="submit" variant="secondary" disabled={epBusy || !epUrl.trim()}>
              <Plus className="h-4 w-4" />
              {epBusy ? "Saving…" : "Add endpoint"}
            </Button>
          </form>
        </CardContent>
      </Card>

      <Dialog
        open={resolveTarget !== null}
        onOpenChange={(open) => {
          if (!open && !resolving) setResolveTarget(null);
        }}
      >
        <DialogContent onClose={() => !resolving && setResolveTarget(null)}>
          <DialogHeader>
            <DialogTitle>Resolve incident</DialogTitle>
            <DialogDescription>
              Mark{" "}
              {resolveTarget
                ? resolveTarget.reference_number || resolveTarget.id
                : ""}{" "}
              as resolved. The note below is optional and is kept with the
              incident&apos;s audit trail.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-1.5">
            <Label htmlFor="resolve-note">Resolution note (optional)</Label>
            <Textarea
              id="resolve-note"
              value={resolveNote}
              onChange={(e) => setResolveNote(e.target.value)}
              placeholder="What fixed it?"
            />
          </div>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setResolveTarget(null)}
              disabled={resolving}
            >
              Cancel
            </Button>
            <Button onClick={() => void resolveIncident()} disabled={resolving}>
              {resolving ? "Resolving…" : "Resolve incident"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <ConfirmDialog
        open={deleteEp !== null}
        onOpenChange={(open) => !open && setDeleteEp(null)}
        title="Remove dispatch endpoint?"
        description={
          deleteEp
            ? `${deleteEp.url} will stop receiving incident deliveries.`
            : undefined
        }
        confirmLabel="Remove endpoint"
        destructive
        busy={epBusy}
        onConfirm={() => void removeEndpoint()}
      />
    </div>
  );
}

function IncidentRow({
  orgSlug,
  incident,
  open,
  onToggle,
  onDispatch,
  onResolve,
}: {
  orgSlug: string;
  incident: Incident;
  open: boolean;
  onToggle: () => void;
  onDispatch: () => void;
  onResolve: () => void;
}) {
  const [deliveries, setDeliveries] = React.useState<IncidentDelivery[] | null>(
    null,
  );

  React.useEffect(() => {
    if (!open) return;
    let cancelled = false;
    (async () => {
      try {
        const d = await api.get<{ deliveries?: IncidentDelivery[] }>(
          `/api/bookings/v1/incidents/${incident.id}`,
          { tenant: orgSlug },
        );
        if (!cancelled) setDeliveries(d.deliveries ?? []);
      } catch {
        if (!cancelled) setDeliveries([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [open, incident.id, orgSlug]);

  return (
    <>
      <TableRow className="cursor-pointer" onClick={onToggle} aria-expanded={open}>
        <TableCell className="w-8">
          {open ? (
            <ChevronDown className="h-4 w-4 text-muted-foreground" />
          ) : (
            <ChevronRight className="h-4 w-4 text-muted-foreground" />
          )}
        </TableCell>
        <TableCell className="font-mono text-xs font-medium">
          {incident.reference_number || incident.id}
        </TableCell>
        <TableCell>{titleCase(incident.incident_type)}</TableCell>
        <TableCell>
          <Badge variant={severityVariant(incident.severity)}>
            {incident.severity}
          </Badge>
        </TableCell>
        <TableCell>
          <Badge variant={statusVariant(incident.status)}>
            {incident.status}
          </Badge>
        </TableCell>
        <TableCell className="text-xs text-muted-foreground">
          {formatDateTime(incident.created_at)}
        </TableCell>
        <TableCell className="pr-5 text-right">
          <div className="flex items-center justify-end gap-2">
            {incident.status === "new" ? (
              <Button
                variant="outline"
                size="sm"
                onClick={(e) => {
                  e.stopPropagation();
                  onDispatch();
                }}
              >
                <Send className="h-3.5 w-3.5" />
                Dispatch
              </Button>
            ) : null}
            <Button
              variant="outline"
              size="sm"
              disabled={!RESOLVABLE.has(incident.status)}
              onClick={(e) => {
                e.stopPropagation();
                onResolve();
              }}
            >
              <CircleCheck className="h-3.5 w-3.5" />
              Resolve
            </Button>
          </div>
        </TableCell>
      </TableRow>
      {open ? (
        <TableRow>
          <TableCell colSpan={7} className="bg-muted/30">
            <div className="grid gap-4 px-2 py-3 md:grid-cols-2">
              <div>
                <p className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                  Payload
                </p>
                <pre className="max-h-64 overflow-auto rounded-md border border-border bg-card p-3 text-xs">
                  {JSON.stringify(incident.payload, null, 2)}
                </pre>
              </div>
              <div>
                <p className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                  Delivery ledger
                </p>
                {deliveries === null ? (
                  <p className="text-sm text-muted-foreground">Loading…</p>
                ) : deliveries.length === 0 ? (
                  <p className="text-sm text-muted-foreground">
                    Not dispatched yet.
                  </p>
                ) : (
                  <ul className="space-y-1.5 text-sm">
                    {deliveries.map((d) => (
                      <li
                        key={d.id}
                        className="flex items-center justify-between gap-3 rounded-md border border-border bg-card px-3 py-1.5"
                      >
                        <span className="min-w-0 truncate font-mono text-xs">
                          {d.endpoint_url}
                        </span>
                        <Badge
                          variant={
                            d.status === "delivered"
                              ? "success"
                              : d.status === "dlq"
                                ? "destructive"
                                : "warning"
                          }
                        >
                          {d.status}
                          {typeof d.last_status_code === "number" &&
                          d.last_status_code > 0
                            ? ` · ${d.last_status_code}`
                            : ""}
                        </Badge>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </div>
          </TableCell>
        </TableRow>
      ) : null}
    </>
  );
}
